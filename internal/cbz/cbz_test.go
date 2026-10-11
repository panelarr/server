// SPDX-License-Identifier: GPL-3.0-or-later

package cbz

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ch.cbz.tmp")
	w, err := Create(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct{ ct, body string }{{"image/jpeg", "one"}, {"image/png", "two"}, {"image/webp; q=1", "three"}} {
		if err := w.AddPage(page.ct, strings.NewReader(page.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.AddPage("text/html", strings.NewReader("x")); !errors.Is(err, ErrNotImage) {
		t.Fatalf("html page: %v", err)
	}
	res, err := w.Finish(ComicInfo{Series: "The Lantern Keeper", Number: "12.5", Manga: "YesAndRightToLeft", Notes: "panelarr:work=x chapter=y"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	sum := sha256.Sum256(raw)
	if res.Pages != 3 || res.Bytes != int64(len(raw)) || res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("result %+v (file %d bytes)", res, len(raw))
	}

	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if strings.HasSuffix(f.Name, ".jpg") && f.Method != zip.Store {
			t.Fatal("pages are compressed")
		}
		if f.Name == "ComicInfo.xml" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			for _, want := range []string{"<Series>The Lantern Keeper</Series>", "<Number>12.5</Number>", "<PageCount>3</PageCount>", "<Manga>YesAndRightToLeft</Manga>"} {
				if !strings.Contains(string(b), want) {
					t.Errorf("ComicInfo lacks %s:\n%s", want, b)
				}
			}
		}
	}
	if strings.Join(names, ",") != "0001.jpg,0002.png,0003.webp,ComicInfo.xml" {
		t.Fatalf("names %v", names)
	}

	f, _ := os.Open(p)
	defer f.Close()
	a, err := Open(f, res.Bytes)
	if err != nil || a.Pages() != 3 {
		t.Fatalf("open %v %d", err, a.Pages())
	}
	ct, rc, size, err := a.Page(1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if ct != "image/png" || string(b) != "two" || size != 3 {
		t.Fatalf("page 1: %s %q %d", ct, b, size)
	}
	if _, _, _, err := a.Page(3); err == nil {
		t.Fatal("page out of range")
	}
}

func TestAbort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.tmp")
	w, err := Create(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.AddPage("image/png", strings.NewReader("x"))
	w.Abort()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("aborted archive left behind")
	}
}
