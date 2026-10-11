// SPDX-License-Identifier: GPL-3.0-or-later

// Package textmatch compares titles and names across sources and the
// metadata catalogue: every site writes a title its own way (case, width,
// accents, punctuation, word order of names). It normalizes the same way
// the metadata service does, so both agree on what "the same title" is.
package textmatch

import (
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// diacritics are the combining marks of Latin, Greek and Cyrillic scripts.
// Marks of other scripts (such as kana voicing marks) change the letter
// and are kept.
var diacritics = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x0300, Hi: 0x036f, Stride: 1},
		{Lo: 0x1ab0, Hi: 0x1aff, Stride: 1},
		{Lo: 0x1dc0, Hi: 0x1dff, Stride: 1},
		{Lo: 0x20d0, Hi: 0x20ff, Stride: 1},
		{Lo: 0xfe20, Hi: 0xfe2f, Stride: 1},
	},
}

// Normalize folds compatibility forms (full-width Latin, half-width kana),
// removes diacritics, lowercases, drops apostrophes, turns other
// punctuation into spaces and collapses spaces.
func Normalize(s string) string {
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(diacritics)), norm.NFKC)
	folded, _, err := transform.String(t, s)
	if err != nil {
		folded = norm.NFKC.String(s)
	}
	var b strings.Builder
	b.Grow(len(folded))
	space := false
	for _, r := range strings.ToLower(folded) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		case r == '\'' || r == '’':
			// "Hell's" and "Hells" should match.
		default:
			space = true
		}
	}
	return b.String()
}

// Similarity scores how alike two titles are, from 0 to 1: 1 when they
// normalize the same; otherwise the best of an edit-distance ratio and a
// word-set ratio (which forgives reordered or extra words such as a
// subtitle on one side).
func Similarity(a, b string) float64 {
	na, nb := Normalize(a), Normalize(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	return max(editRatio(na, nb), tokenSetRatio(na, nb)*0.95)
}

// editRatio is 1 - levenshtein / longer length, over runes.
func editRatio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	n := max(len(ra), len(rb))
	if n == 0 {
		return 1
	}
	return 1 - float64(levenshtein(ra, rb))/float64(n)
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// tokenSetRatio compares the shared words with each side's whole word set:
// "The Lantern Keeper" and "Lantern Keeper (Official)" score high.
func tokenSetRatio(a, b string) float64 {
	ta, tb := uniqueWords(a), uniqueWords(b)
	var common []string
	for _, w := range ta {
		if slices.Contains(tb, w) {
			common = append(common, w)
		}
	}
	if len(common) == 0 {
		return 0
	}
	inter := strings.Join(common, " ")
	restA := strings.TrimSpace(inter + " " + strings.Join(without(ta, common), " "))
	restB := strings.TrimSpace(inter + " " + strings.Join(without(tb, common), " "))
	return max(editRatio(inter, restA), editRatio(inter, restB), editRatio(restA, restB))
}

// stopWords don't count on their own: they make unrelated titles look alike.
var stopWords = []string{"the", "a", "an", "of", "to", "no", "wa", "ga", "de", "la", "le", "der", "die", "das"}

func uniqueWords(s string) []string {
	var out []string
	for _, w := range strings.Fields(s) {
		if !slices.Contains(out, w) && !slices.Contains(stopWords, w) {
			out = append(out, w)
		}
	}
	slices.Sort(out)
	return out
}

func without(words, drop []string) []string {
	var out []string
	for _, w := range words {
		if !slices.Contains(drop, w) {
			out = append(out, w)
		}
	}
	return out
}

// Best returns the best similarity between any of as and any of bs, and
// the pair that scored it.
func Best(as, bs []string) (score float64, a, b string) {
	for _, x := range as {
		for _, y := range bs {
			if s := Similarity(x, y); s > score {
				score, a, b = s, x, y
			}
		}
	}
	return score, a, b
}

// SameName reports whether two person names are the same person as far as
// text tells: the same words in any order ("Akira Toriyama" and
// "Toriyama Akira"), or one written without spaces.
func SameName(a, b string) bool {
	na, nb := Normalize(a), Normalize(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb || strings.ReplaceAll(na, " ", "") == strings.ReplaceAll(nb, " ", "") {
		return true
	}
	wa, wb := strings.Fields(na), strings.Fields(nb)
	slices.Sort(wa)
	slices.Sort(wb)
	return slices.Equal(wa, wb)
}

// PeopleOverlap reports whether any name in as is the same person as any
// in bs.
func PeopleOverlap(as, bs []string) bool {
	for _, a := range as {
		for _, b := range bs {
			if SameName(a, b) {
				return true
			}
		}
	}
	return false
}
