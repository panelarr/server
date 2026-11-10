// SPDX-License-Identifier: GPL-3.0-or-later

package textmatch

import "testing"

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Hello, World!":          "hello world",
		"  Pokémon   Adventure ": "pokemon adventure",
		"ＦＵＬＬ　ＷＩＤＴＨ":             "full width",
		"ﾊﾞｸﾏﾝ":                  "バクマン",
		"Hell's Kitchen":         "hells kitchen",
		"Re:Zero − Starting":     "re zero starting",
		"":                       "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	cases := []struct {
		a, b     string
		min, max float64
	}{
		{"The Lantern Keeper", "the lantern keeper!", 1, 1},
		{"Lantern Keeper", "The Lantern Keeper", 0.9, 1},
		{"The Lantern Keeper (Official)", "The Lantern Keeper", 0.85, 1},
		{"Salt and Iron", "Salt & Iron", 0.7, 1},
		{"The Lantern Keeper", "Paper Cranes", 0, 0.4},
		{"The Lantern Keeper", "The Lantern Keeper's Daughter", 0.6, 0.95},
		{"", "x", 0, 0},
	}
	for _, c := range cases {
		if s := Similarity(c.a, c.b); s < c.min || s > c.max {
			t.Errorf("Similarity(%q, %q) = %.2f, want %.2f..%.2f", c.a, c.b, s, c.min, c.max)
		}
	}
	score, a, b := Best([]string{"Toudai Mori", "The Lantern Keeper"}, []string{"Lighthouse", "the lantern keeper"})
	if score != 1 || a != "The Lantern Keeper" || b != "the lantern keeper" {
		t.Fatalf("Best = %v %q %q", score, a, b)
	}
}

func TestNames(t *testing.T) {
	if !SameName("Akira Ishida", "ISHIDA Akira") || !SameName("Ishida Akira", "IshidaAkira") || SameName("Akira Ishida", "Akira Ishikawa") {
		t.Fatal("SameName")
	}
	if !PeopleOverlap([]string{"X", "A. Author"}, []string{"a author"}) || PeopleOverlap(nil, []string{"x"}) {
		t.Fatal("PeopleOverlap")
	}
}

func TestPlaceholderNames(t *testing.T) {
	// Sites write these where they don't know the person: two series both
	// credited to "Unknown" share no one.
	for _, n := range []string{"Unknown", "UNKNOWN", "unknown author", "N/A", "n/a", "Updating", "Various Artists", "Anonymous", "-", "?", "", "不明"} {
		if !Placeholder(n) {
			t.Errorf("Placeholder(%q) = false", n)
		}
		if SameName(n, n) {
			t.Errorf("SameName(%q, %q) = true", n, n)
		}
	}
	for _, n := range []string{"A. Author", "Na Ri", "Unknown Soldier Kim"} {
		if Placeholder(n) {
			t.Errorf("Placeholder(%q) = true", n)
		}
	}
	if PeopleOverlap([]string{"Unknown", "Some Writer"}, []string{"unknown", "N/A"}) {
		t.Fatal("placeholders overlap")
	}
	if !PeopleOverlap([]string{"Unknown", "Some Writer"}, []string{"unknown", "WRITER Some"}) {
		t.Fatal("a real name next to a placeholder doesn't overlap")
	}
	if got := RealNames([]string{"Unknown", "Some Writer", "N/A"}); len(got) != 1 || got[0] != "Some Writer" {
		t.Fatalf("RealNames = %q", got)
	}
}
