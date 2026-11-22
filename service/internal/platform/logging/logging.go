// Package logging configures the service's structured logging on top of
// log/slog (stdlib — no dependency justified at this scale): levels,
// JSON/text formats, component attribution, per-request correlation IDs,
// and a redaction handler that scrubs registered secret values from
// every emitted record.
//
// The service never logs phone numbers, OTPs, session material, or
// authorization keys; redaction is the second line of defense after the
// design rule "no secret enters a log call".
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/Skryldev/suromesh/service/internal/config"
)

type ctxKey int

const requestIDKey ctxKey = 1

// WithRequestID returns a context carrying the correlation ID; the
// handler attaches it to every record emitted through that context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID extracts the correlation ID, if any.
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok && id != ""
}

// New builds the root logger from configuration.
func New(cfg config.LogConfig, out io.Writer, secrets *Redactor) (*slog.Logger, error) {
	level, err := levelFrom(cfg.Level)
	if err != nil {
		return nil, err
	}
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}
	handler = &requestIDHandler{inner: handler}
	if secrets != nil {
		handler = &redactHandler{inner: handler, red: secrets}
	}
	return slog.New(handler), nil
}

func levelFrom(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: unknown level %q", s)
	}
}

// requestIDHandler attaches the correlation ID from the context.
type requestIDHandler struct{ inner slog.Handler }

func (h *requestIDHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := RequestID(ctx); ok {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.inner.Handle(ctx, r)
}

func (h *requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &requestIDHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *requestIDHandler) WithGroup(name string) slog.Handler {
	return &requestIDHandler{inner: h.inner.WithGroup(name)}
}

// Redactor scrubs registered secret values from strings. Values shorter
// than 4 characters are not registered (redaction would corrupt output);
// the expectation is real credential material.
type Redactor struct {
	mu      sync.RWMutex
	secrets []replacement
}

type replacement struct{ from, to string }

// NewRedactor builds a redactor from secret values (kind names the
// placeholder).
func NewRedactor(secrets map[string]string) *Redactor {
	r := &Redactor{}
	for v, kind := range secrets {
		if len(v) >= 4 {
			r.secrets = append(r.secrets, replacement{from: v, to: "<" + kind + ">"})
		}
	}
	return r
}

// Register adds a secret discovered at runtime.
func (r *Redactor) Register(value, kind string) {
	if len(value) < 4 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = append(r.secrets, replacement{from: value, to: "<" + kind + ">"})
}

func (r *Redactor) Scrub(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rep := range r.secrets {
		s = strings.ReplaceAll(s, rep.from, rep.to)
	}
	return s
}

// redactHandler scrubs the message and every attribute value before the
// inner handler serializes.
type redactHandler struct {
	inner slog.Handler
	red   *Redactor
}

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	scrubbed := slog.NewRecord(r.Time, r.Level, h.red.Scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		scrubbed.AddAttrs(scrubAttr(h.red, a))
		return true
	})
	return h.inner.Handle(ctx, scrubbed)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = scrubAttr(h.red, a)
	}
	return &redactHandler{inner: h.inner.WithAttrs(scrubbed), red: h.red}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name), red: h.red}
}

func scrubAttr(red *Redactor, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.Attr{Key: a.Key, Value: slog.StringValue(red.Scrub(a.Value.String()))}
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))
		for i, ga := range group {
			out[i] = scrubAttr(red, ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	default:
		return a
	}
}
