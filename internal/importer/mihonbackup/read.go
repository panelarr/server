// SPDX-License-Identifier: GPL-3.0-or-later

// Package mihonbackup reads backups in Mihon's format (.tachibk), which
// Suwayomi writes too: protobuf, usually gzip-compressed, with Mihon's field
// numbers. Fields this package doesn't use, including Suwayomi's own fields
// (numbered 9000 and up), are skipped. See docs/import.md for the fields read.
//
// The reader streams the file: it holds one top-level message at a time
// (a series, a category or a source) plus the model it builds. Limits caps
// the input and every collection and byte of text the model keeps, so an
// upload's memory is bounded however the file is crafted.
package mihonbackup

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Errors Read returns, wrapped with details. Any other error comes from the
// reader the backup was read from.
var (
	// ErrNotBackup: the input isn't a backup in Mihon's format, or is damaged.
	ErrNotBackup = errors.New("not a Mihon or Suwayomi backup")
	// ErrUnsupported: a backup in a format this reader doesn't handle (the
	// legacy JSON backups).
	ErrUnsupported = errors.New("unsupported backup format")
	// ErrTooLarge: the input is over one of the Limits.
	ErrTooLarge = errors.New("backup too large")
	// ErrEmpty: a valid backup with no series in the library.
	ErrEmpty = errors.New("backup has no series in the library")
)

// Default limits. A large library (a thousand or so series with a few
// hundred chapters each) is a few MiB compressed and well under every count
// here; the file limit matches the import upload's body limit. With all of
// them at their defaults the model takes at most about 320 MiB, and Read
// about 400 MiB at its peak (see docs/import.md), whatever the file holds.
const (
	DefaultMaxFileSize     = 64 << 20  // bytes as uploaded
	DefaultMaxDataSize     = 256 << 20 // bytes after decompression
	DefaultMaxMessageSize  = 16 << 20  // one series, category or source
	DefaultMaxSeries       = 50_000
	DefaultMaxChapters     = 1_000_000 // over all series
	DefaultMaxHistory      = 1_000_000 // history entries over all series
	DefaultMaxCategoryRefs = 500_000   // category orders over all series
	DefaultMaxTrackerLinks = 200_000   // tracking entries over all series
	DefaultMaxCategories   = 1_000
	DefaultMaxSources      = 5_000
	DefaultMaxText         = 128 << 20 // bytes of text kept in the model
)

// Limits bound what Read accepts, and so the memory it uses. Every
// collection the model keeps has a cap; going over any of them fails with
// ErrTooLarge. Zero fields take the defaults.
type Limits struct {
	MaxFileSize    int64 // the file as read
	MaxDataSize    int64 // the protobuf after decompression
	MaxMessageSize int64 // one top-level message, which is buffered whole
	MaxSeries      int   // series, in the library or not
	MaxChapters    int   // chapters over all series
	// MaxHistory caps history entries over all series.
	MaxHistory int
	// MaxCategoryRefs caps the category orders series list, over all series
	// (repeats included).
	MaxCategoryRefs int
	// MaxTrackerLinks caps tracking entries over all series.
	MaxTrackerLinks int
	MaxCategories   int // top-level categories
	MaxSources      int // top-level source names
	// MaxText caps the bytes of text (titles, URLs, names, …) the model
	// keeps, after invalid UTF-8 is replaced.
	MaxText int64
}

func (l Limits) withDefaults() Limits {
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = DefaultMaxFileSize
	}
	if l.MaxDataSize <= 0 {
		l.MaxDataSize = DefaultMaxDataSize
	}
	if l.MaxMessageSize <= 0 {
		l.MaxMessageSize = DefaultMaxMessageSize
	}
	if l.MaxSeries <= 0 {
		l.MaxSeries = DefaultMaxSeries
	}
	if l.MaxChapters <= 0 {
		l.MaxChapters = DefaultMaxChapters
	}
	if l.MaxHistory <= 0 {
		l.MaxHistory = DefaultMaxHistory
	}
	if l.MaxCategoryRefs <= 0 {
		l.MaxCategoryRefs = DefaultMaxCategoryRefs
	}
	if l.MaxTrackerLinks <= 0 {
		l.MaxTrackerLinks = DefaultMaxTrackerLinks
	}
	if l.MaxCategories <= 0 {
		l.MaxCategories = DefaultMaxCategories
	}
	if l.MaxSources <= 0 {
		l.MaxSources = DefaultMaxSources
	}
	if l.MaxText <= 0 {
		l.MaxText = DefaultMaxText
	}
	return l
}

