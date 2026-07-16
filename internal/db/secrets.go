package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const encryptedAPIKeyPrefix = "enc:v1:"

func localKeyMaterial() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate local provider encryption key: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(key), nil
}

type secretCipher struct {
	aead cipher.AEAD
}

func newSecretCipher(keyMaterial string) (*secretCipher, error) {
	if strings.TrimSpace(keyMaterial) == "" {
		return nil, errors.New("provider encryption key is required")
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(keyMaterial))
	derived := digest.Sum(nil)
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("create provider key cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create provider key AEAD: %w", err)
	}
	return &secretCipher{aead: aead}, nil
}

func (c *secretCipher) encrypt(value string) (string, error) {
	if c == nil {
		return value, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate provider key nonce: %w", err)
	}
	ciphertext := c.aead.Seal(nonce, nonce, []byte(value), nil)
	return encryptedAPIKeyPrefix + base64.RawStdEncoding.EncodeToString(ciphertext), nil
}

func (c *secretCipher) decrypt(value string) (string, error) {
	if c == nil || !strings.HasPrefix(value, encryptedAPIKeyPrefix) {
		// Plaintext values are accepted for backwards-compatible reads and are
		// encrypted on the next write.
		return value, nil
	}
	encoded := strings.TrimPrefix(value, encryptedAPIKeyPrefix)
	ciphertext, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode encrypted provider key: %w", err)
	}
	if len(ciphertext) < c.aead.NonceSize() {
		return "", errors.New("encrypted provider key is truncated")
	}
	nonce := ciphertext[:c.aead.NonceSize()]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext[c.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("decrypt provider key")
	}
	return string(plaintext), nil
}
