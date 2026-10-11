// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are argon2id cost parameters.
type Argon2Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// DefaultArgon2 follows the common recommendation for argon2id (19 MiB, two
// passes), which keeps a login cheap enough for small home servers.
var DefaultArgon2 = Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1}

const (
	saltLen = 16
	keyLen  = 32
)

// Hasher hashes and verifies passwords with argon2id. It caps how many
// hashes run at once, so a burst of logins can't exhaust memory.
type Hasher struct {
	params Argon2Params
	slots  chan struct{}

	dummyOnce sync.Once
	dummy     string
}

// NewHasher returns a hasher running at most concurrency hashes at a time.
func NewHasher(p Argon2Params, concurrency int) *Hasher {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Hasher{params: p, slots: make(chan struct{}, concurrency)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.slots }

// Hash returns a PHC-format argon2id hash of password.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	p := h.params
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

var errBadHash = errors.New("auth: malformed password hash")

func parseHash(encoded string) (Argon2Params, []byte, []byte, error) {
	var p Argon2Params
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, errBadHash
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return p, nil, nil, errBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return p, nil, nil, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, errBadHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return p, nil, nil, errBadHash
	}
	return p, salt, key, nil
}

// Verify reports whether password matches encoded, and whether the hash
// should be replaced because its parameters are outdated.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (ok, rehash bool, err error) {
	p, salt, key, err := parseHash(encoded)
	if err != nil {
		return false, false, err
	}
	// Refuse absurd costs from a tampered hash instead of allocating them.
	if p.MemoryKiB > 1<<20 || p.Iterations > 64 || p.Parallelism == 0 {
		return false, false, errBadHash
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, uint32(len(key)))
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false, nil
	}
	return true, p != h.params, nil
}

// VerifyDummy spends the same time as a real check, for unknown users, so
// response times don't reveal which usernames exist.
func (h *Hasher) VerifyDummy(ctx context.Context, password string) {
	h.dummyOnce.Do(func() {
		h.dummy, _ = h.Hash(context.Background(), "not a real password")
	})
	_, _, _ = h.Verify(ctx, password, h.dummy)
}
