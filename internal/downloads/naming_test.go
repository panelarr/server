// SPDX-License-Identifier: GPL-3.0-or-later

package downloads

import (
	"strings"
	"testing"
)

func n(f float64) *float64 { return &f }

func TestRender(t *testing.T) {
	cases := []struct {
		p    NameParts
		want string
	}{
		{NameParts{Work: "The Lantern Keeper", WorkID: "1a2b3c4d", Volume: "1", Number: n(12), Title: "The Storm", Group: "Team A"},
			"The Lantern Keeper [1a2b3c4d]/Vol.01 Ch.0012 - The Storm [Team A].cbz"},
		{NameParts{Work: "Re:Zero?", WorkID: "x", Number: n(12.5), Title: "Chapter 12.5"},
			"Re Zero [x]/Ch.0012.5.cbz"},
		{NameParts{Work: "A/B", WorkID: "x", Number: nil, ExtraID: "ab12", Title: "Beach Special"},
			"A-B [x]/Extra ab12 - Beach Special.cbz"},
		{NameParts{Work: "CON", WorkID: "", Number: n(1)},
			"CON_/Ch.0001.cbz"},
	}
	for _, c := range cases {
		if got := Render("", c.p); got != c.want {
			t.Errorf("Render(%+v)\n got %q\nwant %q", c.p, got, c.want)
		}
	}
	long := Render("{work}/{chapter}.cbz", NameParts{Work: strings.Repeat("ä", 200), Number: n(1)})
	if seg := strings.Split(long, "/")[0]; len(seg) > maxSegmentBytes {
		t.Fatalf("segment %d bytes", len(seg))
	}
	if got := Render("{work}/{chapter}.cbz", NameParts{Work: "con", Number: n(1)}); !strings.HasPrefix(got, "con_/") {
		t.Fatalf("reserved name: %q", got)
	}
}
