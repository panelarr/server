// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenWithGeneratedKeyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sub", "secret.key")
	b := NewBox("", file)
	sealed, err := b.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "enc:v1:") || strings.Contains(sealed, "hunter2") {
		t.Fatalf("sealed: %q", sealed)
	}
	st, err := os.Stat(file)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	// A new box with the same file opens it.
	plain, err := NewBox("", file).Open(sealed)
	if err != nil || plain != "hunter2" {
		t.Fatalf("open: %q %v", plain, err)
	}
	// Another key can't.
	other := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if _, err := NewBox(other, "").Open(sealed); err == nil {
		t.Fatal("opened with another key")
	}
}

func TestEnvKey(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	b := NewBox(key, "")
	s, err := b.Seal("x")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := b.Open(s); err != nil || p != "x" {
		t.Fatalf("%q %v", p, err)
	}
	if _, err := NewBox("short", "").Seal("x"); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := NewBox("", "").Seal("x"); err == nil {
		t.Fatal("no key accepted")
	}
	if _, err := b.Open("plain"); err == nil {
		t.Fatal("unsealed value opened")
	}
}
