// Package logging configures structured, privacy-conscious logging for
// NotiBridge. Notification bodies, bot tokens and chat identifiers must never
// reach the log by default; helpers in this package enforce redaction.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// secrets holds values that must be scrubbed from any log message.
type redactor struct {
	mu      sync.Mutex
	secrets []string
}

var globalRedactor = &redactor{}

// RegisterSecret adds a value that will be scrubbed from every log record.
// Empty strings are ignored. Registering the same secret twice is harmless.
func RegisterSecret(secret string) {
	if secret == "" {
		return
	}
	globalRedactor.mu.Lock()
	defer globalRedactor.mu.Unlock()
	for _, s := range globalRedactor.secrets {
		if s == secret {
			return
		}
	}
	globalRedactor.secrets = append(globalRedactor.secrets, secret)
}

// UnregisterSecret removes a previously registered secret.
func UnregisterSecret(secret string) {
	if secret == "" {
		return
	}
	globalRedactor.mu.Lock()
	defer globalRedactor.mu.Unlock()
	out := globalRedactor.secrets[:0]
	for _, s := range globalRedactor.secrets {
		if s != secret {
			out = append(out, s)
		}
	}
	globalRedactor.secrets = out
}

// Redact scrubs every registered secret from s.
func Redact(s string) string {
	globalRedactor.mu.Lock()
	defer globalRedactor.mu.Unlock()
	for _, secret := range globalRedactor.secrets {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// handler wraps a slog.Handler and redacts message text and string attributes.
type handler struct {
	next slog.Handler
}

// NewHandler builds a redacting handler writing to w. Both messages and
// string attributes pass through Redact before reaching the output.
func NewHandler(level slog.Level, w io.Writer) slog.Handler {
	return &handler{next: slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if v, ok := a.Value.Any().(string); ok {
				a.Value = slog.StringValue(Redact(v))
			}
			return a
		},
	})}
}

// New returns a slog.Logger that writes to stderr at the given level with
// every registered secret scrubbed from output.
func New(level slog.Level) *slog.Logger {
	return slog.New(NewHandler(level, os.Stderr))
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	r.Message = Redact(r.Message)
	return h.next.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		if v, ok := a.Value.Any().(string); ok {
			a.Value = slog.StringValue(Redact(v))
		}
		out[i] = a
	}
	return &handler{next: h.next.WithAttrs(out)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{next: h.next.WithGroup(name)}
}
