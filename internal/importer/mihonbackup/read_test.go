// SPDX-License-Identifier: GPL-3.0-or-later

package mihonbackup

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// msg builds a protobuf message the way Mihon's and Suwayomi's encoder
// writes one. All data here is made up.
type msg []byte

func (m msg) varint(num protowire.Number, v uint64) msg {
	m = protowire.AppendTag(m, num, protowire.VarintType)
	return protowire.AppendVarint(m, v)
}

func (m msg) int(num protowire.Number, v int64) msg { return m.varint(num, uint64(v)) }

func (m msg) bool(num protowire.Number, v bool) msg { return m.varint(num, protowire.EncodeBool(v)) }

func (m msg) float(num protowire.Number, v float32) msg {
	m = protowire.AppendTag(m, num, protowire.Fixed32Type)
	return protowire.AppendFixed32(m, math.Float32bits(v))
}

func (m msg) fixed64(num protowire.Number, v uint64) msg {
	m = protowire.AppendTag(m, num, protowire.Fixed64Type)
	return protowire.AppendFixed64(m, v)
}

func (m msg) bytes(num protowire.Number, b []byte) msg {
	m = protowire.AppendTag(m, num, protowire.BytesType)
	return protowire.AppendBytes(m, b)
}

func (m msg) str(num protowire.Number, s string) msg { return m.bytes(num, []byte(s)) }

func (m msg) msg(num protowire.Number, sub msg) msg { return m.bytes(num, sub) }

func (m msg) append(b []byte) msg { return append(m, b...) }

func gz(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	added    = time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	readAt   = time.Date(2025, 8, 2, 20, 30, 0, 0, time.UTC)
	readAt2  = time.Date(2025, 8, 3, 21, 0, 0, 0, time.UTC)
	fetched  = time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC)
	uploaded = time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)
)

func ms(t time.Time) int64 { return t.UnixMilli() }

// suwayomiMeta is one entry of Suwayomi's meta map (field 9000).
func suwayomiMeta(k, v string) msg { return msg{}.str(1, k).str(2, v) }

// suwayomiBackup mimics what Suwayomi writes: Mihon's fields as of its copy
// of the format (lastModifiedAt 106, version 109), its meta maps at 9000 and
// unpacked category lists.
func suwayomiBackup() msg {
	ch1 := msg{}.str(1, "/series/alpha/1").str(2, "Chapter 1").str(3, "Group A").
		bool(4, true).int(6, 23).int(7, ms(fetched)).int(8, ms(uploaded)).float(9, 1).int(10, 1).
		int(11, 1700000000000).int(12, 3).msg(9000, suwayomiMeta("k", "v"))
	ch2 := msg{}.str(1, "/series/alpha/2").str(2, "Chapter 2").bool(5, true).int(6, 7).float(9, 2)
	ch3 := msg{}.str(1, "/series/alpha/2.5").str(2, "Extra").float(9, -1)
	alpha := msg{}.int(1, 1234567890123).str(2, "/series/alpha").str(3, "Alpha Story").
		str(4, "Artist A").str(5, "Author A").str(6, "A description.").str(7, "Action").str(7, "Drama").
		int(8, 1).str(9, "https://example.com/alpha.jpg").int(13, ms(added)).int(14, 0).
		msg(16, ch1).msg(16, ch2).msg(16, ch3).
		int(17, 1).int(17, 2).
		msg(18, msg{}.int(1, 2).int(2, 99).str(4, "https://tracker.example.com/1").float(8, 7.5).int(100, 42)).
		int(101, 0).
		msg(104, msg{}.str(1, "/series/alpha/1").int(2, ms(readAt)).int(3, 60000)).
		msg(104, msg{}.str(1, "/series/alpha/2").int(2, ms(readAt2))).
		int(105, 0).int(106, 1700000000000).int(109, 7).bool(111, true).bytes(112, []byte("{}")).
		msg(9000, suwayomiMeta("panelarr-test", "1"))
	beta := msg{}.int(1, 42).str(2, "/series/beta").str(3, "Beta").int(8, 2).
		msg(16, msg{}.str(1, "/series/beta/1").str(2, "Ch. 1").float(9, 1))
	return msg{}.
		msg(1, alpha).msg(1, beta).
		msg(2, msg{}.str(1, "Reading").int(2, 1).int(100, 0).msg(9000, suwayomiMeta("x", "y"))).
		msg(2, msg{}.str(1, "Favourites").int(2, 2)).
		msg(101, msg{}.str(1, "Example Source").int(2, 1234567890123)).
		msg(101, msg{}.str(1, "Another Source").int(2, 42)).
		msg(9001, msg{}.str(1, "server settings"))
}

