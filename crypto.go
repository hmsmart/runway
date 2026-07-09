package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

func EncryptColumnSecret(plainText string, primaryKey string, key []byte) (string, error) {
	// Guard against invalid key lengths for AES (16, 24, or 32 bytes)
	if len(key) != 32 && len(key) != 24 && len(key) != 16 {
		return "", fmt.Errorf("invalid key length: must be 32, 24, or 16 bytes")
	}

	// Guard against empty primary key
	if primaryKey == "" {
		return "", fmt.Errorf("primary key cannot be empty")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("creating GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plainText), []byte(primaryKey))
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func DecryptColumnSecret(encryptedText string, primaryKey string, key []byte) (string, error) {
	// Guard against invalid key lengths for AES (16, 24, or 32 bytes)
	if len(key) != 32 && len(key) != 24 && len(key) != 16 {
		return "", fmt.Errorf("invalid key length: must be 32, 24, or 16 bytes")
	}

	// Guard against empty primary key
	if primaryKey == "" {
		return "", fmt.Errorf("primary key cannot be empty")
	}

	blob, err := base64.StdEncoding.DecodeString(encryptedText)
	if err != nil {
		return "", fmt.Errorf("decoding base64: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("creating GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(blob) < nonceSize {
		return "", fmt.Errorf("ciphertext too short (%d bytes)", len(blob))
	}

	nonce, ciphertext := blob[:nonceSize], blob[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(primaryKey))
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}

	return string(plaintext), nil
}

func RandomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