// Field numbers of Mihon's backup format (Backup, BackupManga, …).
const (
	backupManga      = 1
	backupCategories = 2
	backupSources    = 101

	mangaSource    = 1
	mangaURL       = 2
	mangaTitle     = 3
	mangaArtist    = 4
	mangaAuthor    = 5
	mangaStatus    = 8
	mangaDateAdded = 13
	mangaChapters  = 16
	mangaCategory  = 17
	mangaTracking  = 18
	mangaFavorite  = 100
	mangaHistory   = 104

	chapterURL        = 1
	chapterName       = 2
	chapterScanlator  = 3
	chapterRead       = 4
	chapterBookmark   = 5
	chapterLastPage   = 6
	chapterDateFetch  = 7
	chapterDateUpload = 8
	chapterNumber     = 9

	historyURL      = 1
	historyLastRead = 2

	categoryName  = 1
	categoryOrder = 2

	trackingSyncID = 1

	sourceName = 1
	sourceID   = 2
)

// Read parses a backup: gzip-compressed (as Mihon and Suwayomi write it) or
// plain protobuf (which Mihon also restores).
func Read(r io.Reader, lim Limits) (*Backup, error) {
	lim = lim.withDefaults()
	file := &capReader{r: r, max: lim.MaxFileSize, what: "file", source: true}
	b, err := read(file, lim)
	if err != nil {
		// An error of the reader itself (a dropped upload) is not the
		// file's fault: report it as it is.
		if file.err != nil {
			return nil, fmt.Errorf("reading backup: %w", file.err)
		}
		if !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrEmpty) && !errors.Is(err, ErrNotBackup) {
			err = fmt.Errorf("%w: %w", ErrNotBackup, err)
		}
		return nil, err
	}
	return b, nil
}

func read(file io.Reader, lim Limits) (*Backup, error) {
	in := bufio.NewReader(file)
	magic, err := in.Peek(2)
	if err != nil {
		if errors.Is(err, io.EOF) && len(magic) == 0 {
			return nil, fmt.Errorf("%w: the file is empty", ErrNotBackup)
		}
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: the file is too short to be a backup", ErrNotBackup)
		}
		return nil, err
	}
	var data io.Reader = in
	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b:
		zr, err := gzip.NewReader(in)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", noEOF(err))
		}
		data = zr
	case magic[0] == '{':
		return nil, fmt.Errorf("%w: legacy JSON backups can't be read; make a new backup", ErrUnsupported)
	}
	data = &capReader{r: data, max: lim.MaxDataSize, what: "decompressed data"}
	d := &decoder{lim: lim, out: &Backup{}}
	if err := d.decode(bufio.NewReader(data)); err != nil {
		return nil, err
	}
	return d.finish()
}

// capReader fails with ErrTooLarge once more than max bytes were read. On
// the caller's reader (source), it remembers that reader's own error.
type capReader struct {
	r      io.Reader
	n      int64
	max    int64
	what   string
	source bool
	err    error
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n > c.max {
		return 0, c.tooLarge()
	}
	if rest := c.max - c.n + 1; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return n, c.tooLarge()
	}
	if err != nil && !errors.Is(err, io.EOF) && c.source {
		c.err = err
	}
	return n, err
}

func (c *capReader) tooLarge() error {
	return fmt.Errorf("%w: %s over %s", ErrTooLarge, c.what, size(c.max))
}

// decoder counts what the model keeps over all messages against the Limits.
type decoder struct {
	lim          Limits
	out          *Backup
	chapters     int
	history      int
	categoryRefs int
	trackers     int
	text         int64
	buf          []byte
}

