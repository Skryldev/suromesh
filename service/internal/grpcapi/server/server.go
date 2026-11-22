// Package server assembles the gRPC server: service registrations,
// interceptor chain (recovery → authn/authz → deadline), health service,
// and reflection policy (disabled by default; dev-only override).
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/grpcapi/handlers"
	"github.com/Skryldev/suromesh/service/internal/grpcapi/interceptors"
)

// Config shapes the listener.
type Config struct {
	Addr           string
	TLSCertFile    string
	TLSKeyFile     string
	Reflection     bool // dev-only; production leaves false
	UnaryTimeout   time.Duration
	MaxRecvMsgSize int // bytes; 0 = gRPC default (4 MiB)
	MaxSendMsgSize int // bytes; raised for media chunks
}

// Deps bundles the handler dependencies.
type Deps struct {
	Manager     *application.Manager
	Accounts    *application.AccountService
	Auth        *application.AuthService
	Sessions    handlers.SessionOps
	AuthN       *interceptors.CredentialAuthenticator
	Logger      *slog.Logger
	Permissions map[string]string // method-prefix → required permission
}

// Server is the assembled gRPC server (owned lifecycle).
type Server struct {
	grpc *grpc.Server
	addr string
}

// New assembles the server with the full interceptor chain and service
// registrations.
func New(cfg Config, deps Deps) (*Server, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Interceptor chain (unary): recovery → authn → per-RPC permission →
	// deadline.
	permUnary := interceptors.PermissionUnary(deps.AuthN, deps.Permissions)
	permStream := interceptors.PermissionStream(deps.AuthN, deps.Permissions)
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			interceptors.Recovery(loggerAdapter{logger}),
			permUnary,
			interceptors.TimeoutInterceptor(cfg.UnaryTimeout),
		),
		grpc.ChainStreamInterceptor(
			interceptors.RecoveryStream(loggerAdapter{logger}),
			permStream,
		),
	}
	if cfg.MaxRecvMsgSize > 0 {
		opts = append(opts, grpc.MaxRecvMsgSize(cfg.MaxRecvMsgSize))
	}
	if cfg.MaxSendMsgSize > 0 {
		opts = append(opts, grpc.MaxSendMsgSize(cfg.MaxSendMsgSize))
	}

	gs := grpc.NewServer(opts...)
	accountHandlers := handlers.NewAccountHandlers(deps.Accounts, deps.Auth, deps.Sessions)
	v1.RegisterAccountServiceServer(gs, accountHandlers)
	v1.RegisterSessionServiceServer(gs, accountHandlers)
	v1.RegisterAuthenticationServiceServer(gs, accountHandlers)

	runtimeHandlers := handlers.NewRuntimeHandlers(deps.Manager, deps.Accounts)
	v1.RegisterUserServiceServer(gs, runtimeHandlers)
	v1.RegisterDialogServiceServer(gs, runtimeHandlers)
	v1.RegisterConnectionServiceServer(gs, runtimeHandlers)
	v1.RegisterMessagingServiceServer(gs, runtimeHandlers)
	v1.RegisterUpdateServiceServer(gs, runtimeHandlers)
	v1.RegisterMediaServiceServer(gs, runtimeHandlers)

	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, healthSrv)

	if cfg.Reflection {
		reflection.Register(gs)
		logger.Warn("gRPC reflection ENABLED (development only)")
	}

	return &Server{grpc: gs, addr: cfg.Addr}, nil
}

// Serve blocks serving on the configured address (liveness phase).
// Returns the accept error after Stop.
func (s *Server) Serve(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	return s.ServeOn(ctx, lis)
}

// ServeOn serves on a pre-created listener (test seam).
func (s *Server) ServeOn(ctx context.Context, lis net.Listener) error {
	errC := make(chan error, 1)
	go func() { errC <- s.grpc.Serve(lis) }()
	select {
	case err := <-errC:
		return err
	case <-ctx.Done():
		graceful, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopped := make(chan struct{})
		go func() { s.grpc.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
			return nil
		case <-graceful.Done():
			s.grpc.Stop() // hard stop past the graceful budget
			return nil
		}
	}
}

// loggerAdapter adapts slog.Logger to the interceptor logger interface.
type loggerAdapter struct{ l *slog.Logger }

func (a loggerAdapter) Error(msg string, args ...any) { a.l.Error(msg, args...) }
