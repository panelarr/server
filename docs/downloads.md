# Downloads and storage

Downloaded chapters live in the **shared library**: each chapter is stored once, whoever asked for it, and every user's reader uses that file.

## Storage

Admins configure where files go (`/api/v1/storages`):

- **local**: a directory (which may be a mounted network share). On first start the server creates one at `<PANELARR_DATA>/library` as the default.
- **s3**: any S3-compatible service (endpoint, bucket, optional prefix and region, path-style addressing for most self-hosted services, access key and secret key; the secret is stored encrypted and never shown).

A storage is checked (write, read back, delete a probe) before it is saved. One storage is the default for new downloads. Files are addressed by storage and key, and the reader reads archives with ranged reads, so object storage works without downloading whole files. A storage with files can't be removed; move its files to another storage first (a task with progress). The default storage can't be removed either.

## Downloading

Users with the `may_download` grant (and admins) download chapters of a work: chosen chapters, all, or all unread. A chapter already downloaded or queued by anyone isn't queued again. Each download is a task: it fetches the pages of the chapter's chosen copy through the source's plugin, within the source's limits across all users, and writes a CBZ:

- pages stored uncompressed as `0001.jpg`, `0002.png`, …;
- `ComicInfo.xml` with series, number, volume, title, writer, artist, summary, genres, language, link, reading direction (`YesAndRightToLeft` for manga), age rating and Panelarr's IDs in `Notes`;
- built in a temporary file (`<PANELARR_DATA>/.tmp`), checksummed (SHA-256), then stored atomically: a page image that fails is first tried again like the reader's (see [the reader](reader.md)); a page that still fails fails the download and leaves nothing behind, and the task retries with growing waits (after the source's retry-after when it asked for one).

Files are named by the template in the instance setting `downloads.naming_template` (default `{work} [{work_id}]/{volume} {chapter} - {title} [{group}].cbz`; placeholders `{work}`, `{work_id}`, `{volume}`, `{chapter}`, `{title}`, `{group}`, `{lang}`). Names are made safe for SMB/NTFS shares; a name taken by another chapter gets the chapter's ID added.

**Download new chapters**: a user may turn this on per library entry; new chapters found by update checks are then queued.

`GET /api/v1/downloads` lists downloads with their state and progress (users see their own; admins everyone's with `all=true`), and the event stream reports progress and `chapter.downloaded`. Admins pause (the `download.chapter` task kind), cancel and retry downloads through the task endpoints, and delete a chapter's file. Removing a work from a library never deletes files.

Downloaded chapters are read from their file; the reader falls back to the source when the file's storage can't be reached.
