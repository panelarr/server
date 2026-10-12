# The reader API

What the reader needs comes from the server; the browser never contacts a site.

- **Pages.** `GET /api/v1/chapters/{id}/pages` lists a chapter's pages from its chosen copy, with the previous and next chapters in reading order and your progress. Pass `variant_id` to read another copy. When the chosen copy is on a source you may not use, the first copy you may use is read instead. Page images (`…/pages/{index}`) are fetched through the source's plugin, within the source's request rate, and may be cached by the browser for a day. Page lists are kept for 15 minutes. Downloaded chapters will be served from their files.
- **Progress** is saved per user and chapter (`PUT /api/v1/chapters/{id}/progress`). Reaching the last page marks the chapter read. Your other devices receive a `progress.synced` event. `GET /api/v1/works/{id}/progress` says where to continue: the chapter you read last if unfinished, else the first unread chapter after it. Chapters can be marked read or unread in bulk.
- **Bookmarks** are per user: bookmark one chapter or many at once, or remove them (`POST /api/v1/works/{id}/bookmarks`). Chapters say whether you bookmarked them, the chapter list can show only bookmarked ones (`?bookmarked=true`), and library entries count them. Your other devices receive a `bookmarks.changed` event.
- **History** lists the chapters you read, newest first.
- **Reader settings** live at three levels: global, per source and per work. A more specific level's keys win. The server stores them as JSON objects (up to 16 KiB) and doesn't interpret them: the web app owns their shape (zoom, fit, mode, direction, …).
- **Preferences** (theme, colours, fonts) are stored per user the same way and follow you across devices.
