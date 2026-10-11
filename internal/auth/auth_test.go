// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

var fast = Argon2Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}

func TestHasher(t *testing.T) {
	ctx := context.Background()
	h := NewHasher(fast, 2)
	enc, err := h.Hash(ctx, "secret password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash %q", enc)
	}
	if again, _ := h.Hash(ctx, "secret password"); again == enc {
		t.Fatal("hashes are not salted")
	}
	if ok, rehash, err := h.Verify(ctx, "secret password", enc); !ok || rehash || err != nil {
		t.Fatalf("verify: %v %v %v", ok, rehash, err)
	}
	if ok, _, _ := h.Verify(ctx, "secret passworD", enc); ok {
		t.Fatal("wrong password accepted")
	}
	// A hash from older parameters still verifies and asks for a rehash.
	stronger := NewHasher(Argon2Params{MemoryKiB: 128, Iterations: 1, Parallelism: 1}, 1)
	if ok, rehash, _ := stronger.Verify(ctx, "secret password", enc); !ok || !rehash {
		t.Fatalf("old params: ok=%v rehash=%v", ok, rehash)
	}
	for _, bad := range []string{"", "$argon2i$v=19$m=64,t=1,p=1$c2FsdA$a2V5", "$argon2id$v=19$m=9999999,t=1,p=1$c2FsdA$a2V5", "$argon2id$v=19$m=64,t=1,p=1$!!$a2V5"} {
		if _, _, err := h.Verify(ctx, "x", bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	h.VerifyDummy(ctx, "anything") // must not panic
}

func TestHasherHonoursContext(t *testing.T) {
	h := NewHasher(fast, 1)
	h.slots <- struct{}{} // all slots busy
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "x"); err == nil {
		t.Fatal("hash ran without a free slot")
	}
}

func TestFailureLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewFailureLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for range 3 {
		if blocked, _ := l.Blocked("k"); blocked {
			t.Fatal("blocked too early")
		}
		l.Fail("k")
	}
	blocked, wait := l.Blocked("k")
	if !blocked || wait != time.Minute {
		t.Fatalf("blocked=%v wait=%v", blocked, wait)
	}
	if b, _ := l.Blocked("other"); b {
		t.Fatal("keys share counts")
	}
	now = now.Add(time.Minute)
	if b, _ := l.Blocked("k"); b {
		t.Fatal("still blocked after the window")
	}
	l.Fail("k")
	l.Reset("k")
	if b, _ := l.Blocked("k"); b {
		t.Fatal("reset didn't clear")
	}
}

func TestTokens(t *testing.T) {
	a, b := newToken(APIKeyPrefix), newToken(APIKeyPrefix)
	if a == b || !strings.HasPrefix(a, APIKeyPrefix) || len(a) != len(APIKeyPrefix)+43 {
		t.Fatalf("tokens %q %q", a, b)
	}
	tok, hash := NewInviteToken()
	if string(HashInviteToken(tok)) != string(hash) || len(hash) != 32 {
		t.Fatal("invite hash mismatch")
	}
}

func TestRoleScopes(t *testing.T) {
	if got := strings.Join(RoleScopes(RoleAdmin), ","); got != "read,write,admin,metrics:read" {
		t.Fatalf("admin %s", got)
	}
	if got := strings.Join(RoleScopes(RoleUser), ","); got != "read,write" {
		t.Fatalf("user %s", got)
	}
	var p *Principal
	if p.IsAdmin() || p.HasScope(ScopeRead) {
		t.Fatal("nil principal holds something")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo", 2); got != "h" {
		t.Fatalf("cut inside a rune: %q", got)
	}
	if got := truncate("bad\xffbytes", 100); got != "badbytes" {
		t.Fatalf("invalid UTF-8 kept: %q", got)
	}
}
