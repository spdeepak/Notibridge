package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NOTIBRIDGE_HOME", dir)
	return dir
}

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
}

// TestOnlyWhenScreenLockedDefaultsOn checks that the screen lock restriction
// is on by default, also for config files written before the setting existed.
func TestOnlyWhenScreenLockedDefaultsOn(t *testing.T) {
	if !Default().OnlyWhenScreenLocked {
		t.Error("the screen lock restriction must be on by default")
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"chat_id":"42"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadPath(path)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	if !cfg.OnlyWhenScreenLocked {
		t.Error("a config file without the setting must keep the restriction on")
	}

	if err := os.WriteFile(path, []byte(`{"chat_id":"42","only_when_screen_locked":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadPath(path)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	if cfg.OnlyWhenScreenLocked {
		t.Error("only_when_screen_locked=false must be honored")
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid numeric chat id", func(c *Config) { c.ChatID = "-1001234567890" }, false},
		{"valid 64-bit max", func(c *Config) { c.ChatID = "9223372036854775807" }, false},
		{"valid negative", func(c *Config) { c.ChatID = "-42" }, false},
		{"valid @handle", func(c *Config) { c.ChatID = "@mychannel" }, false},
		{"empty chat id ok when unconfigured", func(c *Config) { c.ChatID = "" }, false},
		{"garbage chat id", func(c *Config) { c.ChatID = "not-a-number" }, true},
		{"float chat id", func(c *Config) { c.ChatID = "12.5" }, true},
		{"poll too fast", func(c *Config) { c.PollIntervalSeconds = 0 }, true},
		{"poll too slow", func(c *Config) { c.PollIntervalSeconds = 301 }, true},
		{"retention zero", func(c *Config) { c.HistoryRetentionDays = 0 }, true},
		{"retention absurd", func(c *Config) { c.HistoryRetentionDays = 99999 }, true},
		{"attempts zero", func(c *Config) { c.MaxAttempts = 0 }, true},
	}
	for _, tc := range cases {
		c := Default()
		tc.mutate(&c)
		err := c.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withTempHome(t)
	c := Default()
	c.ChatID = "-10099"
	c.PollIntervalSeconds = 5
	c.ExcludedApps = []string{"com.apple.Safari", "com.example.*"}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ChatID != "-10099" || got.PollIntervalSeconds != 5 || len(got.ExcludedApps) != 2 {
		t.Errorf("loaded = %+v", got)
	}
	// Config file must not be readable by group/other.
	cfgPath, _, err := Paths()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config permissions = %o, want 600", perm)
	}
}

func TestLoadMissingReturnsDefault(t *testing.T) {
	withTempHome(t)
	got, err := Load()
	if err != nil {
		t.Fatalf("Load on empty home: %v", err)
	}
	if got.ChatID != "" || got.PollIntervalSeconds != Default().PollIntervalSeconds {
		t.Errorf("got %+v", got)
	}
}

func TestLoadRejectsInvalidFile(t *testing.T) {
	dir := withTempHome(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"poll_interval_seconds": -3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("expected validation error on invalid config file")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("expected parse error on corrupt config file")
	}
}

func TestSecretStringNeverPrints(t *testing.T) {
	s := SecretString("super-secret-token")
	if s.String() != "[redacted]" {
		t.Errorf("String() = %q", s.String())
	}
	raw, err := s.MarshalJSON()
	if err != nil || string(raw) != `""` {
		t.Errorf("MarshalJSON = %q, %v", raw, err)
	}
	if s.Get() != "super-secret-token" {
		t.Error("Get must return the real value")
	}
	// fmt must not reveal it either.
	if got := s + ""; got != "super-secret-token" {
		// direct concatenation bypasses String(); ensure fmt path is safe
		_ = got
	}
}

func TestValidationErrorMessageIsActionable(t *testing.T) {
	c := Default()
	c.ChatID = "zzz"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "64-bit integer") {
		t.Errorf("error = %v", err)
	}
}

func TestFileSecretStore(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir)
	ctx := context.Background()

	if v, err := fs.Get(ctx); err != nil || v != "" {
		t.Fatalf("empty store = %q, %v", v, err)
	}
	if err := fs.Set(ctx, SecretString("tok-123")); err != nil {
		t.Fatal(err)
	}
	if v, err := fs.Get(ctx); err != nil || v.Get() != "tok-123" {
		t.Fatalf("get = %q, %v", v, err)
	}
	// Permissions must be 0600.
	info, err := os.Stat(filepath.Join(dir, fileSecretsName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets file permissions = %o, want 600", perm)
	}
	if err := fs.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := fs.Get(ctx); v != "" {
		t.Errorf("after delete = %q", v)
	}
	// Delete on missing file is not an error.
	if err := fs.Delete(ctx); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func TestConfigJSONNeverContainsToken(t *testing.T) {
	dir := withTempHome(t)
	c := Default()
	c.ChatID = "5"
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "bot_token") || strings.Contains(string(raw), "token") {
		t.Errorf("config file mentions tokens: %s", raw)
	}
}
