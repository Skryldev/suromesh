package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/service/internal/config"
)

func env(kv ...string) []string {
	out := make([]string, 0, len(kv))
	for i := 0; i < len(kv); i += 2 {
		out = append(out, kv[i]+"="+kv[i+1])
	}
	return out
}

func TestLoad_Defaults(t *testing.T) {
	// Defaults alone deliberately do NOT validate: the operator must
	// choose TLS or explicit dev plaintext (fail closed).
	if _, err := config.Load(nil); err == nil {
		t.Fatal("defaults without a listener choice must fail validation")
	}
	cfg, err := config.Load(env(
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin",
		"SUROMESH_STORAGE_POSTGRES_DSN", "postgres://localhost/test"))
	if err != nil {
		t.Fatal(err)
	}
	d := config.Defaults()
	if cfg.Listen.Addr != d.Listen.Addr || cfg.Log.Level != d.Log.Level ||
		cfg.Accounts.MutationQueue != d.Accounts.MutationQueue ||
		cfg.Streams.Buffer != d.Streams.Buffer ||
		cfg.Shutdown.Timeout != d.Shutdown.Timeout {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	// Every documented default is justified; assert the load-bearing ones.
	if cfg.Listen.Addr != ":50051" || cfg.Accounts.MutationQueue != 64 ||
		cfg.Streams.Buffer != 1024 || cfg.Shutdown.Timeout != 20*time.Second {
		t.Fatalf("documented defaults drifted: %+v", cfg)
	}
}

func TestLoad_EnvOverridesDefaults(t *testing.T) {
	cfg, err := config.Load(env(
		"SUROMESH_LISTEN_ADDR", ":7000",
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin",
		"SUROMESH_STORAGE_POSTGRES_DSN", "postgres://localhost/test",
		"SUROMESH_LOG_LEVEL", "debug",
		"SUROMESH_ACCOUNTS_MUTATION_QUEUE", "128",
		"SUROMESH_STREAMS_BUFFER", "2048",
		"SUROMESH_SHUTDOWN_TIMEOUT", "45s",
		"SUROMESH_ACCOUNTS_AUTO_CONNECT", "true",
	))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen.Addr != ":7000" || cfg.Log.Level != "debug" ||
		cfg.Accounts.MutationQueue != 128 || cfg.Streams.Buffer != 2048 ||
		cfg.Shutdown.Timeout != 45*time.Second || !cfg.Accounts.AutoConnect {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
}

func TestLoad_KMSProviderFailsClosed(t *testing.T) {
	_, err := config.Load(env(
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_PROVIDER", "kms"))
	if err == nil || !strings.Contains(err.Error(), "owner decision") {
		t.Fatalf("kms provider err = %v, want owner-decision fail-closed", err)
	}
}

func TestLoad_StorageRequired(t *testing.T) {
	_, err := config.Load(env(
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin"))
	if err == nil || !strings.Contains(err.Error(), "storage.postgres.dsn is required") {
		t.Fatalf("missing storage err = %v", err)
	}
}

func TestLoad_BootstrapKeyValidation(t *testing.T) {
	if _, err := config.Load(env(
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin",
		"SUROMESH_STORAGE_POSTGRES_DSN", "postgres://localhost/test",
		"SUROMESH_AUTH_BOOTSTRAP_KEY", "short",
		"SUROMESH_AUTH_BOOTSTRAP_TENANT", "t")); err == nil {
		t.Fatal("short bootstrap key must fail")
	}
	cfg, err := config.Load(env(
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin",
		"SUROMESH_STORAGE_POSTGRES_DSN", "postgres://localhost/test",
		"SUROMESH_AUTH_BOOTSTRAP_KEY", "sk_validbootstrapkey123456",
		"SUROMESH_AUTH_BOOTSTRAP_TENANT", "bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.BootstrapTenant != "bootstrap" {
		t.Fatal("bootstrap tenant not applied")
	}
}

func TestLoad_UnknownKeysIgnored(t *testing.T) {
	cfg, err := config.Load(env(
		"SUROMESH_SOMETHING_ELSE", "1", "OTHER_VAR", "x",
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin",
		"SUROMESH_STORAGE_POSTGRES_DSN", "postgres://localhost/test"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen.Addr != ":50051" {
		t.Fatal("unknown keys must not perturb config")
	}
}

func TestValidate_TLSRules(t *testing.T) {
	cases := []struct {
		name    string
		env     []string
		wantErr string
	}{
		{"no tls and no dev flag", env("SUROMESH_LISTEN_ADDR", ":50051"), "TLS cert+key, or set listen.plaintext_dev"},
		{"half tls pair", env("SUROMESH_LISTEN_TLS_CERT", "/x/c.pem", "SUROMESH_LISTEN_PLAINTEXT_DEV", "true"), "BOTH cert and key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
	// Valid: full TLS pair without dev flag.
	cfg, err := config.Load(env(
		"SUROMESH_LISTEN_TLS_CERT", "/x/c.pem", "SUROMESH_LISTEN_TLS_KEY", "/x/k.pem",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin",
		"SUROMESH_STORAGE_POSTGRES_DSN", "postgres://localhost/test"))
	if err != nil {
		t.Fatalf("full TLS pair rejected: %v", err)
	}
	_ = cfg
	// Valid: dev plaintext.
	if _, err := config.Load(env(
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin",
		"SUROMESH_STORAGE_POSTGRES_DSN", "postgres://localhost/test")); err != nil {
		t.Fatalf("plaintext dev rejected: %v", err)
	}
}

func TestValidate_Ranges(t *testing.T) {
	// Every bad case needs the otherwise-valid S4 baseline so the specific
	// validation failure under test is the one that fires.
	base := []string{
		"SUROMESH_LISTEN_PLAINTEXT_DEV", "true",
		"SUROMESH_CRYPTO_FILE_KEY", "/keys/kek.bin",
		"SUROMESH_STORAGE_POSTGRES_DSN", "postgres://localhost/test",
	}
	bad := [][]string{
		{"SUROMESH_ACCOUNTS_MUTATION_QUEUE", "0"},
		{"SUROMESH_ACCOUNTS_MUTATION_QUEUE", "10001"},
		{"SUROMESH_STREAMS_BUFFER", "8"},
		{"SUROMESH_STREAMS_BUFFER", "2000000"},
		{"SUROMESH_SHUTDOWN_TIMEOUT", "50ms"},
		{"SUROMESH_SHUTDOWN_TIMEOUT", "5m"},
		{"SUROMESH_LOG_LEVEL", "loud"},
		{"SUROMESH_LOG_FORMAT", "xml"},
		{"SUROMESH_LISTEN_ADDR", "no-port"},
	}
	for _, kv := range bad {
		if _, err := config.Load(env(append(base, kv...)...)); err == nil {
			t.Fatalf("%s=%s must fail validation", kv[0], kv[1])
		}
	}
}

func TestConfig_LogValueShapeOnly(t *testing.T) {
	cfg := config.Defaults()
	cfg.Listen.TLSCertFile = "/etc/certs/c.pem"
	cfg.Listen.TLSKeyFile = "/etc/certs/k.pem"
	cfg.Storage.Dsn = "postgres://localhost/secret-dsn"
	cfg.Auth.BootstrapKey = "sk_bootstrap_secret_value"
	v := cfg.LogValue()
	// The log value emits shape only: TLS is a boolean; secrets (DSN,
	// bootstrap key) and paths NEVER appear.
	text := v.String()
	if strings.Contains(text, "/etc/certs") || strings.Contains(text, ".pem") ||
		strings.Contains(text, "secret-dsn") || strings.Contains(text, "sk_bootstrap") {
		t.Fatalf("secret/path material in log value: %s", text)
	}
	if !strings.Contains(text, "listen_tls=true") {
		t.Fatalf("tls boolean missing: %s", text)
	}
	if strings.Contains(strings.ToLower(text), "password") ||
		strings.Contains(text, "BEGIN") {
		t.Fatalf("secret-like material in log value: %s", text)
	}
}
