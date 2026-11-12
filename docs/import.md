# Importing a library

Panelarr can import a library and its reading state from a Suwayomi or Mihon backup. This page describes the
backup reader; the import itself (preview, apply, the API) builds on it.

## Why the backup file

Suwayomi and Mihon write the same backup format (`.tachibk`): protobuf with Mihon's field numbers, gzip-compressed.
Reading the file needs no credentials and no network access to the old server, works after that server is gone,
and covers both apps with one reader. Suwayomi exports only series in its library, while Mihon also keeps series that
are only in its history.

## The reader (`internal/importer/mihonbackup`)

`mihonbackup.Read` parses a backup into a plain model: the series with their source id, series URL, title, author,
artist, status, date added and categories; their chapters with the read mark, bookmark, last page read and
last-read time; the history; the categories; the names of the sources; and the trackers each series is linked to.
`Backup.Summary` counts these for a preview.

It decodes the wire format directly (`google.golang.org/protobuf/encoding/protowire`) instead of generating code
from a schema: it needs a handful of fields out of a large, app-specific format, and decoding only those makes
skipping everything else the default. Fields it reads:

| Message | Fields |
|---|---|
| `Backup` | 1 series, 2 categories, 101 sources |
| `BackupManga` | 1 source id, 2 URL, 3 title, 4 artist, 5 author, 8 status, 13 date added, 16 chapters, 17 category orders (packed or not), 18 tracking, 100 in library (defaults to true when absent), 104 history |
| `BackupChapter` | 1 URL, 2 name, 3 scanlator, 4 read, 5 bookmark, 6 last page read, 7 date fetched, 8 date uploaded, 9 number (float) |
| `BackupHistory` | 1 chapter URL, 2 last read |
| `BackupCategory` | 1 name, 2 order |
| `BackupTracking` | 1 tracker id |
| `BackupSource` | 1 name, 2 source id |

Everything else is skipped: preferences, extension repositories, reader settings, fields only one of the apps
writes (such as Suwayomi's own fields from 9000 up) and the legacy source list (field 100, whose inner field 0 isn't
valid protobuf). Times are milliseconds since the epoch; 0 means unknown. Text has invalid UTF-8 replaced and NUL
characters removed, since PostgreSQL text holds neither.

Joins the reader makes:

- **Categories by order.** A series refers to its categories by their `order`, not their id, as Mihon restores
  them. Orders without a category are dropped; when two categories share an order, the first one wins.
- **Last-read times by chapter URL.** History entries name the chapter by its URL. Backups from older versions (Suwayomi
  before mid-2025, for one) have no history: read marks without times.
- **Source names by source id.** The backup lists names without a language.

### Rejected input

`Read` fails with one of these errors (wrapped with a detail for the user):

| Error | When |
|---|---|
| `ErrNotBackup` | Empty, not gzip or protobuf, damaged (truncated, bad checksum, a field of the wrong type) or missing a field Mihon requires (a series' source or URL, a chapter's URL, …) |
| `ErrUnsupported` | A legacy JSON backup |
| `ErrTooLarge` | Over one of the limits below |
| `ErrEmpty` | A valid backup with no series in the library |

An error of the reader passed in (a dropped upload) is returned as it is, not as a bad backup.

### Limits

The reader streams the file and holds one top-level message (one series with its chapters) at a time, plus the
model. Each limit can be set; zero takes the default:

| Limit | Default | Why |
|---|---|---|
| File size | 64 MiB | The import upload's body limit. A large library is a few MiB compressed |
| Decompressed size | 256 MiB | Bounds a decompression bomb, counted as it streams |
| One message | 16 MiB | One series with all its chapters is far smaller |
| Series | 50,000 | Bounds the model's memory |
| Chapters (over all series) | 2,000,000 | Bounds the model's memory |
