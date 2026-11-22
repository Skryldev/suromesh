// Command service is the SuroMesh Service entry point: configuration →
// logging → storage (PostgreSQL, migrations) → crypto (KEK provider) →
// upstream factory → runtime (manager + services + gRPC server) →
// signals. Every step fails closed; shutdown is ordered and bounded.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Skryldev/suromesh/service/internal/app"
	"github.com/Skryldev/suromesh/service/internal/config"
	"github.com/Skryldev/suromesh/service/internal/platform/logging"
)

func main() {
	os.Exit(run())
}

func run() int {
	// 1. Configuration (env over defaults; validated; secrets never logged).
	cfg, err := config.Load(os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}

	// 2. Logging (redaction registry seeded with the runtime secrets — the
	//    DSN and bootstrap key are scrubbed from every record).
	red := logging.NewRedactor(map[string]string{
		cfg.Storage.Dsn:       "dsn",
		cfg.Auth.BootstrapKey: "apikey",
	})
	logger, err := logging.New(cfg.Log, os.Stdout, red)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logging error:", err)
		return 2
	}

	// 3. OS signals → graceful shutdown request.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// 4. Resources in dependency order (started sequentially; a startup
	//    failure stops the already-started prefix in reverse).
	storage := newStorageResource(cfg, logger)
	cryptoRes := newCryptoResource(cfg, logger)
	gatewayRes := newGatewayResource()
	runtimeRes := newRuntimeResource(cfg, logger, storage, cryptoRes, gatewayRes)

	application := app.New(cfg, logger, storage, cryptoRes, gatewayRes, runtimeRes)
	if err := application.Run(ctx); err != nil {
		logger.Error("service terminated with error", slog.String("error", err.Error()))
		return 1
	}
	logger.Info("service terminated cleanly")
	return 0
}
