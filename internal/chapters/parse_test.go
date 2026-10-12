// SPDX-License-Identifier: GPL-3.0-or-later

package chapters

import "testing"

func TestParse(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name        string
		number, end *float64
		volume      string
		title       string
		extra       bool
	}{
		{"Vol. 1 Ch. 0 …", f(0), nil, "1", "", false},
		{"Vol. 1 Volume 1 Chapter 1: Double Dungeon", f(1), nil, "1", "Double Dungeon", false},
		{"Vol. 1 Ch. 1.1", f(1.1), nil, "1", "", false},
		{"Ch. 200 OFFICIAL TRANSLATION", f(200), nil, "", "", false},
		{"Chapter 12.5", f(12.5), nil, "", "", false},
		{"Chapter 12,5 - The Long Night", f(12.5), nil, "", "The Long Night", false},
		{"Ch.3", f(3), nil, "", "", false},
		{"Episode 12 - The Gate", f(12), nil, "", "The Gate", false},
		{"Ep. 7: Homecoming", f(7), nil, "", "Homecoming", false},
		{"#7", f(7), nil, "", "", false},
		{"Chapter 10-11", f(10), f(11), "", "", false},
		{"10-11", f(10), f(11), "", "", false},
		{"10.5", f(10.5), nil, "", "", false},
		{"Vol.02 Ch.015 [GroupName] Rain", f(15), nil, "2", "Rain", false},
		{"Volume 3 Chapter 21 (END)", f(21), nil, "3", "", false},
		{"v4 c? Interlude", nil, nil, "4", "c? Interlude", false},
		{"第3卷 第12话 雨の日", f(12), nil, "3", "雨の日", false},
		{"第1卷", nil, nil, "1", "", false},
		{"Part 3", f(3), nil, "", "", false},
		{"Chapter 5 Part 2", f(5), nil, "", "Part 2", false},
		{"Side Story 2: Summer", nil, nil, "", "Side Story 2: Summer", true},
		{"Special Chapter: New Year", nil, nil, "", "Special Chapter: New Year", true},
		{"Extra", nil, nil, "", "Extra", true},
		{"Notice", nil, nil, "", "Notice", true},
		{"Hiatus Announcement", nil, nil, "", "Hiatus Announcement", true},
		{"Vol. 2 Extra 1", nil, nil, "2", "Extra 1", true},
		{"Chapter 50: The Special One", f(50), nil, "", "The Special One", false},
		{"Ch. 80.1 SCANSLATION", f(80.1), nil, "", "", false},
		{"Ch. 81 Scanlation", f(81), nil, "", "", false},
		{"Chapter 12 [Fan Translation]", f(12), nil, "", "", false},
		{"Chapter 12 Fan Translation", f(12), nil, "", "", false},
		{"Chapter 13 TL", f(13), nil, "", "", false},
		{"Ch. 14 MTL", f(14), nil, "", "", false},
		{"Ch. 15 LQ", f(15), nil, "", "", false},
		{"Ch. 16 RAW", f(16), nil, "", "", false},
		{"Chapter 17: The Bridge (Night Owls)", f(17), nil, "", "The Bridge", false},
		{"Chapter 18 (Night Owls)", f(18), nil, "", "", false},
		{"Chapter 19: The Bridge (Part 1)", f(19), nil, "", "The Bridge (Part 1)", false},
		// A known limit: a title that starts or ends with a release word
		// loses it; inside a title it stays.
		{"Chapter 20: Lost in Translation", f(20), nil, "", "Lost in", false},
		{"Chapter 21: Translation Errors of the Heart", f(21), nil, "", "Errors of the Heart", false},
		{"Chapter 22: The Translation Office", f(22), nil, "", "The Translation Office", false},
		{"Ch. 40.4", f(40.4), nil, "", "", false},
		{"Ch. 179.5", f(179.5), nil, "", "", false},
		{"Prologue", nil, nil, "", "Prologue", false},
		{"", nil, nil, "", "", false},
	} {
		p := Parse(tc.name)
		same := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
		if !same(p.Number, tc.number) || !same(p.NumberEnd, tc.end) || p.Volume != tc.volume || p.Title != tc.title || p.Extra != tc.extra {
			t.Errorf("%q: number %v end %v volume %q title %q extra %v; want %v %v %q %q %v", tc.name,
				numOf(p.Number), numOf(p.NumberEnd), p.Volume, p.Title, p.Extra, numOf(tc.number), numOf(tc.end), tc.volume, tc.title, tc.extra)
		}
	}
}

func numOf(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func TestNumberAndVolumeOf(t *testing.T) {
	t.Parallel()
	five := 5.0
	// The source's number wins, except for extras.
	if n := NumberOf(&five, "Chapter 7"); n == nil || *n != 5 {
		t.Fatalf("source number %v", numOf(n))
	}
	if n := NumberOf(&five, "Side Story: Summer"); n != nil {
		t.Fatalf("an extra got number %v", *n)
	}
	if n := NumberOf(nil, "Vol. 1 Ch. 1.1"); n == nil || *n != 1.1 {
		t.Fatalf("from the name %v", numOf(n))
	}
	// A volume from the source (without leading zeros), else the name.
	for _, tc := range []struct{ volume, name, want string }{
		{"02", "Chapter 9", "2"},
		{"", "Vol. 1 Volume 1 Chapter 1: Double Dungeon", "1"},
		{"", "第4卷 第30话", "4"},
		{"Box Set", "Chapter 1", "Box Set"},
		{"", "Chapter 1", ""},
	} {
		if got := VolumeOf(tc.volume, tc.name); got != tc.want {
			t.Errorf("VolumeOf(%q, %q) = %q, want %q", tc.volume, tc.name, got, tc.want)
		}
	}
}
