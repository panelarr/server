// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestAuthorizationMatrix calls every operation in the spec as an
// anonymous caller, a user, a user's read-only API key and an admin's key
// without the admin scope, and checks each is refused exactly as its
// policy says. Authorization runs before validation, so an empty body is
// enough. New endpoints are covered automatically.
func TestAuthorizationMatrix(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)
	anon := h.client()
	userRead := h.keyClient(user.newAPIKey("read"))
	userWrite := h.keyClient(user.newAPIKey("read", "write"))
	adminNoAdminScope := h.keyClient(admin.newAPIKey("read", "write"))

	pols, err := loadPolicies()
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(pols))
	for k := range pols {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type want struct {
		status int
		code   string
	}
	ok := want{} // anything but an authorization refusal
	for _, k := range keys {
		pol := pols[k]
		method := pol.Route.Method
		path := BasePath + pathParam.ReplaceAllString(pol.Route.Path, "00000000-0000-0000-0000-00000000abcd")
		safe := safeMethod(method)

		var anonWant, userWant, readKeyWant, writeKeyWant, adminKeyWant want
		switch pol.Access {
		case AccessPublic:
			anonWant, userWant, readKeyWant, writeKeyWant, adminKeyWant = ok, ok, ok, ok, ok
		case AccessUser:
			anonWant = want{401, "unauthenticated"}
			userWant, writeKeyWant, adminKeyWant = ok, ok, ok
			readKeyWant = ok
			if !safe && !pol.ReadOnly {
				readKeyWant = want{403, "insufficient_scope"}
			}
		case AccessAdmin:
			anonWant = want{401, "unauthenticated"}
			userWant = want{403, "forbidden"}
			readKeyWant, writeKeyWant = want{403, "forbidden"}, want{403, "forbidden"}
			adminKeyWant = want{403, "insufficient_scope"}
		}
		if pol.SessionOnly {
			readKeyWant, writeKeyWant, adminKeyWant = want{403, "session_required"}, want{403, "session_required"}, want{403, "session_required"}
			if pol.Access == AccessAdmin {
				readKeyWant, writeKeyWant = want{403, "forbidden"}, want{403, "forbidden"}
			}
		}

		for _, tc := range []struct {
			who  string
			c    *client
			want want
		}{
			{"anonymous", anon, anonWant},
			{"user session", user, userWant},
			{"user read key", userRead, readKeyWant},
			{"user write key", userWrite, writeKeyWant},
			{"admin key without admin scope", adminNoAdminScope, adminKeyWant},
		} {
			// Logging out would end the shared session; its refusals are
			// covered by the anonymous and key cases.
			if strings.EqualFold(pol.OperationID, "logout") && tc.c == user {
				continue
			}
			res := tc.c.do(method, path, "{}")
			if tc.want == ok {
				if res.status == 401 || (res.status == 403 && isAuthzCode(res.problem(t).Code)) {
					t.Errorf("%s %s as %s: refused (%d %s)", method, path, tc.who, res.status, res.body)
				}
				continue
			}
			if res.status != tc.want.status || res.problem(t).Code != tc.want.code {
				t.Errorf("%s %s as %s: got %d %s, want %d %s", method, path, tc.who, res.status, res.body, tc.want.status, tc.want.code)
			}
		}
	}
}

func isAuthzCode(code string) bool {
	switch code {
	case "unauthenticated", "forbidden", "insufficient_scope", "session_required", "csrf_failed", "invalid_api_key":
		return true
	}
	return false
}

// TestPublicOperationsAreFew pins the list of operations that need no
// authentication, so making one public is a deliberate change.
func TestPublicOperationsAreFew(t *testing.T) {
	pols, err := loadPolicies()
	if err != nil {
		t.Fatal(err)
	}
	var public []string
	for _, p := range pols {
		if p.Access == AccessPublic {
			public = append(public, strings.ToLower(p.OperationID[:1])+p.OperationID[1:])
		}
	}
	sort.Strings(public)
	want := []string{"acceptInvite", "checkInvite", "createSetupAdmin", "getSetup", "getSystemInfo", "login"}
	if len(public) != len(want) {
		t.Fatalf("public operations %v, want %v", public, want)
	}
	for i := range want {
		if public[i] != want[i] {
			t.Fatalf("public operations %v, want %v", public, want)
		}
	}
}
