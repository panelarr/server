// SPDX-License-Identifier: GPL-3.0-or-later

package mihonbackup

import "time"

// Backup is what a backup holds that an import can use. Everything else in
// the file (preferences, extension repositories, reader settings, app-specific
// extensions) is skipped while reading.
type Backup struct {
	// Series in file order: the library and, in Mihon backups, series kept
	// only for their history (InLibrary false).
	Series []Series
	// Categories ordered by Order.
	Categories []Category
	// Sources are the names of the sources the series come from, as the
	// backup lists them (no language), in file order.
	Sources []Source
}

// Series is one series of the backup.
type Series struct {
	// SourceID is the Mihon source id, which mihon-source uses too.
	SourceID int64
	// SourceName is the source's name from the backup's source list, or "".
	SourceName string
	// URL is the source's key of the series.
	URL       string
	Title     string
	Author    string
	Artist    string
	Status    Status
	InLibrary bool
	// DateAdded is when the series was added to the library (zero if unknown).
	DateAdded time.Time
	// Categories are the names of the series' categories, joined by order
	// (the backup refers to categories by their order, not their id).
	Categories []string
	Chapters   []Chapter
	// History is the series' read history: one entry per chapter URL with its
	// last-read time. Backups made before history was added have none.
	History []History
	// Trackers are the tracker ids of the series' tracking links (counted,
	// not imported).
	Trackers []int32

	categoryOrders []int64 // while reading, before categories are joined
}

// textSize is the bytes of text the series keeps, chapters and history
// included.
func (s *Series) textSize() int {
	n := len(s.URL) + len(s.Title) + len(s.Author) + len(s.Artist)
	for i := range s.Chapters {
		c := &s.Chapters[i]
		n += len(c.URL) + len(c.Name) + len(c.Scanlator)
	}
	for i := range s.History {
		n += len(s.History[i].URL)
	}
	return n
}

// Chapter is one chapter of a series with the user's reading state.
type Chapter struct {
	// URL is the source's key of the chapter.
	URL       string
	Name      string
	Scanlator string
	// Number is the chapter number; negative when the source has none.
	Number       float32
	Read         bool
	Bookmark     bool
	LastPageRead int64
	// LastRead is the chapter's last-read time from the series' history
	// (zero when it has none).
	LastRead   time.Time
	DateFetch  time.Time
	DateUpload time.Time
}

// History is a last-read time for a chapter, by the chapter's URL.
type History struct {
	URL      string
	LastRead time.Time
}

// Category is a library category.
type Category struct {
	Name  string
	Order int64
}

// Source names a source the backup's series come from.
type Source struct {
	ID   int64
	Name string
}

// Status is a series' publishing status as Mihon numbers it.
type Status int32

// Mihon's publishing statuses.
const (
	StatusUnknown Status = iota
	StatusOngoing
	StatusCompleted
	StatusLicensed
	StatusPublishingFinished
	StatusCancelled
	StatusOnHiatus
)

var statusNames = [...]string{"unknown", "ongoing", "completed", "licensed", "publishing_finished", "cancelled", "on_hiatus"}

// String names the status; values the format doesn't define are "unknown".
func (s Status) String() string {
	if s < 0 || int(s) >= len(statusNames) {
		return "unknown"
	}
	return statusNames[s]
}

// Summary counts what a backup holds, for a preview or a report.
type Summary struct {
	Series       int // series in the library
	OutOfLibrary int // series kept only for their history
	Chapters     int // chapters of series in the library
	ChaptersRead int
	InProgress   int // unread chapters with a last page read
	Bookmarks    int
	History      int // history entries of series in the library
	Categories   int
	TrackerLinks int // tracking links of series in the library
}

// Summary counts the backup's contents. Only series in the library count
// towards chapters, bookmarks, history and tracking.
func (b *Backup) Summary() Summary {
	sum := Summary{Categories: len(b.Categories)}
	for i := range b.Series {
		s := &b.Series[i]
		if !s.InLibrary {
			sum.OutOfLibrary++
			continue
		}
		sum.Series++
		sum.Chapters += len(s.Chapters)
		sum.History += len(s.History)
		sum.TrackerLinks += len(s.Trackers)
		for _, c := range s.Chapters {
			if c.Read {
				sum.ChaptersRead++
			} else if c.LastPageRead > 0 {
				sum.InProgress++
			}
			if c.Bookmark {
				sum.Bookmarks++
			}
		}
	}
	return sum
}
