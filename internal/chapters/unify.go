// SPDX-License-Identifier: GPL-3.0-or-later

package chapters

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Variant is one source's copy of a chapter.
type Variant struct {
	ID         uuid.UUID
	SeriesID   uuid.UUID
	SourceID   string
	Key        string
	Name       string
	Number     *float64
	Volume     string
	Scanlator  string
	Lang       string
	UploadedAt *time.Time
	Position   int32
	// Priority is the series' priority for the work.
	Priority int32
	// Health is the source's: ok, failing or unknown.
	Health string
}

// Group is one canonical chapter: a number (or, for chapters without one,
// the single variant it came from) and its variants.
type Group struct {
	Number   *float64
	ExtraKey string
	Variants []Variant
}

// Reasons a variant was chosen.
const (
	ReasonPinned      = "pinned"
	ReasonOnlySource  = "only_source"
	ReasonLanguage    = "language"
	ReasonHealthier   = "healthier_source"
	ReasonPriority    = "priority"
	ReasonNewest      = "newest_upload"
	ReasonFirstListed = "first_listed"
)

var numberInName = regexp.MustCompile(`(?i)(?:\bch(?:apter|\.)?|\bep(?:isode|\.)?|#|第)\s*(\d+(?:[.,]\d+)?)`)

// roundNumber keeps three decimals, so 12.5 from two sources is one key.
func roundNumber(f float64) float64 { return math.Round(f*1000) / 1000 }

// NumberOf is a variant's chapter number: the source's, or one read from
// its name ("Chapter 12.5", "Ch. 3", "#7"). Negative numbers mean unknown.
func NumberOf(number *float64, name string) *float64 {
	if number != nil && *number >= 0 && !math.IsNaN(*number) && !math.IsInf(*number, 0) {
		n := roundNumber(*number)
		return &n
	}
	if m := numberInName.FindStringSubmatch(name); m != nil {
		if f, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64); err == nil {
			n := roundNumber(f)
			return &n
		}
	}
	return nil
}

// GroupVariants groups variants into canonical chapters by number; each
// variant without a number is a chapter of its own. Groups come in number
// order, extras last.
func GroupVariants(vs []Variant) []Group {
	byNumber := map[float64]*Group{}
	var groups []*Group
	for _, v := range vs {
		n := NumberOf(v.Number, v.Name)
		if n == nil {
			groups = append(groups, &Group{ExtraKey: "v:" + v.ID.String(), Variants: []Variant{v}})
			continue
		}
		g, ok := byNumber[*n]
		if !ok {
			num := *n
			g = &Group{Number: &num}
			byNumber[*n] = g
			groups = append(groups, g)
		}
		g.Variants = append(g.Variants, v)
	}
	slices.SortStableFunc(groups, func(a, b *Group) int {
		switch {
		case a.Number == nil && b.Number == nil:
			return strings.Compare(a.ExtraKey, b.ExtraKey)
		case a.Number == nil:
			return 1
		case b.Number == nil:
			return -1
		case *a.Number < *b.Number:
			return -1
		case *a.Number > *b.Number:
			return 1
		}
		return 0
	})
	out := make([]Group, len(groups))
	for i, g := range groups {
		out[i] = *g
	}
	return out
}

// Volume is the volume most variants give (empty when none does).
func (g Group) Volume() string {
	counts := map[string]int{}
	best, n := "", 0
	for _, v := range g.Variants {
		vol := strings.TrimSpace(v.Volume)
		if vol == "" {
			continue
		}
		counts[vol]++
		if counts[vol] > n || (counts[vol] == n && vol < best) {
			best, n = vol, counts[vol]
		}
	}
	return best
}

// Choice is the variant chosen for a chapter and why.
type Choice struct {
	Variant Variant
	Reason  string
}

// Choose picks a group's preferred variant: a pinned one that still
// exists; else by preferred language (languages, in order; others after),
// a healthy source over a failing one, the series' priority, the newest
// upload, then the source's own order. The reason is the first of these
// that set the winner apart from the runner-up.
func Choose(g Group, pinned *uuid.UUID, languages []string) Choice {
	if pinned != nil {
		for _, v := range g.Variants {
			if v.ID == *pinned {
				return Choice{Variant: v, Reason: ReasonPinned}
			}
		}
	}
	if len(g.Variants) == 1 {
		return Choice{Variant: g.Variants[0], Reason: ReasonOnlySource}
	}
	langRank := func(v Variant) int {
		for i, l := range languages {
			if strings.EqualFold(v.Lang, l) || strings.EqualFold(strings.SplitN(v.Lang, "-", 2)[0], l) {
				return i
			}
		}
		return len(languages)
	}
	healthRank := func(v Variant) int {
		if v.Health == "failing" {
			return 1
		}
		return 0
	}
	uploaded := func(v Variant) int64 {
		if v.UploadedAt == nil {
			return 0
		}
		return v.UploadedAt.Unix()
	}
	type criterion struct {
		reason string
		cmp    func(a, b Variant) int
	}
	criteria := []criterion{
		{ReasonLanguage, func(a, b Variant) int { return langRank(a) - langRank(b) }},
		{ReasonHealthier, func(a, b Variant) int { return healthRank(a) - healthRank(b) }},
		{ReasonPriority, func(a, b Variant) int { return int(b.Priority) - int(a.Priority) }},
		{ReasonNewest, func(a, b Variant) int {
			switch ua, ub := uploaded(a), uploaded(b); {
			case ua > ub:
				return -1
			case ua < ub:
				return 1
			}
			return 0
		}},
		{ReasonFirstListed, func(a, b Variant) int {
			if c := strings.Compare(a.SourceID, b.SourceID); c != 0 {
				return c
			}
			return int(a.Position) - int(b.Position)
		}},
	}
	vs := slices.Clone(g.Variants)
	slices.SortStableFunc(vs, func(a, b Variant) int {
		for _, c := range criteria {
			if d := c.cmp(a, b); d != 0 {
				return d
			}
		}
		return 0
	})
	reason := ReasonFirstListed
	for _, c := range criteria {
		if c.cmp(vs[0], vs[1]) != 0 {
			reason = c.reason
			break
		}
	}
	return Choice{Variant: vs[0], Reason: reason}
}
