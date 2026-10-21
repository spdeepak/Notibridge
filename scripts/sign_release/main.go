// Command sign_release signs a release checksum file with an Ed25519 key and
// prints the matching public key.
//
// Usage:
//
//	go run ./scripts/sign_release pubkey <key-file>
//	go run ./scripts/sign_release sign <key-file> <file> [<signature-file>]
//
// The key file holds the base64-encoded Ed25519 private key: either the
// 32-byte seed or the 64-byte private key. Whitespace is ignored.
//
// The public key is what the release workflow embeds into the application
// (internal/update.PublicKey); the signature is the detached file published
// next to SHA256SUMS. The application refuses to install an update whose
// signature does not match the embedded public key.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sign_release:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: sign_release pubkey|sign <key-file> [file] [signature-file]")
	}
	key, err := loadKey(args[1])
	if err != nil {
		return err
	}
	switch args[0] {
	case "pubkey":
		fmt.Println(base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
		return nil
	case "sign":
		if len(args) < 3 {
			return fmt.Errorf("usage: sign_release sign <key-file> <file> [<signature-file>]")
		}
		data, err := os.ReadFile(args[2])
		if err != nil {
			return fmt.Errorf("read %s: %w", args[2], err)
		}
		out := args[2] + ".sig"
		if len(args) > 3 {
			out = args[3]
		}
		if err := os.WriteFile(out, ed25519.Sign(key, data), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
		fmt.Printf("signed %s -> %s\n", args[2], out)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// loadKey reads a base64-encoded Ed25519 private key.
func loadKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("the key file is not base64: %w", err)
	}
	switch len(blob) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(blob), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(blob), nil
	}
	return nil, fmt.Errorf("the key must be %d or %d bytes, got %d",
		ed25519.SeedSize, ed25519.PrivateKeySize, len(blob))
}
