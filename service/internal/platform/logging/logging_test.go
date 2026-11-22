package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Skryldev/suromesh/service/internal/config"
	"github.com/Skryldev/suromesh/service/internal/platform/logging"
)

func TestNew_JSONFieldsAndLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New(config.LogConfig{Level: "debug", Format: "json"}, &buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello", slog.String("component", "test"), slog.Int("n", 1))
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["msg"] != "hello" || rec["component"] != "test" {
		t.Fatalf("fields missing: %v", rec)
	}
	if _, ok := rec["n"]; !ok {
		t.Fatalf("int field missing: %v", rec)
	}
}

func TestNew_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New(config.LogConfig{Level: "warn", Format: "text"}, &buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("dropped")
	if buf.Len() != 0 {
		t.Fatalf("info emitted under warn level: %q", buf.String())
	}
	logger.Warn("kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Fatal("warn not emitted")
	}
}

func TestNew_InvalidLevel(t *testing.T) {
	if _, err := logging.New(config.LogConfig{Level: "loud", Format: "json"}, nil, nil); err == nil {
		t.Fatal("expected error for unknown level")
	}
}

func TestRequestID_AttachedFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := logging.New(config.LogConfig{Level: "info", Format: "json"}, &buf, nil)
	ctx := logging.WithRequestID(context.Background(), "req-123")
	logger.InfoContext(ctx, "with correlation")
	var rec map[string]any
	_ = json.Unmarshal(buf.Bytes(), &rec)
	if rec["request_id"] != "req-123" {
		t.Fatalf("request_id missing: %v", rec)
	}
}

func TestRedactor_ScrubsMessageAndAttributes(t *testing.T) {
	var buf bytes.Buffer
	secrets := logging.NewRedactor(map[string]string{
		"supersecretvalue": "otp",
		"+15550001111":     "phone",
	})
	logger, _ := logging.New(config.LogConfig{Level: "info", Format: "json"}, &buf, secrets)

	// Simulate a runtime-discovered secret (e.g. session key fingerprint
	// material that must never appear).
	secrets.Register("runtime-key-material-xyz", "session")

	logger.Info("submitting supersecretvalue now",
		slog.String("phone", "+15550001111"),
		slog.String("blob", "runtime-key-material-xyz"),
		slog.Group("nested", slog.String("inner", "supersecretvalue")),
	)

	out := buf.String()
	if strings.Contains(out, "supersecretvalue") || strings.Contains(out, "+15550001111") ||
		strings.Contains(out, "runtime-key-material-xyz") {
		t.Fatalf("secret leaked into log output: %s", out)
	}
	if !strings.Contains(out, "<otp>") || !strings.Contains(out, "<phone>") ||
		!strings.Contains(out, "<session>") {
		t.Fatalf("placeholders missing: %s", out)
	}
}

func TestRedactor_ShortValuesNotRegistered(t *testing.T) {
	r := logging.NewRedactor(map[string]string{"ab": "x"})
	// "ab" is too short to register; scrub must be a no-op (no corruption).
	if got := r.Scrub("abc ab"); got != "abc ab" {
		t.Fatalf("unexpected scrub: %q", got)
	}
}
