// SPDX-License-Identifier: GPL-3.0-or-later

// Package secret encrypts small secrets (service keys, plugin keys, storage credentials) before they are
// stored, with AES-256-GCM.
//
// The key comes from PANELARR_SECRET_KEY (base64 of 32 bytes) or, when that
// isn't set, from a key file in the server's config directory, generated on first
// use. Keep the key with your database backups: without it, stored
// passwords can't be read back.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// prefix marks values sealed by this package, with a version.
const prefix = "enc:v1:"

// Box seals and opens secrets. It loads its key on first use, so the server runs
// fine without a key until a secret has to be stored.
type Box struct {
	envKey  string
	keyFile string

	once sync.Once
	aead cipher.AEAD
	err  error
}

// NewBox returns a box whose key is envKey (base64) when set, otherwise the
// key in keyFile, created when missing.
func NewBox(envKey, keyFile string) *Box {
	return &Box{envKey: strings.TrimSpace(envKey), keyFile: keyFile}
}

func (b *Box) load() (cipher.AEAD, error) {
	b.once.Do(func() {
		key, err := b.key()
		if err != nil {
			b.err = err
			return
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			b.err = err
			return
		}
		b.aead, b.err = cipher.NewGCM(block)
	})
	return b.aead, b.err
}

func decodeKey(s, where string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("secret: %s must be the base64 of exactly 32 bytes (e.g. `openssl rand -base64 32`)", where)
	}
	return key, nil
}

func (b *Box) key() ([]byte, error) {
	if b.envKey != "" {
		return decodeKey(b.envKey, "PANELARR_SECRET_KEY")
	}
	if b.keyFile == "" {
		return nil, errors.New("secret: no encryption key: set PANELARR_SECRET_KEY or PANELARR_CONFIG")
	}
	raw, err := os.ReadFile(b.keyFile)
	if err == nil {
		return decodeKey(string(raw), b.keyFile)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secret: read key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(b.keyFile), 0o700); err != nil {
		return nil, fmt.Errorf("secret: create key directory (set PANELARR_SECRET_KEY or a writable PANELARR_CONFIG): %w", err)
	}
	f, err := os.OpenFile(b.keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("secret: create key file (set PANELARR_SECRET_KEY or a writable PANELARR_CONFIG): %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		return nil, err
	}
	return key, nil
}

// Seal encrypts plain.
func (b *Box) Seal(plain string) (string, error) {
	aead, err := b.load()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a value from Seal.
func (b *Box) Open(sealed string) (string, error) {
	if !strings.HasPrefix(sealed, prefix) {
		return "", errors.New("secret: not a sealed value")
	}
	aead, err := b.load()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil || len(raw) < aead.NonceSize() {
		return "", errors.New("secret: malformed sealed value")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("secret: can't decrypt (a different key?)")
	}
	return string(plain), nil
}
