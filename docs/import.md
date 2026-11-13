# Importing a library

Panelarr can import a library and its reading state from a Suwayomi or Mihon backup. This page describes the
backup reader and the preview; applying an import comes later. The API is in [api.md](api.md#imports).

## Why the backup file

Suwayomi and Mihon write the same backup format (`.tachibk`): protobuf with Mihon's field numbers, gzip-compressed.
Reading the file needs no credentials and no network access to the old server, works after that server is gone,
and covers both apps with one reader. Suwayomi exports only series in its library, while Mihon also keeps series that
are only in its history.

## Finding the sources

A backup names each series' source by the app's own source ID. Panelarr's core doesn't know any app's IDs: plugins
report, per source, the IDs it is known by elsewhere (`compat_ids`, `"<scheme>:<id>"`, e.g. `mihon:<id>` from a plugin
that runs Mihon extensions). The server stores them with the source on every sync (`sources.compat_ids`, a GIN index
for lookups; well-formed values only, at most 32 per source) and shows them to admins on the source calls
(`GET /sources`, and a source after a change), never to users.

The importer looks a source up by the ID its format uses (`Host.SourcesByCompatID`). More than one installed source
may claim one ID (two plugins serving the same extensions), so the lookup returns all of them, best first: sources
the importing user may use, then the others; within each, the plugin added first. Sources the user may not use and
ones their plugin no longer lists stay in the list, so the import can tell "not allowed" or "missing" from
"unknown". Series an import attaches are recorded with the origin `import`.

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
| `BackupTracking` | 1 tracker id (an entry without one is skipped) |
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
model. Every collection the model keeps has a cap, and so does its text, so the memory an upload takes is bounded
however the file is crafted: a few bytes on the wire can become a much larger value (a 4-byte empty chapter is a
136-byte `Chapter`), and the decompressed size alone doesn't bound that. Going over any limit fails with
`ErrTooLarge`. Each limit can be set; zero takes the default. The defaults leave room for libraries far larger than
real ones (a large library has a thousand or so series with a few hundred chapters each):

| Limit | Default | What it bounds |
|---|---|---|
| `MaxFileSize` | 64 MiB | Bytes of the file as uploaded: the import upload's body limit |
| `MaxDataSize` | 256 MiB | Bytes after decompression, counted as they stream: a decompression bomb |
| `MaxMessageSize` | 16 MiB | One top-level message (a series with its chapters, a category or a source), which is buffered whole |
| `MaxSeries` | 50,000 | Series, in the library or not |
| `MaxChapters` | 1,000,000 | Chapters over all series |
| `MaxHistory` | 1,000,000 | History entries over all series |
| `MaxCategoryRefs` | 500,000 | Category orders listed by series, over all series, repeats included |
| `MaxTrackerLinks` | 200,000 | Tracking entries over all series, including those skipped for having no tracker id |
| `MaxCategories` | 1,000 | Categories |
| `MaxSources` | 5,000 | Source names |
| `MaxText` | 128 MiB | Bytes of text the model keeps (titles, URLs, names, …) after cleaning. Replacing invalid UTF-8 can double text, so the data size alone doesn't bound it |

The chapter, history and tracking counts of a series are checked before it is decoded, and its lists are allocated
once at their final size. Categories are joined to series on small integers: each distinct category name is hashed
once, every order maps to an index, and a series' orders are deduplicated on those indexes. The join is linear in the
orders listed and doesn't depend on how long the names are (a name can be megabytes).

What the defaults allow at most, from the size of each element on 64-bit platforms:

| Part | Worst case |
|---|---|
| Series (240 bytes each) | 11 MiB |
| Chapters (136 bytes each) | 130 MiB |
| History (40 bytes each) | 38 MiB |
| Category orders, then joined names | 8 MiB |
| Tracking, categories, sources and the maps that join them | 2 MiB |
| Text | 128 MiB |
| **Model, by element sizes** | **about 320 MiB** |
| Allocation overhead (size classes, string headers, slack in maps) | about 30 MiB more |
| **Model, as measured** | **about 352 MiB** |
| While reading: one message buffer, the text of the message being decoded, the series list growing, one series' last-read times | about 90 MiB more |
| **Peak heap, as measured** | **about 445 MiB** |
| **Peak RSS of the process, as measured** | **about 465 MiB** |

The measured figures come from the worst file a review could build within the defaults. The tests read a backup at
every count limit at once and small, hostile files (a few hundred KiB that hold millions of entries of one kind, text
that doubles when cleaned, or megabyte-long category names referred to half a million times), and check the memory
and time `Read` takes against these figures.

### One read at a time

These figures are for one `Read`. Each call holds its own buffers and model, so concurrent reads add up: a caller
must allow only one `Read` at a time or budget the memory of each. The importer does the first: one gate per process
(`importer.ProcessGate`) allows one read at a time, held from the read until the parsed series are stored and the
model can be collected. An upload that can't get the gate within a few seconds is refused (`429` with `Retry-After`)
instead of queueing in memory.

## The preview (`internal/importer`)

`POST /me/imports` copies the upload to a private temporary file, hashing it on the way, and checks two things first,
without the gate:

- **The same file again?** If the user already has an import of a file with the same SHA-256 that didn't fail or get
  cancelled, they get that import back.
- **Another import running?** If one of the user's imports is still running, the upload is refused. A partial unique
  index on `imports` enforces this.

Then it takes the gate, reads the backup and deletes the file. It stores the import (`imports`, state `previewing`)
and one row per series in the backup's library (`import_items`), and queues the task `import.preview`. A file that
isn't a usable backup creates nothing.

A row keeps what later steps need, so nothing is uploaded twice:

- the title, author, artist, source (`mihon:<id>` and the backup's name for it), series key, date added and
  categories;
- the chapters with something to import (read, a page, a bookmark), as compact JSON. Each chapter's time is its
  last-read time; failing that, the chapter's fetch date; failing that, the series' date added. This way undated marks
  sort below real reading.

The task matches each series that is still `pending`, in file order, so a preview interrupted by a restart picks up
where it stopped. It finds the work in this order:

1. **The same series.** A work that already has this series (by key) attached on one of the Panelarr sources known
   by the backup's source ID: a re-import, or a series another user already has.
2. **The source's own details.** When the user may use that source, the source's title, alternative titles and
   people for the series, matched against the catalogue (`library.MatchEntry`, the same matching as adding a series
   found on a source).
3. **The backup's own title and people**, when the source isn't usable or doesn't answer.

Matching never guesses. A work counts as clear only with exactly one catalogue entry that has the exact title and a
shared author; anything else is a set of candidates. The catalogue is searched past the user's content rating limit,
so a series above the limit is reported as such instead of "not found". Entries above the limit are never offered as
candidates.

The outcome follows from the work found and the user's grants, the first that applies:

| Outcome | When |
|---|---|
| `not_allowed` | The work is above the user's content rating limit or only on sources they may not use |
| `in_library` | The work is in the user's library already. This works without `may_add` too: their reading can still be imported |
| `not_allowed` | The user may not add works (`may_add`), or the source exists in Panelarr but they may not use it |
| `ready` | The work is clear and the source is usable: it will be attached |
| `source_missing` | The work is clear but no plugin provides the source: it imports without it |
| `needs_choice` | Several catalogue entries could be it |
| `not_found` | Nothing in the catalogue |

**Package hints.** For sources no plugin provides, the preview asks each enabled plugin with packages which
packages it offers but hasn't installed, and matches their sources' `compat_ids` against the missing ones. The
import's summary then says, for example, "Multi Comics would let Panelarr attach 2 series". A plugin that doesn't
answer is skipped.

Progress (series matched out of the total) is computed from the rows, so it survives reloads and restarts.
`import.progress` and `import.finished` go to the importing user only. If the task ends without finishing an import,
for example because an admin cancelled it or it ran out of attempts, the import is marked `failed` the next time the
user's imports are read, so a dead import never blocks a new one.
