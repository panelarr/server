// SPDX-License-Identifier: GPL-3.0-or-later

package chapters

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// What a chapter's name says, as sources write it: "Vol. 1 Ch. 0",
// "Vol. 1 Volume 1 Chapter 1: Double Dungeon", "Ch. 200 OFFICIAL
// TRANSLATION", "Episode 12 - The Gate", "#7", "第3卷 第12话", "10-11",
// "Side Story 2", … The number and volume feed unification; the clean
// title is what the app shows.

// Parsed is what a chapter's name says.
type Parsed struct {
	// Number is the chapter number, or nil; NumberEnd ends a range
	// ("Chapter 10-11").
	Number, NumberEnd *float64
	// Volume, without leading zeros; empty when the name doesn't say.
	Volume string
	// Title is the chapter's own name with number, volume and scanlator
	// noise taken out; empty when there's nothing else ("Chapter 12").
	Title string
	// Extra is a side story, special, extra, notice or hiatus: not part of
	// the numbered run, even if it carries a number.
	Extra bool
}

const num = `(\d+(?:[.,]\d+)?)`

var (
	volumeRe = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:^|[^\pL])(?:vol(?:ume)?\.?|v\.?)\s*` + num + `\b`),
		regexp.MustCompile(`第\s*` + num + `\s*[卷巻]`),
	}
	// Chapter markers, most specific first; a range may follow.
	chapterRe = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:^|[^\pL])(?:ch(?:apter|ap|\.)?|ep(?:isode|\.)?|#|no\.)\s*` + num + `(?:\s*[-–~]\s*` + num + `)?`),
		regexp.MustCompile(`第\s*` + num + `\s*[话話章回]`),
		regexp.MustCompile(`(?i)(?:^|[^\pL])part\s*` + num + `\b`),
	}
	// A name that is only a number or a range.
	bareRe = regexp.MustCompile(`^\s*` + num + `(?:\s*[-–~]\s*` + num + `)?\s*$`)
	// Names starting with these (after the volume) are extras.
	extraRe = regexp.MustCompile(`(?i)^\W*(?:extra|special|side\s*story|bonus|omake|notice|hiatus|announcement|q\s*&\s*a)\b`)
	// Bracketed parts of a name that only say who made the scan.
	bracketRe = regexp.MustCompile(`\[[^\]]*\]`)
	// Words around a title that aren't the title.
	noiseRe = regexp.MustCompile(`(?i)\b(?:official\s+translation|official|raw|hq|scan(?:lation)?s?|end)\b`)
)

func parseNum(s string) *float64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil {
		return nil
	}
	f = roundNumber(f)
	return &f
}

func cleanVolume(s string) string {
	s = strings.TrimLeft(strings.ReplaceAll(s, ",", "."), "0")
	if s == "" || s[0] == '.' {
		s = "0" + s
	}
	return s
}

// cut removes a match from s, leaving a space so words don't join.
func cut(s string, loc []int) string { return s[:loc[0]] + " " + s[loc[1]:] }

// Parse reads a chapter's name.
func Parse(name string) Parsed {
	var p Parsed
	rest := name
	for _, re := range volumeRe {
		for {
			m := re.FindStringSubmatchIndex(rest)
			if m == nil {
				break
			}
			if p.Volume == "" {
				p.Volume = cleanVolume(rest[m[2]:m[3]])
			}
			rest = cut(rest, m[:2])
		}
	}
	p.Extra = extraRe.MatchString(rest)
	for _, re := range chapterRe {
		m := re.FindStringSubmatchIndex(rest)
		if m == nil {
			continue
		}
		p.Number = parseNum(rest[m[2]:m[3]])
		if len(m) > 4 && m[4] >= 0 {
			p.NumberEnd = parseNum(rest[m[4]:m[5]])
		}
		rest = cut(rest, m[:2])
		break
	}
	if p.Number == nil {
		if m := bareRe.FindStringSubmatch(rest); m != nil {
			p.Number = parseNum(m[1])
			if m[2] != "" {
				p.NumberEnd = parseNum(m[2])
			}
			rest = ""
		}
	}
	p.Title = cleanTitle(rest)
	return p
}

// cleanTitle takes scanlator noise and separators off what's left of a
// name.
func cleanTitle(s string) string {
	s = bracketRe.ReplaceAllString(s, " ")
	s = noiseRe.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(":-–—|.,·•()~_", r)
	})
	// Nothing but punctuation (e.g. "…") is no title.
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
		return ""
	}
	return s
}
