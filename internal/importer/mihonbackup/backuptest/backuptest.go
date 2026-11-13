// SPDX-License-Identifier: GPL-3.0-or-later

// Package backuptest builds small synthetic backups in the Mihon format
// for tests: the fields the reader uses, with Mihon's field numbers. All
// data in tests is made up.
package backuptest

import (
	"bytes"
	"compress/gzip"
	"math"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Backup is a backup to encode.
type Backup struct {
	Series     []Series
	Categories []Category
	Sources    []Source
}

// Series is one series. InLibrary false writes favorite = false (a series
// kept only for its history, as Mihon does).
type Series struct {
	SourceID       int64
	URL            string
	Title          string
	Author, Artist string
	DateAdded      time.Time
	NotInLibrary   bool
	CategoryOrders []int64
	Chapters       []Chapter
	History        []History
}

// Chapter is one chapter with its reading state.
type Chapter struct {
	URL       string
	Name      string
	Number    float32
	Read      bool
	Bookmark  bool
	LastPage  int64
	DateFetch time.Time
}

// History is a last-read time for a chapter URL.
type History struct {
	URL      string
	LastRead time.Time
}

// Category is a library category.
type Category struct {
	Name  string
	Order int64
}

// Source names a source.
type Source struct {
	ID   int64
	Name string
}

type msg []byte

func (m msg) int(num protowire.Number, v int64) msg {
	m = protowire.AppendTag(m, num, protowire.VarintType)
	return protowire.AppendVarint(m, uint64(v)) //nolint:gosec // protobuf int64 encoding
}

func (m msg) bool(num protowire.Number, v bool) msg {
	m = protowire.AppendTag(m, num, protowire.VarintType)
	return protowire.AppendVarint(m, protowire.EncodeBool(v))
}

func (m msg) float(num protowire.Number, v float32) msg {
	m = protowire.AppendTag(m, num, protowire.Fixed32Type)
	return protowire.AppendFixed32(m, math.Float32bits(v))
}

func (m msg) str(num protowire.Number, s string) msg {
	m = protowire.AppendTag(m, num, protowire.BytesType)
	return protowire.AppendString(m, s)
}

func (m msg) msg(num protowire.Number, sub msg) msg {
	m = protowire.AppendTag(m, num, protowire.BytesType)
	return protowire.AppendBytes(m, sub)
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Protobuf returns the backup's uncompressed protobuf.
func (b Backup) Protobuf() []byte {
	var out msg
	for _, s := range b.Series {
		m := msg{}.int(1, s.SourceID).str(2, s.URL).str(3, s.Title)
		if s.Artist != "" {
			m = m.str(4, s.Artist)
		}
		if s.Author != "" {
			m = m.str(5, s.Author)
		}
		if !s.DateAdded.IsZero() {
			m = m.int(13, ms(s.DateAdded))
		}
		for _, c := range s.Chapters {
			cm := msg{}.str(1, c.URL).str(2, c.Name)
			if c.Read {
				cm = cm.bool(4, true)
			}
			if c.Bookmark {
				cm = cm.bool(5, true)
			}
			if c.LastPage > 0 {
				cm = cm.int(6, c.LastPage)
			}
			if !c.DateFetch.IsZero() {
				cm = cm.int(7, ms(c.DateFetch))
			}
			m = m.msg(16, cm.float(9, c.Number))
		}
		for _, o := range s.CategoryOrders {
			m = m.int(17, o)
		}
		if s.NotInLibrary {
			m = m.bool(100, false)
		}
		for _, h := range s.History {
			m = m.msg(104, msg{}.str(1, h.URL).int(2, ms(h.LastRead)))
		}
		out = out.msg(1, m)
	}
	for _, c := range b.Categories {
		out = out.msg(2, msg{}.str(1, c.Name).int(2, c.Order))
	}
	for _, s := range b.Sources {
		out = out.msg(101, msg{}.str(1, s.Name).int(2, s.ID))
	}
	return out
}

// File returns the backup as a .tachibk file: gzip'd protobuf.
func (b Backup) File() []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b.Protobuf())
	_ = zw.Close()
	return buf.Bytes()
}