// keepText counts n more bytes of text kept in the model.
func (d *decoder) keepText(n int) error {
	d.text += int64(n)
	if d.text > d.lim.MaxText {
		return fmt.Errorf("%w: over %s of text", ErrTooLarge, size(d.lim.MaxText))
	}
	return nil
}

// decode reads the top-level Backup message field by field, holding one
// field's value at a time.
func (d *decoder) decode(in *bufio.Reader) error {
	for {
		tag, err := binary.ReadUvarint(in)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return noEOF(err)
		}
		num, typ := protowire.Number(tag>>3), protowire.Type(tag&7)
		if tag>>3 > uint64(protowire.MaxValidNumber) || !num.IsValid() {
			return fmt.Errorf("invalid field number %d", tag>>3)
		}
		switch typ {
		case protowire.VarintType:
			_, err = binary.ReadUvarint(in)
		case protowire.Fixed32Type:
			_, err = in.Discard(4)
		case protowire.Fixed64Type:
			_, err = in.Discard(8)
		case protowire.BytesType:
			err = d.field(in, num)
		default:
			return fmt.Errorf("field %d has unsupported wire type %d", num, typ)
		}
		if err != nil {
			return noEOF(err)
		}
	}
}

func (d *decoder) field(in *bufio.Reader, num protowire.Number) error {
	n, err := binary.ReadUvarint(in)
	if err != nil {
		return err
	}
	if num != backupManga && num != backupCategories && num != backupSources {
		if n > uint64(d.lim.MaxDataSize) {
			return fmt.Errorf("%w: field %d is %d bytes", ErrTooLarge, num, n)
		}
		_, err := io.CopyN(io.Discard, in, int64(n))
		return err
	}
	if n > uint64(d.lim.MaxMessageSize) {
		return fmt.Errorf("%w: a message of %d bytes is over %s", ErrTooLarge, n, size(d.lim.MaxMessageSize))
	}
	d.buf = slices.Grow(d.buf[:0], int(n))[:n]
	if _, err := io.ReadFull(in, d.buf); err != nil {
		return err
	}
	switch num {
	case backupManga:
		if len(d.out.Series) >= d.lim.MaxSeries {
			return fmt.Errorf("%w: over %d series", ErrTooLarge, d.lim.MaxSeries)
		}
		s, err := d.series(d.buf)
		if err == nil {
			err = d.keepText(s.textSize())
		}
		if err != nil {
			return fmt.Errorf("series %d: %w", len(d.out.Series)+1, err)
		}
		d.out.Series = append(d.out.Series, s)
	case backupCategories:
		if len(d.out.Categories) >= d.lim.MaxCategories {
			return fmt.Errorf("%w: over %d categories", ErrTooLarge, d.lim.MaxCategories)
		}
		c, err := category(d.buf)
		if err == nil {
			err = d.keepText(len(c.Name))
		}
		if err != nil {
			return fmt.Errorf("category %d: %w", len(d.out.Categories)+1, err)
		}
		d.out.Categories = append(d.out.Categories, c)
	case backupSources:
		if len(d.out.Sources) >= d.lim.MaxSources {
			return fmt.Errorf("%w: over %d sources", ErrTooLarge, d.lim.MaxSources)
		}
		s, err := source(d.buf)
		if err == nil {
			err = d.keepText(len(s.Name))
		}
		if err != nil {
			return fmt.Errorf("source %d: %w", len(d.out.Sources)+1, err)
		}
		d.out.Sources = append(d.out.Sources, s)
	}
	return nil
}