// mihonBackup mimics what Mihon writes: newer fields (favoriteModifiedAt
// 107, notes 110, …), packed category lists, a series kept only for its
// history, preferences and the legacy broken-source field (number 100,
// whose inner field 0 isn't valid protobuf, so it must be skipped whole).
func mihonBackup() msg {
	gamma := msg{}.int(1, 7).str(2, "/g").str(3, "Gamma").str(5, "Author G").int(8, 6).int(13, ms(added)).
		msg(16, msg{}.str(1, "/g/1").str(2, "1").bool(4, true).float(9, 1).bytes(13, []byte("{}"))).
		msg(16, msg{}.str(1, "/g/2").str(2, "2").int(6, 4).float(9, 2)).
		bytes(17, protowire.AppendVarint(protowire.AppendVarint(nil, 0), 5)).
		msg(104, msg{}.str(1, "/g/1").int(2, ms(readAt))).
		int(107, 1730000000000).str(110, "a note").bool(111, true).int(113, 0)
	gone := msg{}.int(1, 7).str(2, "/h").str(3, "Read once").bool(100, false).
		msg(16, msg{}.str(1, "/h/1").str(2, "1").bool(4, true).float(9, 1)).
		msg(104, msg{}.str(1, "/h/1").int(2, ms(readAt)))
	broken := []byte{0x02, 0x03, 'o', 'l', 'd', 0x08, 0x07} // field 0 (name), field 1 (id)
	return msg{}.
		bytes(100, broken).
		msg(1, gamma).msg(1, gone).
		msg(2, msg{}.str(1, "Default-ish").int(2, 0).int(3, 11)).
		msg(2, msg{}.str(1, "Later").int(2, 5).int(3, 12).int(100, 3)).
		msg(101, msg{}.int(2, 7)).
		msg(104, msg{}.str(1, "pref").msg(2, msg{}.int(1, 1))).
		msg(106, msg{}.str(1, "https://repo.example.com")).
		fixed64(9002, 1).float(9003, 1)
}

