// SPDX-License-Identifier: GPL-3.0-or-later

package limits

import (
	"context"
	"testing"
	"time"
)

func TestConcurrency(t *testing.T) {
	r := NewRegistry()
	r.Set("source:a", Limit{Concurrency: 2})
	k := r.For("source:a")
	if got := acquire(k, 3); got != 2 {
		t.Fatalf("%d slots taken, want 2", got)
	}
	k.Release()
	if !k.TryAcquire() {
		t.Fatal("slot not freed")
	}
	// Unknown keys get the default.
	if got := acquire(r.For("source:b"), 5); got != Default.Concurrency {
		t.Fatalf("default: %d slots", got)
	}
}

// acquire tries n times and returns how many slots it got.
func acquire(k *Key, n int) int {
	got := 0
	for range n {
		if k.TryAcquire() {
			got++
		}
	}
	return got
}

func TestRate(t *testing.T) {
	r := NewRegistry()
	r.Set("k", Limit{Concurrency: 1, Every: 50 * time.Millisecond, Burst: 1})
	k := r.For("k")
	start := time.Now()
	for range 3 {
		if err := k.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 90*time.Millisecond {
		t.Fatalf("3 requests in %v", d)
	}
	r.Set("free", Limit{Concurrency: 1})
	f := r.For("free")
	start = time.Now()
	for range 100 {
		_ = f.Wait(context.Background())
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("unlimited key waited")
	}
}