// finish joins what the backup refers to by key: source names, categories
// by order and last-read times by chapter URL.
func (d *decoder) finish() (*Backup, error) {
	b := d.out
	inLibrary := 0
	for _, s := range b.Series {
		if s.InLibrary {
			inLibrary++
		}
	}
	if inLibrary == 0 {
		return nil, ErrEmpty
	}
	slices.SortStableFunc(b.Categories, func(x, y Category) int {
		switch {
		case x.Order < y.Order:
			return -1
		case x.Order > y.Order:
			return 1
		}
		return 0
	})
	sources := make(map[int64]string, len(b.Sources))
	for _, s := range b.Sources {
		sources[s.ID] = s.Name
	}
	categories := make(map[int64]string, len(b.Categories))
	for _, c := range b.Categories {
		if _, ok := categories[c.Order]; !ok {
			categories[c.Order] = c.Name
		}
	}
	for i := range b.Series {
		s := &b.Series[i]
		s.SourceName = sources[s.SourceID]
		s.Categories = joinCategories(s.categoryOrders, categories)
		s.categoryOrders = nil
		if len(s.History) == 0 {
			continue
		}
		lastRead := make(map[string]time.Time, len(s.History))
		for _, h := range s.History {
			if h.LastRead.After(lastRead[h.URL]) {
				lastRead[h.URL] = h.LastRead
			}
		}
		for j := range s.Chapters {
			s.Chapters[j].LastRead = lastRead[s.Chapters[j].URL]
		}
	}
	return b, nil
}

