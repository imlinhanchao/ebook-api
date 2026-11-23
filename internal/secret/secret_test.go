package secret

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

func TestSealAndOpenRoundTrip(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	orig := providers.Credentials{
		"domain":        "https://z-lib.gd",
		"remix_userid":  "12345",
		"remix_userkey": "super-secret-key",
	}
	token, err := Seal(key, "zlib", orig)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		t.Fatalf("unexpected token format: %q", token)
	}
	if strings.Contains(token, "super-secret-key") {
		t.Fatalf("token leaked plaintext secret")
	}

	got, err := Open(key, "zlib", token)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got["remix_userid"] != "12345" || got["remix_userkey"] != "super-secret-key" || got["domain"] != "https://z-lib.gd" {
		t.Fatalf("opened creds mismatch: %+v", got)
	}

	// Cross-provider replay must fail (AAD mismatch).
	if _, err := Open(key, "other", token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for wrong provider, got %v", err)
	}

	// Wrong key must fail.
	otherKey, _ := GenerateKey()
	if _, err := Open(otherKey, "zlib", token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for wrong key, got %v", err)
	}

	// Tampered token must fail.
	tampered := token[:len(token)-2] + "AA"
	if _, err := Open(key, "zlib", tampered); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for tampered token, got %v", err)
	}
}

func TestLoadOrCreateKeyPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	k1, err := LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatalf("first LoadOrCreateKey: %v", err)
	}
	k2, err := LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatalf("expected persisted key to match across loads")
	}
}
