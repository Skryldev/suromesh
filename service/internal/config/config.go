// Package config implements the SuroMesh Service typed configuration
// model: environment-variable sources over explicit defaults, eager
// validation, and fail-closed handling of keys reserved for future
// phases (S4: storage DSN, KEK).
//
// Precedence: explicit defaults < environment variables. A configuration
// file source is deliberately deferred (S2) until a parser dependency is
// justified by real configuration volume; env-only keeps S1 dependency
// free. Secrets are never logged: the String/LogValue representations of
// this package emit shape only.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

// EnvKeyPrefix is the namespace for all service environment variables.
const EnvKeyPrefix = "SUROMESH_"

// Reserved future-phase environment variables. Setting them is rejected
// at validation so a configured-but-unimplemented feature can never be
// silently ignored (fail closed). S4's storage/crypto keys are now
// implemented and live in the config groups below; this mechanism remains
// for future phases (e.g. S7 leases, S10 admin RPCs).
var reservedKeys = map[string]string{}

// ListenConfig configures the gRPC listener (live since S5).
type ListenConfig struct {
	Addr          string // host:port for the gRPC listener
	TLSCertFile   string // PEM certificate; required unless PlaintextDev
	TLSKeyFile    string // PEM key; required unless PlaintextDev
	PlaintextDev  bool   // explicit dev-only plaintext listener
	ReflectionDev bool   // dev-only gRPC reflection
}

// StorageConfig configures PostgreSQL (S4).
type StorageConfig struct {
	Dsn string // postgres DSN (secret: env/secret store only)
}

// CryptoConfig selects the session-encryption key source (ADR-0008).
// provider: "file" (dev/self-hosted) | "kms" (production; not yet
// implemented — fails closed with an owner-decision message).
type CryptoConfig struct {
	Provider    string // file|kms
	FileKeyPath string // file provider: 32-byte KEK file
}

// AuthConfig carries the bootstrap credential (dev path): a raw API key
// with full permissions, registered before storage-backed credentials
// exist. Production deployments manage keys via storage instead.
type AuthConfig struct {
	BootstrapKey    string // raw sk_... key (secret: env only)
	BootstrapTenant string // tenant name for the bootstrap credential
}

// LogConfig selects logging behavior (see internal/platform/logging).
type LogConfig struct {
	Level  string // debug|info|warn|error
	Format string // json|text
}

// AccountsConfig bounds per-account runtime behavior (enforced by the
// AccountRunner from S5).
type AccountsConfig struct {
	AutoConnect   bool // connect bound accounts at startup
	MutationQueue int  // bounded per-account mutation queue depth
}

// StreamsConfig bounds update-stream behavior (S7).
type StreamsConfig struct {
	Buffer int // per-stream event buffer
}

// ShutdownConfig bounds termination.
type ShutdownConfig struct {
	Timeout time.Duration // overall graceful-shutdown budget
}

// Config is the root typed configuration.
type Config struct {
	Listen         ListenConfig
	Log            LogConfig
	Accounts       AccountsConfig
	Streams        StreamsConfig
	Shutdown       ShutdownConfig
	Storage        StorageConfig
	Crypto         CryptoConfig
	Auth           AuthConfig
	RequestTimeout time.Duration
}

// LogValue implements slog.LogValuer: emits shape only, never secret
// material (this configuration contains no secrets by design — file
// paths and sizes only — and the redaction layer double-checks).
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen_addr", c.Listen.Addr),
		slog.Bool("listen_tls", c.Listen.TLSCertFile != ""),
		slog.Bool("plaintext_dev", c.Listen.PlaintextDev),
		slog.Bool("reflection_dev", c.Listen.ReflectionDev),
		slog.String("log_level", c.Log.Level),
		slog.String("log_format", c.Log.Format),
		slog.Bool("auto_connect", c.Accounts.AutoConnect),
		slog.Int("mutation_queue", c.Accounts.MutationQueue),
		slog.Int("stream_buffer", c.Streams.Buffer),
		slog.String("shutdown", c.Shutdown.Timeout.String()),
		slog.Bool("storage", c.Storage.Dsn != ""),
		slog.String("crypto_provider", c.Crypto.Provider),
		// Secrets (DSN, bootstrap key, KEK path) are NEVER logged.
	)
}

// RequestTimeoutOrDefault returns the configured RPC default timeout
// (30s when unset; never negative).
func (c Config) RequestTimeoutOrDefault() time.Duration {
	if c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return 30 * time.Second
}

// Defaults returns the explicit-default configuration. Every default is
// justified in docs/configuration.md.
func Defaults() Config {
	return Config{
		Listen:   ListenConfig{Addr: ":50051"},
		Log:      LogConfig{Level: "info", Format: "json"},
		Accounts: AccountsConfig{AutoConnect: false, MutationQueue: 64},
		Streams:  StreamsConfig{Buffer: 1024},
		Shutdown: ShutdownConfig{Timeout: 20 * time.Second},
		Crypto:   CryptoConfig{Provider: "file"},
	}
}

