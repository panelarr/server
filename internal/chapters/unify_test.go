// SPDX-License-Identifier: GPL-3.0-or-later

package chapters

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func f(v float64) *float64 { return &v }

func TestNumberOf(t *testing.T) {
	for _, c := range []struct {
		num  *float64
		name string
		want *float64
	}{
		{f(12), "whatever", f(12)},
		{f(12.5), "", f(12.5)},
		{f(-1), "Chapter 7.5: The Return", f(7.5)},
		{nil, "Ch. 3", f(3)},
		{nil, "Episode 10", f(10)},
		{nil, "第12話", f(12)},
		{nil, "Extra: Beach Day", nil},
		{f(-1), "Oneshot", nil},
		{f(1.0000001), "", f(1)},
	} {
		got := NumberOf(c.num, c.name)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("NumberOf(%v, %q) = %v, want %v", c.num, c.name, deref(got), deref(c.want))
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func v(source string, num *float64, name string) Variant {
	return Variant{ID: uuid.New(), SourceID: source, Number: num, Name: name, Health: "ok"}
}

func TestGroupVariants(t *testing.T) {
	vs := []Variant{
		v("a", f(2), "Chapter 2"), v("b", f(2), "Ch. 2"), v("a", f(1), "Chapter 1"),
		v("b", f(12.5), "Ch. 12.5"), v("b", nil, "Special"), v("a", f(-1), "Chapter 3"),
	}
	vs[0].Volume, vs[1].Volume = "1", ""
	groups := GroupVariants(vs)
	var nums []any
	for _, g := range groups {
		nums = append(nums, deref(g.Number))
	}
	if len(groups) != 5 || nums[0] != 1.0 || nums[1] != 2.0 || nums[2] != 3.0 || nums[3] != 12.5 || nums[4] != nil {
		t.Fatalf("groups %v", nums)
	}
	if len(groups[1].Variants) != 2 || groups[1].Volume() != "1" {
		t.Fatalf("chapter 2: %+v", groups[1])
	}
	if groups[4].ExtraKey == "" {
		t.Fatal("extra without key")
	}
}

func TestChoose(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)
	a := Variant{ID: uuid.New(), SourceID: "a", Lang: "en", Health: "ok", Priority: 0, UploadedAt: &older}
	b := Variant{ID: uuid.New(), SourceID: "b", Lang: "en", Health: "ok", Priority: 0, UploadedAt: &now}
	g := Group{Variants: []Variant{a, b}}

	if c := Choose(Group{Variants: []Variant{a}}, nil, nil); c.Reason != ReasonOnlySource || c.Variant.ID != a.ID {
		t.Fatalf("only %+v", c)
	}
	if c := Choose(g, nil, nil); c.Variant.ID != b.ID || c.Reason != ReasonNewest {
		t.Fatalf("newest %+v", c)
	}
	a.Priority = 5
	g = Group{Variants: []Variant{a, b}}
	if c := Choose(g, nil, nil); c.Variant.ID != a.ID || c.Reason != ReasonPriority {
		t.Fatalf("priority %+v", c)
	}
	a.Health = "failing"
	g = Group{Variants: []Variant{a, b}}
	if c := Choose(g, nil, nil); c.Variant.ID != b.ID || c.Reason != ReasonHealthier {
		t.Fatalf("healthier %+v", c)
	}
	b.Lang = "de"
	g = Group{Variants: []Variant{a, b}}
	if c := Choose(g, nil, []string{"de", "en"}); c.Variant.ID != b.ID || c.Reason != ReasonLanguage {
		t.Fatalf("language %+v", c)
	}
	// A failing source still serves when it's the only one with the chapter.
	if c := Choose(Group{Variants: []Variant{a}}, nil, nil); c.Variant.ID != a.ID {
		t.Fatalf("failing only %+v", c)
	}
	pin := a.ID
	if c := Choose(g, &pin, []string{"de"}); c.Variant.ID != a.ID || c.Reason != ReasonPinned {
		t.Fatalf("pinned %+v", c)
	}
	gone := uuid.New()
	if c := Choose(g, &gone, nil); c.Reason == ReasonPinned {
		t.Fatal("a vanished pin still wins")
	}

	// An unusable source gives way to any usable one, before language and
	// pins; it still serves when nothing else has the chapter.
	b.Unusable = true
	g = Group{Variants: []Variant{a, b}}
	if c := Choose(g, nil, []string{"de"}); c.Variant.ID != a.ID || c.Reason != ReasonHealthier {
		t.Fatalf("unusable preferred language %+v", c)
	}
	pinB := b.ID
	if c := Choose(g, &pinB, nil); c.Variant.ID != a.ID || c.Reason == ReasonPinned {
		t.Fatalf("unusable pin %+v", c)
	}
	if c := Choose(Group{Variants: []Variant{b}}, nil, nil); c.Variant.ID != b.ID {
		t.Fatalf("unusable only %+v", c)
	}
	a.Unusable = true
	g = Group{Variants: []Variant{a, b}}
	if c := Choose(g, &pinB, nil); c.Variant.ID != b.ID || c.Reason != ReasonPinned {
		t.Fatalf("pin among unusable %+v", c)
	}
}
