// SPDX-License-Identifier: GPL-3.0-or-later

package users

import (
	"regexp"
	"strings"
	"testing"
)

// The users table's check on usernames.
var usernameRule = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,63}$`)

func TestUsernameFrom(t *testing.T) {
	for in, want := range map[string]string{
		"alice":                   "alice",
		"Ann Smith!":              "AnnSmith",
		"_.-bob":                  "bob",
		"jürgen.müller":           "jrgen.mller",
		"a":                       "",
		"é":                       "",
		"--":                      "",
		"":                        "",
		"x9-sub":                  "x9-sub",
		"a@b":                     "ab",
		strings.Repeat("z", 100):  strings.Repeat("z", 58),
		"evil\x00name\r\n":        "evilname",
		"CN=admin,OU=people,DC=x": "CNadminOUpeopleDCx",
	} {
		got := usernameFrom(in)
		if got != want {
			t.Errorf("usernameFrom(%q) = %q, want %q", in, got, want)
		}
		// Even with the longest suffix tried, it fits the table's rule.
		if got != "" && !usernameRule.MatchString(got+"-1000") {
			t.Errorf("usernameFrom(%q) = %q breaks the username rule", in, got)
		}
	}
}

func TestCleanText(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"Ann Smith", 100, "Ann Smith"},
		{" Ann\x00 \x07Smith\n", 100, "Ann Smith"},
		{"bad \xff utf8", 100, "bad  utf8"},
		{"ééééé", 3, "ééé"},
	} {
		if got := cleanText(tc.in, tc.n); got != tc.want {
			t.Errorf("cleanText(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestInAny(t *testing.T) {
	if !inAny([]string{"a", "b"}, []string{"c", "b"}) || inAny([]string{"a"}, []string{"A"}) || inAny(nil, []string{"a"}) || inAny([]string{"a"}, nil) {
		t.Fatal("inAny")
	}
}