// joinCategories turns the category orders a series refers to into names,
// each name once. Orders with no category are dropped, as Mihon does when
// restoring. The seen set keeps it linear in the number of orders.
func joinCategories(orders []int64, names map[int64]string) []string {
	if len(orders) == 0 {
		return nil
	}
	var out []string
	seen := make(map[string]struct{}, min(len(orders), len(names)))
	for _, order := range orders {
		name, ok := names[order]
		if !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// count checks a series' chapters, history and tracking links against the
// limits before any is decoded, so that each list is allocated once at its
// final size instead of growing (and taking up to twice the room) entry by
// entry.
func (d *decoder) count(b []byte) (chapters, history, trackers int, err error) {
	err = fields(b, func(f field) error {
		switch f.num {
		case mangaChapters:
			chapters++
		case mangaHistory:
			history++
		case mangaTracking:
			trackers++
		}
		return nil
	})
	switch {
	case err != nil:
	case d.chapters+chapters > d.lim.MaxChapters:
		err = fmt.Errorf("%w: over %d chapters", ErrTooLarge, d.lim.MaxChapters)
	case d.history+history > d.lim.MaxHistory:
		err = fmt.Errorf("%w: over %d history entries", ErrTooLarge, d.lim.MaxHistory)
	case d.trackers+trackers > d.lim.MaxTrackerLinks:
		err = fmt.Errorf("%w: over %d tracking links", ErrTooLarge, d.lim.MaxTrackerLinks)
	}
	return chapters, history, trackers, err
}

func (d *decoder) series(b []byte) (Series, error) {
	s := Series{InLibrary: true} // favorite defaults to true and is then left out
	nChapters, nHistory, nTrackers, err := d.count(b)
	if err != nil {
		return s, err
	}
	if nChapters > 0 {
		s.Chapters = make([]Chapter, 0, nChapters)
	}
	if nHistory > 0 {
		s.History = make([]History, 0, nHistory)
	}
	if nTrackers > 0 {
		s.Trackers = make([]int32, 0, nTrackers)
	}
	var hasSource, hasURL bool
	err = fields(b, func(f field) error {
		var err error
		switch f.num {
		case mangaSource:
			s.SourceID, err = f.int64()
			hasSource = true
		case mangaURL:
			s.URL, err = f.string()
			hasURL = true
		case mangaTitle:
			s.Title, err = f.string()
		case mangaArtist:
			s.Artist, err = f.string()
		case mangaAuthor:
			s.Author, err = f.string()
		case mangaStatus:
			var v int64
			v, err = f.int64()
			s.Status = Status(v) //nolint:gosec // any value maps to a Status; unknown ones print as "unknown"
		case mangaDateAdded:
			s.DateAdded, err = f.time()
		case mangaFavorite:
			s.InLibrary, err = f.bool()
		case mangaCategory:
			err = f.varints(func(v uint64) error {
				if d.categoryRefs >= d.lim.MaxCategoryRefs {
					return fmt.Errorf("%w: over %d category references", ErrTooLarge, d.lim.MaxCategoryRefs)
				}
				d.categoryRefs++
				s.categoryOrders = append(s.categoryOrders, int64(v)) //nolint:gosec // two's complement, as encoded
				return nil
			})
		case mangaChapters:
			if d.chapters >= d.lim.MaxChapters {
				return fmt.Errorf("%w: over %d chapters", ErrTooLarge, d.lim.MaxChapters)
			}
			var c Chapter
			c, err = chapter(f)
			if err == nil {
				d.chapters++
				s.Chapters = append(s.Chapters, c)
			}
		case mangaHistory:
			if d.history >= d.lim.MaxHistory {
				return fmt.Errorf("%w: over %d history entries", ErrTooLarge, d.lim.MaxHistory)
			}
			var h History
			h, err = history(f)
			if err == nil {
				d.history++
				s.History = append(s.History, h)
			}
		case mangaTracking:
			if d.trackers >= d.lim.MaxTrackerLinks {
				return fmt.Errorf("%w: over %d tracking links", ErrTooLarge, d.lim.MaxTrackerLinks)
			}
			var id int32
			var ok bool
			id, ok, err = tracking(f)
			d.trackers++ // counted even when skipped, as count did
			if err == nil && ok {
				s.Trackers = append(s.Trackers, id)
			}
		}
		return err
	})
	if err == nil && (!hasSource || !hasURL) {
		err = errors.New("missing its source or URL")
	}
	return s, err
}

func chapter(f field) (Chapter, error) {
	var c Chapter
	var hasURL bool
	err := f.message(func(f field) error {
		var err error
		switch f.num {
		case chapterURL:
			c.URL, err = f.string()
			hasURL = true
		case chapterName:
			c.Name, err = f.string()
		case chapterScanlator:
			c.Scanlator, err = f.string()
		case chapterRead:
			c.Read, err = f.bool()
		case chapterBookmark:
			c.Bookmark, err = f.bool()
		case chapterLastPage:
			c.LastPageRead, err = f.int64()
		case chapterDateFetch:
			c.DateFetch, err = f.time()
		case chapterDateUpload:
			c.DateUpload, err = f.time()
		case chapterNumber:
			c.Number, err = f.float32()
		}
		return err
	})
	if err == nil && !hasURL {
		err = errors.New("a chapter has no URL")
	}
	return c, err
}

func history(f field) (History, error) {
	var h History
	var hasURL bool
	err := f.message(func(f field) error {
		var err error
		switch f.num {
		case historyURL:
			h.URL, err = f.string()
			hasURL = true
		case historyLastRead:
			h.LastRead, err = f.time()
		}
		return err
	})
	if err == nil && !hasURL {
		err = errors.New("a history entry has no URL")
	}
	return h, err
}

// tracking reads a tracking link's tracker id. Mihon requires it; an entry
// without one names no tracker, so it isn't kept (ok false).
func tracking(f field) (id int32, ok bool, err error) {
	var v int64
	err = f.message(func(f field) error {
		if f.num != trackingSyncID {
			return nil
		}
		ok = true
		var err error
		v, err = f.int64()
		return err
	})
	return int32(v), ok, err //nolint:gosec // an Int in the format
}

func category(b []byte) (Category, error) {
	var c Category
	var hasName bool
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case categoryName:
			c.Name, err = f.string()
			hasName = true
		case categoryOrder:
			c.Order, err = f.int64()
		}
		return err
	})
	if err == nil && !hasName {
		err = errors.New("missing its name")
	}
	return c, err
}

func source(b []byte) (Source, error) {
	var s Source
	var hasID bool
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case sourceName:
			s.Name, err = f.string()
		case sourceID:
			s.ID, err = f.int64()
			hasID = true
		}
		return err
	})
	if err == nil && !hasID {
		err = errors.New("missing its id")
	}
	return s, err
}

// errTruncated: the input ends in the middle of a value.
var errTruncated = errors.New("the file ends early (incomplete download or upload?)")

// noEOF turns an end of input in the middle of a value into a truncation.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errTruncated
	}
	return err
}

// size formats a limit for an error message.
func size(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}
