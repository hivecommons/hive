package hub

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var hmacKeyPath = "/data/saas/hmac.key"

const hmacKeySize = 32

func loadOrCreateHMACKey() ([]byte, error) {
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(filepath.Dir(hmacKeyPath), 0o755)
	if data, err := os.ReadFile(hmacKeyPath); err == nil && len(data) == hmacKeySize {
		return data, nil
	}
	key := make([]byte, hmacKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(hmacKeyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write HMAC key: %w", err)
	}
	return key, nil
}

func encryptToken(plaintext string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
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
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptToken(encoded string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
