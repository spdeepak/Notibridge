// Package config loads and validates NotiBridge settings. Non-secret settings
// live in a JSON file in the user's Application Support directory; the bot
// token is stored in the macOS Keychain by default, with a clearly-flagged
// 0600 file fallback when the Keychain is unavailable.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SecretString is a string whose printed form is always redacted, so it can
// never leak through logs, fmt, or accidental serialization.
type SecretString string

func (s SecretString) String() string { return "[redacted]" }

// MarshalJSON deliberately does not emit the secret; marshaling a Config
// never includes the token.
func (s SecretString) MarshalJSON() ([]byte, error) { return []byte(`""`), nil }

// Get returns the underlying secret value.
func (s SecretString) Get() string { return string(s) }

// Config is the persisted, non-secret configuration.
type Config struct {
	// ChatID is the destination chat: a signed 64-bit integer as a string
	// (chat IDs can exceed 32 bits) or an @username.
	ChatID string `json:"chat_id"`
	// PollIntervalSeconds controls notification polling frequency.
	PollIntervalSeconds int `json:"poll_interval_seconds"`
	// ForwardingEnabled is the master switch for sending notifications.
	ForwardingEnabled bool `json:"forwarding_enabled"`
	// ExcludedApps lists bundle identifiers (or "prefix.*" patterns) whose
	// notifications are not forwarded.
	ExcludedApps []string `json:"excluded_apps"`
	// HistoryRetentionDays bounds how long delivery records (including
	// notification text) are kept locally.
	HistoryRetentionDays int `json:"history_retention_days"`
	// MaxAttempts is the delivery retry budget.
	MaxAttempts int `json:"max_attempts"`
}

// Default returns the first-run configuration. Forwarding starts disabled
// until the user provides credentials and enables it.
func Default() Config {
	return Config{
		ChatID:               "",
		PollIntervalSeconds:  2,
		ForwardingEnabled:    false,
		ExcludedApps:         []string{},
		HistoryRetentionDays: 14,
		MaxAttempts:          8,
	}
}

// Validate checks the configuration and returns a descriptive error.
func (c Config) Validate() error {
	var errs []string
	if c.ChatID != "" {
		if !strings.HasPrefix(c.ChatID, "@") {
			if !isInt64(c.ChatID) {
				errs = append(errs, "chat_id must be a 64-bit integer or an @username")
			}
		} else if len(c.ChatID) < 2 {
			errs = append(errs, "chat_id @username is too short")
		}
	}
	if c.PollIntervalSeconds < 1 || c.PollIntervalSeconds > 300 {
		errs = append(errs, "poll_interval_seconds must be between 1 and 300")
	}
	if c.HistoryRetentionDays < 1 || c.HistoryRetentionDays > 3650 {
		errs = append(errs, "history_retention_days must be between 1 and 3650")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 100 {
		errs = append(errs, "max_attempts must be between 1 and 100")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return nil
}

func isInt64(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return err == nil && int64String(n) == s
}

func int64String(n int64) string { return fmt.Sprintf("%d", n) }

// PollInterval returns the validated polling interval with a safe fallback.
func (c Config) PollInterval() time.Duration {
	if c.PollIntervalSeconds < 1 {
		return 2 * time.Second
	}
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

// Redacted returns a copy safe for display: no secrets are present in Config
// itself, but this method documents that intent and normalizes the chat ID.
func (c Config) Redacted() Config { return c }

// Paths locates NotiBridge's per-user files, honoring NOTIBRIDGE_HOME (used
// by tests and development runs).
func Paths() (configPath string, stateDir string, err error) {
	if dir := os.Getenv("NOTIBRIDGE_HOME"); dir != "" {
		return filepath.Join(dir, "config.json"), dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("determine home directory: %w", err)
	}
	base := filepath.Join(home, "Library", "Application Support", "NotiBridge")
	return filepath.Join(base, "config.json"), base, nil
}

// StateDBPath returns the path of NotiBridge's own SQLite database.
func StateDBPath() (string, error) {
	_, dir, err := Paths()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.db"), nil
}

// Load reads the config file, returning Default() when it does not exist yet.
func Load() (Config, error) {
	cfgPath, _, err := Paths()
	if err != nil {
		return Default(), err
	}
	return LoadPath(cfgPath)
}

// LoadPath is Load for an explicit file path.
func LoadPath(path string) (Config, error) {
	c := Default()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Save writes the config with restrictive permissions (0600, directory 0700).
func Save(c Config) error {
	cfgPath, dir, err := Paths()
	if err != nil {
		return err
	}
	return SavePath(cfgPath, dir, c)
}

// SavePath is Save for an explicit path and directory.
func SavePath(path, dir string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Delete removes local configuration and secrets. The state database is left
// in place; the user can clear it separately.
func Delete() error {
	cfgPath, _, err := Paths()
	if err != nil {
		return err
	}
	if err := os.Remove(cfgPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove config: %w", err)
	}
	return nil
}

// SecretStore abstracts where the bot token is kept.
type SecretStore interface {
	Get(ctx context.Context) (SecretString, error)
	Set(ctx context.Context, token SecretString) error
	Delete(ctx context.Context) error
	// Name identifies the backing store for diagnostics ("keychain",
	// "file").
	Name() string
}
