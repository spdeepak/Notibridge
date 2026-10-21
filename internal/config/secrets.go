package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

const (
	keyringService  = "NotiBridge"
	keyringAccount  = "bot_token"
	fileSecretsName = "secrets.json"
)

// KeyringStore stores the bot token in the macOS Keychain via the generic
// password facility. This is the preferred store: the token is encrypted by
// macOS and is not readable from ordinary file access or backups as plain
// text.
type KeyringStore struct{}

func (KeyringStore) Get(ctx context.Context) (SecretString, error) {
	// go-keyring's darwin implementation performs blocking Keychain calls;
	// run them without hanging forever by racing a context deadline.
	type res struct {
		v   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := keyring.Get(keyringService, keyringAccount)
		ch <- res{v, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if errors.Is(r.err, keyring.ErrNotFound) {
			return "", nil
		}
		if r.err != nil {
			return "", fmt.Errorf("read bot token from Keychain: %w", r.err)
		}
		return SecretString(r.v), nil
	}
}

func (KeyringStore) Set(ctx context.Context, token SecretString) error {
	if token == "" {
		return KeyringStore{}.Delete(ctx)
	}
	if err := keyring.Set(keyringService, keyringAccount, token.Get()); err != nil {
		return fmt.Errorf("write bot token to Keychain: %w", err)
	}
	return nil
}

func (KeyringStore) Delete(ctx context.Context) error {
	if err := keyring.Delete(keyringService, keyringAccount); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete bot token from Keychain: %w", err)
	}
	return nil
}

func (KeyringStore) Name() string { return "keychain" }

// FileStore is the fallback used when the Keychain is unavailable (for
// example in some CI or headless environments). The token is stored in a
// 0600 JSON file inside the app's private Application Support directory.
//
// Limitations: the file is plaintext on disk (though protected by file
// permissions and FileVault at rest), and it can be copied or backed up more
// easily than Keychain items. Users of this store are warned by the UI.
type FileStore struct {
	dir string
}

// NewFileStore stores secrets under dir (usually the app state directory).
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

func (s *FileStore) path() string { return filepath.Join(s.dir, fileSecretsName) }

func (s *FileStore) Get(ctx context.Context) (SecretString, error) {
	raw, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read secrets file: %w", err)
	}
	var m struct {
		BotToken string `json:"bot_token"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("parse secrets file: %w", err)
	}
	return SecretString(m.BotToken), nil
}

func (s *FileStore) Set(ctx context.Context, token SecretString) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create secrets directory: %w", err)
	}
	if token == "" {
		if err := os.Remove(s.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove secrets file: %w", err)
		}
		return nil
	}
	raw, err := json.MarshalIndent(map[string]string{"bot_token": token.Get()}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode secrets: %w", err)
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write secrets file: %w", err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		return fmt.Errorf("replace secrets file: %w", err)
	}
	return nil
}

func (s *FileStore) Delete(ctx context.Context) error {
	if err := os.Remove(s.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove secrets file: %w", err)
	}
	return nil
}

func (s *FileStore) Name() string { return "file" }

// DefaultSecretStore returns the Keychain store when usable, falling back to
// a 0600 file store in stateDir when the Keychain cannot be read (for
// example in headless or CI environments). The chosen store's Name() lets
// the UI warn users about the weaker fallback.
func DefaultSecretStore(ctx context.Context, stateDir string) SecretStore {
	ks := KeyringStore{}
	if _, err := ks.Get(ctx); err == nil {
		return ks
	}
	return NewFileStore(stateDir)
}
