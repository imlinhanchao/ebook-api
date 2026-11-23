// Package secret provides AES-256-GCM sealing and opening for opaque
// provider credential tokens (ebk1.<base64url>).
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

const tokenPrefix = "ebk1."

var (
	ErrInvalidToken     = errors.New("invalid credential token")
	ErrProviderMismatch = errors.New("credential token does not match provider")
)

type sealedPayload struct {
	Provider    string                `json:"p"`
	Credentials providers.Credentials `json:"c"`
	IssuedAt    int64                 `json:"iat"`
}

// Seal encrypts provider credentials into an opaque, tamper-proof token bound
// to providerID via AES-256-GCM AAD.
func Seal(key []byte, providerID string, creds providers.Credentials) (string, error) {
	if len(key) != 32 {
		return "", errors.New("secret key must be 32 bytes")
	}
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	if providerID == "" {
		return "", errors.New("provider id is required")
	}
	if len(creds) == 0 {
		return "", errors.New("credentials are empty")
	}

	plain, err := json.Marshal(sealedPayload{
		Provider:    providerID,
		Credentials: creds,
		IssuedAt:    time.Now().Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal credentials: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	aad := tokenAAD(providerID)
	sealed := gcm.Seal(nonce, nonce, plain, aad)
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decrypts and authenticates a credential token, verifying it was sealed
// for expectedProviderID.
func Open(key []byte, expectedProviderID, token string) (providers.Credentials, error) {
	if len(key) != 32 {
		return nil, errors.New("secret key must be 32 bytes")
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, tokenPrefix) {
		return nil, ErrInvalidToken
	}
	expectedProviderID = strings.ToLower(strings.TrimSpace(expectedProviderID))

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, tokenPrefix))
	if err != nil {
		return nil, ErrInvalidToken
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, ErrInvalidToken
	}

	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, tokenAAD(expectedProviderID))
	if err != nil {
		return nil, ErrInvalidToken
	}

	var payload sealedPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, ErrInvalidToken
	}
	if payload.Provider != expectedProviderID {
		return nil, ErrProviderMismatch
	}
	return payload.Credentials, nil
}

func tokenAAD(providerID string) []byte {
	return []byte("ebook-api:v1:" + providerID)
}

// GenerateKey creates a cryptographically random 32-byte AES-256 key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// DecodeKey parses a 32-byte key encoded as base64 (standard or URL) or hex.
func DecodeKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := dec(raw); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("需要 32 字节密钥（base64 或 hex）")
}

// LoadOrCreateKey resolves the 32-byte secret key from envValue, or loads/creates
// <dataDir>/secret.key on disk so tokens survive service restarts.
func LoadOrCreateKey(envValue, dataDir string) ([]byte, error) {
	if strings.TrimSpace(envValue) != "" {
		key, err := DecodeKey(envValue)
		if err != nil {
			return nil, fmt.Errorf("EBOOK_SECRET 无效: %w", err)
		}
		return key, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return GenerateKey()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s: %w", dataDir, err)
	}
	keyPath := filepath.Join(dataDir, "secret.key")
	if data, err := os.ReadFile(keyPath); err == nil {
		return DecodeKey(strings.TrimSpace(string(data)))
	}
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("写入 %s: %w", keyPath, err)
	}
	return key, nil
}
