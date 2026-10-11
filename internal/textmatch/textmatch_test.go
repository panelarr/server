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
