// SPDX-License-Identifier: GPL-3.0-or-later

package users

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/store/storetest"
)

// Two admins sign in with single sign-on at the same time, each no longer
// in the admin groups: each sign-in alone may demote its admin, since the
// other one is left, but together they must leave one. The sign-ins run
// under the accounts lock, so the second one counts after the first one's
// demotion. The first sign-in pauses after counting until the second one
// has counted too, or waits on a lock: without the lock both count two
// admins and both demote.
func TestSSORoleSyncKeepsAnAdminUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.New(t)
	s := NewService(st, auth.NewService(st, auth.NewHasher(auth.Argon2Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}, 1), nil), nil)
	const issuer = "https://id.example.com"
	login := func(subject string, groups ...string) SSOLogin {
		return SSOLogin{
			Identity:  auth.Identity{Issuer: issuer, Subject: subject},
			Usernames: []string{subject}, Groups: groups, GroupsKnown: true,
			AutoProvision: true, AdminGroups: []string{"admins"},
		}
	}
	for _, sub := range []string{"alice", "bob"} {
		out, err := s.SignInSSO(ctx, login(sub, "admins"), auth.ClientInfo{})
		if err != nil || out.User.Role != auth.RoleAdmin {
			t.Fatalf("provision %s: %v %+v", sub, err, out.User)
		}
	}

	var arrived atomic.Int32
	both := make(chan struct{})
	s.adminsCounted = func() {
		if arrived.Add(1) > 1 {
			close(both)
			return
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			select {
			case <-both:
				return
			default:
			}
			var waiting int
			if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
				t.Error(err)
				return
			}
			if waiting > 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Error("the other sign-in neither counted nor waited")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	var wg sync.WaitGroup
	for _, sub := range []string{"alice", "bob"} {
		wg.Go(func() {
			if _, err := s.SignInSSO(ctx, login(sub), auth.ClientInfo{}); err != nil {
				t.Errorf("sign in %s: %v", sub, err)
			}
		})
	}
	wg.Wait()
	n, err := st.CountEnabledAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("enabled admins after both sign-ins: %d, want 1", n)
	}
}