// Load builds the configuration from the environment over Defaults and
// validates it. environ is the os.Environ-style slice (injectable for
// tests).
func Load(environ []string) (Config, error) {
	cfg := Defaults()
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(key, EnvKeyPrefix) {
			continue
		}
		if reason, reserved := reservedKeys[key]; reserved {
			return Config{}, fmt.Errorf("config: %s is reserved: %s", key, reason)
		}
		if err := apply(&cfg, strings.TrimPrefix(key, EnvKeyPrefix), value); err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", key, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func apply(cfg *Config, key, value string) error {
	switch key {
	case "LISTEN_ADDR":
		cfg.Listen.Addr = value
	case "LISTEN_TLS_CERT":
		cfg.Listen.TLSCertFile = value
	case "LISTEN_TLS_KEY":
		cfg.Listen.TLSKeyFile = value
	case "LISTEN_PLAINTEXT_DEV":
		b, err := parseBool(value)
		if err != nil {
			return err
		}
		cfg.Listen.PlaintextDev = b
	case "LOG_LEVEL":
		cfg.Log.Level = strings.ToLower(value)
	case "LOG_FORMAT":
		cfg.Log.Format = strings.ToLower(value)
	case "ACCOUNTS_AUTO_CONNECT":
		b, err := parseBool(value)
		if err != nil {
			return err
		}
		cfg.Accounts.AutoConnect = b
	case "ACCOUNTS_MUTATION_QUEUE":
		n, err := parseInt(value)
		if err != nil {
			return err
		}
		cfg.Accounts.MutationQueue = n
	case "STREAMS_BUFFER":
		n, err := parseInt(value)
		if err != nil {
			return err
		}
		cfg.Streams.Buffer = n
	case "SHUTDOWN_TIMEOUT":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", value, err)
		}
		cfg.Shutdown.Timeout = d
	case "STORAGE_POSTGRES_DSN":
		cfg.Storage.Dsn = value
	case "CRYPTO_PROVIDER":
		cfg.Crypto.Provider = strings.ToLower(value)
	case "CRYPTO_FILE_KEY":
		cfg.Crypto.FileKeyPath = value
	case "AUTH_BOOTSTRAP_KEY":
		cfg.Auth.BootstrapKey = value
	case "AUTH_BOOTSTRAP_TENANT":
		cfg.Auth.BootstrapTenant = value
	case "REQUEST_TIMEOUT":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", value, err)
		}
		cfg.RequestTimeout = d
	default:
		// Unknown service keys are ignored (standard env practice: the
		// environment is shared). Reserved keys above are the exception
		// because they name future features of THIS service.
	}
	return nil
}

// Validate enforces every documented constraint. Invalid configuration
// fails startup before any resource is created.
func (c Config) Validate() error {
	if c.Listen.Addr == "" {
		return errors.New("listen.addr must not be empty")
	}
	if _, _, err := net.SplitHostPort(c.Listen.Addr); err != nil {
		return fmt.Errorf("listen.addr %q is not host:port: %w", c.Listen.Addr, err)
	}
	tlsPairSet := c.Listen.TLSCertFile != "" && c.Listen.TLSKeyFile != ""
	tlsHalfSet := c.Listen.TLSCertFile != "" || c.Listen.TLSKeyFile != ""
	if tlsHalfSet && !tlsPairSet {
		return errors.New("listen.tls requires BOTH cert and key files")
	}
	if !tlsPairSet && !c.Listen.PlaintextDev {
		return errors.New("listen: provide TLS cert+key, or set listen.plaintext_dev=true (development only)")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q: want debug|info|warn|error", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		return fmt.Errorf("log.format %q: want json|text", c.Log.Format)
	}
	if c.Accounts.MutationQueue < 1 || c.Accounts.MutationQueue > 10000 {
		return fmt.Errorf("accounts.mutation_queue %d: want 1..10000", c.Accounts.MutationQueue)
	}
	if c.Streams.Buffer < 16 || c.Streams.Buffer > 1_000_000 {
		return fmt.Errorf("streams.buffer %d: want 16..1000000", c.Streams.Buffer)
	}
	if c.Shutdown.Timeout < time.Second || c.Shutdown.Timeout > 2*time.Minute {
		return fmt.Errorf("shutdown.timeout %s: want 1s..2m", c.Shutdown.Timeout)
	}
	switch c.Crypto.Provider {
	case "file":
		if c.Crypto.FileKeyPath == "" {
			return errors.New("crypto.file_key required when provider=file")
		}
	case "kms":
		return errors.New("crypto.provider=kms requires the KMS provider implementation " +
			"(owner decision pending) — use provider=file for development only")
	default:
		return fmt.Errorf("crypto.provider %q: want file|kms", c.Crypto.Provider)
	}
	// Production fail-closed rule: sessions need a key provider; storage is
	// required whenever accounts will persist (it always is in S4+).
	if c.Storage.Dsn == "" {
		return errors.New("storage.postgres.dsn is required (S4): " +
			"the service cannot run without durable metadata + encrypted sessions")
	}
	if c.Auth.BootstrapKey != "" {
		if !strings.HasPrefix(c.Auth.BootstrapKey, "sk_") || len(c.Auth.BootstrapKey) < 16 {
			return errors.New("auth.bootstrap_key must be an sk_ key (>= 16 chars)")
		}
		if strings.TrimSpace(c.Auth.BootstrapTenant) == "" {
			return errors.New("auth.bootstrap_tenant is required with auth.bootstrap_key")
		}
	}
	return nil
}

func parseBool(v string) (bool, error) {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid bool %q", v)
	}
	return b, nil
}

func parseInt(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid int %q", v)
	}
	return n, nil
}
