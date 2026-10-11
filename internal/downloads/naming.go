// SPDX-License-Identifier: GPL-3.0-or-later

package downloads

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultTemplate names downloaded chapters: one folder per work (the ID
// keeps two works with the same title apart and survives renames).
const DefaultTemplate = "{work} [{work_id}]/{volume} {chapter} - {title} [{group}].cbz"

// NameParts are what a file name is made of.
type NameParts struct {
	Work    string
	WorkID  string
	Volume  string
	Number  *float64
	Title   string
	Group   string
	Lang    string
	ExtraID string
}

func chapterLabel(n *float64, extraID string) string {
	if n == nil {
		return "Extra " + extraID
	}
	whole, frac := math.Modf(*n)
	s := fmt.Sprintf("Ch.%04d", int(whole))
	if frac != 0 {
		s += strings.TrimPrefix(strconv.FormatFloat(frac, 'f', -1, 64), "0")
	}
	return s
}

func volumeLabel(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if n, err := strconv.Atoi(v); err == nil {
		return fmt.Sprintf("Vol.%02d", n)
	}
	return "Vol." + v
}

// A title that only repeats the chapter number ("Chapter 12", "Ch. 12:")
// adds nothing to the name.
var genericTitle = regexp.MustCompile(`(?i)^\s*(chapter|ch\.?|episode|ep\.?|#)\s*[\d.,]+\s*[:\-–—.]?\s*`)

func cleanTitle(t string) string {
	return strings.TrimSpace(genericTitle.ReplaceAllString(t, ""))
}

// Render fills a template and makes every path segment safe for common
// filesystems (including SMB/NTFS shares). Empty parts leave no stray
// separators or brackets behind.
func Render(template string, p NameParts) string {
	if template == "" {
		template = DefaultTemplate
	}
	// A "/" inside a value must not open a folder.
	v := func(s string) string { return strings.NewReplacer("/", "-", "\\", "-").Replace(s) }
	r := strings.NewReplacer(
		"{work}", v(p.Work), "{work_id}", v(p.WorkID), "{volume}", v(volumeLabel(p.Volume)),
		"{chapter}", chapterLabel(p.Number, p.ExtraID), "{title}", v(cleanTitle(p.Title)),
		"{group}", v(p.Group), "{lang}", v(p.Lang),
	)
	out := r.Replace(template)
	segs := strings.Split(out, "/")
	for i, s := range segs {
		segs[i] = tidy(s)
	}
	return strings.Join(segs, "/")
}

var (
	emptyBrackets = regexp.MustCompile(`\[\s*\]|\(\s*\)`)
	danglingDash  = regexp.MustCompile(`\s+-\s*(\.cbz)?$|\s+-\s+-\s+`)
	spaces        = regexp.MustCompile(`\s+`)
)

// windowsReserved are names Windows (and SMB shares) refuse.
var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "LPT1": true, "LPT2": true, "LPT3": true}

const maxSegmentBytes = 180

func tidy(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return -1
		case strings.ContainsRune(`<>:"\|?*`, r):
			return ' '
		}
		return r
	}, s)
	s = emptyBrackets.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(s, " ")
	ext := ""
	if strings.HasSuffix(s, ".cbz") {
		ext, s = ".cbz", strings.TrimSuffix(s, ".cbz")
	}
	s = strings.TrimSpace(s)
	for {
		t := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "-"), "–"))
		if t == s {
			break
		}
		s = t
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "- "))
	s = danglingDash.ReplaceAllString(s, " - ")
	s = strings.TrimRight(s, ". ")
	for len(s) > maxSegmentBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	s = strings.TrimRight(s, ". ")
	if s == "" {
		s = "_"
	}
	if windowsReserved[strings.ToUpper(s)] {
		s += "_"
	}
	return s + ext
}
