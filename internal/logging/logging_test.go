package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactScrubsRegisteredSecrets(t *testing.T) {
	RegisterSecret("tok-abc-123")
	defer UnregisterSecret("tok-abc-123")
	in := "error while calling bot tok-abc-123: boom"
	out := Redact(in)
	if strings.Contains(out, "tok-abc-123") {
		t.Errorf("secret not redacted: %q", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("redaction marker missing: %q", out)
	}
}

func TestRedactIgnoresEmptyAndDuplicates(t *testing.T) {
	RegisterSecret("")
	RegisterSecret("s1")
	RegisterSecret("s1") // duplicate must not panic
	if Redact("clean text s1") == "clean text s1" {
		t.Error("s1 should be redacted")
	}
	UnregisterSecret("s1")
	if Redact("s1 stays") != "s1 stays" {
		t.Error("unregistered secret must not be redacted")
	}
}

func TestLoggerRedactsMessagesAndAttrs(t *testing.T) {
	RegisterSecret("super-secret-token")
	defer UnregisterSecret("super-secret-token")

	var buf bytes.Buffer
	logger := slog.New(NewHandler(slog.LevelInfo, &buf))
	logger.Info("token is super-secret-token here", "chat", "super-secret-token", "app", "com.example")
	_logged := buf.String()
	if strings.Contains(_logged, "super-secret-token") {
		t.Errorf("secret leaked into logs: %s", _logged)
	}
	if !strings.Contains(_logged, "com.example") {
		t.Errorf("non-secret attr lost: %s", _logged)
	}
}

func TestWithAttrsRedacts(t *testing.T) {
	RegisterSecret("leaky-token")
	defer UnregisterSecret("leaky-token")
	var buf bytes.Buffer
	logger := slog.New(NewHandler(slog.LevelInfo, &buf))
	child := logger.With("token", "leaky-token")
	child.Error("failed")
	if strings.Contains(buf.String(), "leaky-token") {
		t.Errorf("secret leaked via With: %s", buf.String())
	}
}