func TestReadSuwayomiBackup(t *testing.T) {
	b, err := Read(bytes.NewReader(gz(t, suwayomiBackup())), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Series) != 2 {
		t.Fatalf("series = %d, want 2", len(b.Series))
	}
	a := b.Series[0]
	if a.SourceID != 1234567890123 || a.SourceName != "Example Source" || a.URL != "/series/alpha" ||
		a.Title != "Alpha Story" || a.Author != "Author A" || a.Artist != "Artist A" ||
		a.Status != StatusOngoing || !a.InLibrary || !a.DateAdded.Equal(added) {
		t.Errorf("series = %+v", a)
	}
	if got := strings.Join(a.Categories, ","); got != "Reading,Favourites" {
		t.Errorf("categories = %q", got)
	}
	if len(a.Trackers) != 1 || a.Trackers[0] != 2 {
		t.Errorf("trackers = %v", a.Trackers)
	}
	if len(a.Chapters) != 3 {
		t.Fatalf("chapters = %d", len(a.Chapters))
	}
	c1, c2, c3 := a.Chapters[0], a.Chapters[1], a.Chapters[2]
	if c1.URL != "/series/alpha/1" || c1.Name != "Chapter 1" || c1.Scanlator != "Group A" || !c1.Read ||
		c1.Bookmark || c1.LastPageRead != 23 || c1.Number != 1 || !c1.LastRead.Equal(readAt) ||
		!c1.DateFetch.Equal(fetched) || !c1.DateUpload.Equal(uploaded) {
		t.Errorf("chapter 1 = %+v", c1)
	}
	if c2.Read || !c2.Bookmark || c2.LastPageRead != 7 || !c2.LastRead.Equal(readAt2) {
		t.Errorf("chapter 2 = %+v", c2)
	}
	if c3.Number != -1 || !c3.LastRead.IsZero() || c3.Read {
		t.Errorf("chapter 3 = %+v", c3)
	}
	if len(a.History) != 2 {
		t.Errorf("history = %+v", a.History)
	}
	if s := b.Series[1]; s.SourceName != "Another Source" || s.Status != StatusCompleted || s.Categories != nil {
		t.Errorf("series 2 = %+v", s)
	}
	want := Summary{Series: 2, Chapters: 4, ChaptersRead: 1, InProgress: 1, Bookmarks: 1, History: 2, Categories: 2, TrackerLinks: 1}
	if got := b.Summary(); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

func TestReadMihonBackup(t *testing.T) {
	for name, data := range map[string][]byte{
		"gzip":  gz(t, mihonBackup()),
		"plain": mihonBackup(), // Mihon restores uncompressed backups too
	} {
		t.Run(name, func(t *testing.T) {
			b, err := Read(bytes.NewReader(data), Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if len(b.Series) != 2 {
				t.Fatalf("series = %d", len(b.Series))
			}
			g, gone := b.Series[0], b.Series[1]
			if !g.InLibrary || gone.InLibrary {
				t.Errorf("in library = %v, %v", g.InLibrary, gone.InLibrary)
			}
			if g.SourceName != "" || g.Status != StatusOnHiatus || g.Author != "Author G" {
				t.Errorf("series = %+v", g)
			}
			// Packed orders 0 and 5 join to the categories with those orders.
			if got := strings.Join(g.Categories, ","); got != "Default-ish,Later" {
				t.Errorf("categories = %q", got)
			}
			if !g.Chapters[0].LastRead.Equal(readAt) || g.Chapters[1].LastPageRead != 4 {
				t.Errorf("chapters = %+v", g.Chapters)
			}
			if b.Categories[0].Name != "Default-ish" || b.Categories[1].Order != 5 {
				t.Errorf("categories = %+v", b.Categories)
			}
			want := Summary{Series: 1, OutOfLibrary: 1, Chapters: 2, ChaptersRead: 1, InProgress: 1, History: 1, Categories: 2}
			if got := b.Summary(); got != want {
				t.Errorf("summary = %+v, want %+v", got, want)
			}
		})
	}
}

// Backups made before history was added (field 104) carry read marks only.
func TestReadBackupWithoutHistory(t *testing.T) {
	s := msg{}.int(1, 1).str(2, "/old").str(3, "Old").
		msg(16, msg{}.str(1, "/old/1").str(2, "1").bool(4, true).int(7, ms(fetched)).float(9, 1))
	b, err := Read(bytes.NewReader(gz(t, msg{}.msg(1, s))), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	c := b.Series[0].Chapters[0]
	if !c.Read || !c.LastRead.IsZero() || !c.DateFetch.Equal(fetched) || b.Series[0].History != nil {
		t.Errorf("chapter = %+v", c)
	}
	if !b.Series[0].DateAdded.IsZero() {
		t.Errorf("date added = %v", b.Series[0].DateAdded)
	}
}

// Categories are joined by order, the first category wins a shared order,
// unknown orders are dropped and a series lists a category once.
func TestReadCategoriesJoinedByOrder(t *testing.T) {
	s := msg{}.int(1, 1).str(2, "/s").int(17, 3).int(17, 9).int(17, 1).int(17, 3)
	data := msg{}.msg(1, s).
		msg(2, msg{}.str(1, "C").int(2, 3).int(3, 1)).
		msg(2, msg{}.str(1, "A").int(2, 1).int(3, 2)).
		msg(2, msg{}.str(1, "C again").int(2, 3).int(3, 3))
	b, err := Read(bytes.NewReader(gz(t, data)), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(b.Series[0].Categories, ","); got != "C,A" {
		t.Errorf("categories = %q", got)
	}
	if b.Categories[0].Name != "A" || b.Categories[1].Name != "C" || b.Categories[2].Name != "C again" {
		t.Errorf("order = %+v", b.Categories)
	}
}

func TestReadCleansText(t *testing.T) {
	s := msg{}.int(1, 1).str(2, "/s").str(3, "Bad\x00 \xff title")
	b, err := Read(bytes.NewReader(gz(t, msg{}.msg(1, s))), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Series[0].Title; got != "Bad � title" {
		t.Errorf("title = %q", got)
	}
}

func TestReadRejects(t *testing.T) {
	valid := gz(t, suwayomiBackup())
	series := slices.Clip(msg{}.int(1, 1).str(2, "/s")) // appends copy, so cases share nothing
	cases := []struct {
		name string
		data []byte
		lim  Limits
		want error
		msg  string
	}{
		{name: "empty file", data: nil, want: ErrNotBackup, msg: "empty"},
		{name: "one byte", data: []byte{0x1f}, want: ErrNotBackup, msg: "too short"},
		{name: "text", data: []byte("hello, this is not a backup\n"), want: ErrNotBackup},
		{name: "image", data: []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), want: ErrNotBackup},
		{name: "legacy JSON", data: []byte(`{"version":2,"mangas":[]}`), want: ErrUnsupported, msg: "JSON"},
		{name: "gzip of JSON", data: gz(t, []byte(`{"version":2}`)), want: ErrNotBackup},
		{name: "gzip of text", data: gz(t, []byte("not protobuf at all")), want: ErrNotBackup},
		{name: "truncated gzip", data: valid[:len(valid)/2], want: ErrNotBackup, msg: "ends early"},
		{name: "bad gzip checksum", data: append(append([]byte{}, valid[:len(valid)-8]...), 0, 0, 0, 0, 0, 0, 0, 0), want: ErrNotBackup},
		{name: "bad gzip header", data: []byte{0x1f, 0x8b, 0, 0, 0}, want: ErrNotBackup},
		{name: "wrong wire type", data: gz(t, msg{}.msg(1, msg{}.int(1, 1).int(2, 5))), want: ErrNotBackup, msg: "wire type"},
		{name: "length past the end", data: gz(t, append(msg{}.msg(1, series), 0x0a, 0x7f, 1)), want: ErrNotBackup},
		{name: "inner length past the end", data: gz(t, msg{}.bytes(1, append(series, 0x82, 0x01, 0x7f))), want: ErrNotBackup},
		{name: "series without URL", data: gz(t, msg{}.msg(1, msg{}.int(1, 1).str(3, "T"))), want: ErrNotBackup, msg: "series 1"},
		{name: "chapter without URL", data: gz(t, msg{}.msg(1, append(series, msg{}.msg(16, msg{}.str(2, "n"))...))), want: ErrNotBackup, msg: "chapter"},
		{name: "top-level group", data: gz(t, protowire.AppendTag(nil, 5, protowire.StartGroupType)), want: ErrNotBackup},
		{name: "field number 0", data: gz(t, []byte{0x02, 0x00}), want: ErrNotBackup},
		{name: "no series", data: gz(t, msg{}.msg(2, msg{}.str(1, "Cat"))), want: ErrEmpty},
		{name: "only series out of the library", data: gz(t, msg{}.msg(1, series.bool(100, false))), want: ErrEmpty},
		{name: "empty gzip", data: gz(t, nil), want: ErrEmpty},
		{name: "file too large", data: valid, lim: Limits{MaxFileSize: int64(len(valid)) - 1}, want: ErrTooLarge, msg: "file over"},
		{name: "data too large", data: valid, lim: Limits{MaxDataSize: int64(len(suwayomiBackup())) - 1}, want: ErrTooLarge, msg: "decompressed"},
		{name: "message too large", data: valid, lim: Limits{MaxMessageSize: 64}, want: ErrTooLarge},
		{name: "too many series", data: valid, lim: Limits{MaxSeries: 1}, want: ErrTooLarge, msg: "series"},
		{name: "too many chapters", data: valid, lim: Limits{MaxChapters: 3}, want: ErrTooLarge, msg: "chapters"},
		{name: "too many history entries", data: valid, lim: Limits{MaxHistory: 1}, want: ErrTooLarge, msg: "history"},
		{name: "too many category references", data: valid, lim: Limits{MaxCategoryRefs: 1}, want: ErrTooLarge, msg: "category references"},
		{name: "too many tracking links", data: gz(t, msg{}.msg(1, series.msg(18, msg{}.int(1, 1)).msg(18, msg{}.int(1, 2)))), lim: Limits{MaxTrackerLinks: 1}, want: ErrTooLarge, msg: "tracking"},
		{name: "too many categories", data: valid, lim: Limits{MaxCategories: 1}, want: ErrTooLarge, msg: "categories"},
		{name: "too many sources", data: valid, lim: Limits{MaxSources: 1}, want: ErrTooLarge, msg: "sources"},
		{name: "too much text", data: valid, lim: Limits{MaxText: 100}, want: ErrTooLarge, msg: "text"},
		{name: "too much text in a category", data: gz(t, msg{}.msg(1, series).msg(2, msg{}.str(1, strings.Repeat("c", 64)))), lim: Limits{MaxText: 64}, want: ErrTooLarge, msg: "category 1"},
		{name: "too much text in a source", data: gz(t, msg{}.msg(1, series).msg(101, msg{}.str(1, strings.Repeat("s", 64)).int(2, 1))), lim: Limits{MaxText: 64}, want: ErrTooLarge, msg: "source 1"},
		{name: "huge declared length", data: gz(t, append(protowire.AppendTag(nil, 9001, protowire.BytesType), protowire.AppendVarint(nil, 1<<40)...)), want: ErrTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Read(bytes.NewReader(tc.data), tc.lim)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if b != nil {
				t.Error("returned a backup with the error")
			}
			if tc.msg != "" && !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("err = %q, want it to mention %q", err, tc.msg)
			}
		})
	}
}

// A decompression bomb stops at the data limit instead of filling memory:
// skipped fields, each under the limit, stream through without being held
// until the decompressed total is over it.
func TestReadStopsDecompressionBomb(t *testing.T) {
	const field = 256 << 10
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for range 32 { // 8 MiB
		_, _ = zw.Write(protowire.AppendVarint(protowire.AppendTag(nil, 9001, protowire.BytesType), field))
		_, _ = zw.Write(make([]byte, field))
	}
	_ = zw.Close()
	if buf.Len() > 64<<10 {
		t.Fatalf("bomb is %d bytes compressed", buf.Len())
	}
	_, err := Read(&buf, Limits{MaxDataSize: 1 << 20})
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "decompressed") {
		t.Fatalf("err = %v", err)
	}
}

// A tracking entry without its tracker id names no tracker: it is skipped
// instead of becoming tracker 0.
func TestReadSkipsTrackingWithoutID(t *testing.T) {
	s := msg{}.int(1, 1).str(2, "/s").msg(18, msg{}.int(2, 5)).msg(18, msg{}.int(1, 3))
	b, err := Read(bytes.NewReader(gz(t, msg{}.msg(1, s))), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.Series[0].Trackers, []int32{3}) {
		t.Errorf("trackers = %v", b.Series[0].Trackers)
	}
}

// A long list of category orders joins in linear time: 200,000 distinct
// orders, all naming categories, took minutes with a quadratic join.
func TestJoinCategoriesLinear(t *testing.T) {
	const n = 200_000
	orders := make([]int64, n)
	names := make(map[int64]string, n)
	for i := range orders {
		orders[i] = int64(i)
		names[int64(i)] = strconv.Itoa(i)
	}
	start := time.Now()
	got := joinCategories(append(orders, orders...), names)
	if len(got) != n {
		t.Fatalf("%d names", len(got))
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("join took %v", d)
	}
}

// invalidText is a series' title, artist and author, 1 MiB each of text
// where every other byte isn't UTF-8: replaced, it takes twice that in the
// model. Every series of hostile holds it once.
func invalidText() msg {
	bad := strings.Repeat("a\xff", 1<<19)
	return msg{}.str(3, bad).str(4, bad).str(5, bad)
}

// hostile builds a small gzip'd backup that repeats one tiny entry n times:
// inside series, per entries to a series (0: as many as fit in 8 MiB), or,
// with top true, as top-level fields.
func hostile(t *testing.T, entry msg, n, per int, top bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	write := func(b []byte) {
		if _, err := zw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	write(msg{}.msg(1, msg{}.int(1, 1).str(2, "/s")))
	switch {
	case top:
		per = n
	case per == 0:
		per = max(1, (8<<20)/len(entry)) // messages of 8 MiB or less
	}
	chunk := bytes.Repeat(entry, per)
	for n > 0 {
		k := min(per, n)
		if top {
			write(chunk[:k*len(entry)])
		} else {
			write(msg{}.msg(1, append(msg{}.int(1, 1).str(2, "/s"), chunk[:k*len(entry)]...)))
		}
		n -= k
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Small hostile files full of tiny entries, each far past one cap, fail
// with ErrTooLarge at the default limits (text at a lower one, to keep the
// test quick), and the memory Read allocates stays within a budget set by
// that cap. Without the caps these entries,
// a few bytes each on the wire, grew the model to gigabytes.
func TestReadHostileFilesStayBounded(t *testing.T) {
	cases := []struct {
		name   string
		entry  msg
		n      int
		per    int // entries per series; 0: as many as fit
		top    bool
		lim    Limits
		budget uint64 // bytes Read may allocate in total, garbage included
		msg    string
	}{
		{name: "history in one series", entry: msg{}.msg(104, msg{}.str(1, "")), n: 2 * DefaultMaxHistory, budget: 32 << 20, msg: "history"},
		{name: "history", entry: msg{}.msg(104, msg{}.str(1, "")), n: 2 * DefaultMaxHistory, per: 1000, budget: 64 << 20, msg: "history"},
		{name: "chapters in one series", entry: msg{}.msg(16, msg{}.str(1, "")), n: 2 * DefaultMaxChapters, budget: 32 << 20, msg: "chapters"},
		{name: "chapters", entry: msg{}.msg(16, msg{}.str(1, "")), n: 2 * DefaultMaxChapters, per: 1000, budget: 192 << 20, msg: "chapters"},
		{name: "packed category orders", entry: msg{}.bytes(17, make([]byte, 2*DefaultMaxCategoryRefs)), n: 1, budget: 64 << 20, msg: "category references"},
		{name: "category orders", entry: msg{}.int(17, 0), n: 2 * DefaultMaxCategoryRefs, budget: 64 << 20, msg: "category references"},
		{name: "tracking", entry: msg{}.msg(18, msg{}.int(1, 1)), n: 2 * DefaultMaxTrackerLinks, per: 1000, budget: 16 << 20, msg: "tracking"},
		{name: "categories", entry: msg{}.msg(2, msg{}.str(1, "")), n: 1 << 20, top: true, budget: 16 << 20, msg: "categories"},
		{name: "sources", entry: msg{}.msg(101, msg{}.int(2, 1)), n: 1 << 20, top: true, budget: 16 << 20, msg: "sources"},
		{name: "invalid UTF-8 text", entry: invalidText(), n: 4, per: 1, lim: Limits{MaxText: 16 << 20}, budget: 32 << 20, msg: "text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := hostile(t, tc.entry, tc.n, tc.per, tc.top)
			if len(data) > 1<<20 {
				t.Fatalf("hostile file is %d bytes", len(data))
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := Read(bytes.NewReader(data), tc.lim)
			runtime.ReadMemStats(&after)
			if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v", err)
			}
			if got := after.TotalAlloc - before.TotalAlloc; got > tc.budget {
				t.Errorf("Read allocated %d MiB, budget %d MiB", got>>20, tc.budget>>20)
			}
		})
	}
}

// A backup at every count limit at once (all but text, which
// TestReadHostileFilesStayBounded covers) is accepted, and the model it
// gives stays within the size docs/import.md states for those limits.
func TestReadAtTheLimitsStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and reads a backup of a million chapters")
	}
	const series = 1000
	s := msg{}.int(1, 1).str(2, "/s").
		bytes(17, make([]byte, DefaultMaxCategoryRefs/series)).
		append(bytes.Repeat(msg{}.msg(16, msg{}.str(1, "")), DefaultMaxChapters/series)).
		append(bytes.Repeat(msg{}.msg(104, msg{}.str(1, "")), DefaultMaxHistory/series)).
		append(bytes.Repeat(msg{}.msg(18, msg{}.int(1, 1)), DefaultMaxTrackerLinks/series))
	var data msg
	for range series {
		data = data.msg(1, s)
	}
	for i := range DefaultMaxCategories {
		data = data.msg(2, msg{}.str(1, "").int(2, int64(i)))
	}
	for i := range DefaultMaxSources {
		data = data.msg(101, msg{}.int(2, int64(i)))
	}
	file := gz(t, data)
	data = nil

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	b, err := Read(bytes.NewReader(file), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	sum := b.Summary()
	if sum.Series != series || sum.Chapters != DefaultMaxChapters || sum.History != DefaultMaxHistory ||
		sum.TrackerLinks != DefaultMaxTrackerLinks || sum.Categories != DefaultMaxCategories {
		t.Errorf("summary = %+v", sum)
	}
	// 130 MiB of chapters, 38 MiB of history, the rest a few MiB.
	if live := int64(after.HeapAlloc) - int64(before.HeapAlloc); live > 224<<20 { //nolint:gosec // heap sizes
		t.Errorf("model takes %d MiB", live>>20)
	}
	if peak := after.TotalAlloc - before.TotalAlloc; peak > 320<<20 {
		t.Errorf("Read allocated %d MiB", peak>>20)
	}
	runtime.KeepAlive(b)
}

// An error of the caller's reader isn't reported as a bad backup.
func TestReadKeepsReaderErrors(t *testing.T) {
	boom := errors.New("connection reset")
	valid := gz(t, suwayomiBackup())
	r := io.MultiReader(bytes.NewReader(valid[:20]), iotest.ErrReader(boom))
	_, err := Read(r, Limits{})
	if !errors.Is(err, boom) || errors.Is(err, ErrNotBackup) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusString(t *testing.T) {
	if StatusOnHiatus.String() != "on_hiatus" || Status(99).String() != "unknown" || Status(-1).String() != "unknown" {
		t.Error("status names")
	}
}

// clean gives what replacing invalid UTF-8 and then removing NULs gives.
func FuzzClean(f *testing.F) {
	for _, s := range []string{"", "plain", "a\xffb", "\xff\xfe", "\xff\x00\xff", "x\x00", "\xe2\x82", "日本\xff語"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		want := strings.ReplaceAll(strings.ToValidUTF8(string(b), "\uFFFD"), "\x00", "")
		if got := clean(b); got != want {
			t.Fatalf("clean(%q) = %q, want %q", b, got, want)
		}
	})
}

func FuzzRead(f *testing.F) {
	f.Add([]byte(suwayomiBackup()))
	f.Add([]byte(mihonBackup()))
	f.Add(gz(f, mihonBackup()))
	f.Add([]byte{0x0a, 0x02, 0x08})
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := Read(bytes.NewReader(data), Limits{MaxDataSize: 1 << 20, MaxMessageSize: 1 << 16})
		if (b == nil) == (err == nil) {
			t.Fatalf("backup %v with error %v", b != nil, err)
		}
		if err != nil && !errors.Is(err, ErrNotBackup) && !errors.Is(err, ErrUnsupported) &&
			!errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrEmpty) {
			t.Fatalf("unclassified error: %v", err)
		}
	})
}
