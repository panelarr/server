# The API

The Panelarr server's REST API is a **public client API**: the web app is one client among others, and everything it does goes through the same documented endpoints.

- **Contract:** [`api/openapi.yaml`](../api/openapi.yaml) (OpenAPI 3.1). A running server also serves it at `GET /api/v1/openapi.json`.
- **Base path:** `/api/v1`. Breaking changes need `/api/v2`; within v1, endpoints and fields are only added. CI checks every change against the base branch.
- **Errors:** [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem details (`application/problem+json`) with a stable `code` such as `not_found` or `invalid_request`, the `request_id` and, for validation errors, per-field `errors`. Unexpected errors are answered with a bare `internal` problem; details go to the server log under the same request ID.
- **Request IDs:** every response carries `X-Request-Id`. Send your own (letters, digits, `.`, `_`, `-`, up to 64 characters) to correlate calls.
- **Lists** are cursor-paginated (`page_token` → `next_page_token`).

## Authentication

- **Browser sessions.** `POST /api/v1/auth/login` (or the first-run `POST /api/v1/setup/admin`, or accepting an invite) sets two cookies: `panelarr_session` (HTTP-only) and `panelarr_csrf` (readable). Every request that changes something (not GET, HEAD or OPTIONS) must echo the CSRF token in the `X-CSRF-Token` header. Cross-site requests are refused. Sessions last 30 days from their last use. When single sign-on is configured (`GET /api/v1/auth/methods` says so), a browser navigation to `GET /api/v1/auth/oidc/start` signs in with the identity provider and ends with the same two cookies (a signed-in user links a login with `POST /api/v1/auth/oidc/link` and sends the browser to its `redirect_to`); `return_to` must be a page outside `/api`; see [configuration](configuration.md#single-sign-on-oidc). With provider logout configured, `POST /api/v1/auth/logout` of such a session answers `200` with a `redirect_to` to send the browser to.
- **Personal API keys** (`pnl_…`), created by a signed-in user under `/api/v1/me/api-keys`. Send `X-Api-Key: <key>` or `Authorization: Bearer <key>`; no CSRF token is needed. Scopes: `read` (GET), `write` (changes), `admin` (admin endpoints; admins only), `metrics:read` (`/metrics`; admins only). A key never holds more than its owner's current role allows. Changing the password and managing sessions and keys need a browser session.
- Sending an invalid key is an error (`401 invalid_api_key`), never a silent fallback to anonymous.

Every operation in the spec declares who may call it (`x-panelarr-access`: `public`, `user` or `admin`; `x-panelarr-session-only`). The server enforces exactly that, and a test calls every operation as each kind of caller.

## First run

Until the first account exists, every endpoint except `GET /system/info`, `GET /setup` and `POST /setup/admin` answers `409 setup_required`. `GET /system/info` tells clients whether setup is needed (`setup_required`).

## The catalogue

`/catalog/search`, `/catalog/browse` and `/catalog/entries/{id}` show the metadata service's catalogue, limited to the caller's content-rating ceiling (entries above it answer `404` like missing ones). Covers come from `/catalog/entries/{id}/cover`, served by Panelarr, never from the metadata sites. Text comes in the `lang` parameter's language, else the `Accept-Language` header's.

## Sources

Sources are an admin concern. Listing sources (`GET /sources`) and browsing one directly (its popular and latest lists, search, filters, a series and its chapters under `/sources/{source_id}/…`) are admin endpoints, so an API key needs the `admin` scope for them. Browsing is the admin's test view: it also works on a source turned off (to try it before turning it on), but not on one its plugin no longer lists (absent, e.g. its package uninstalled or its language turned off) or whose plugin is turned off; those answer `404`. Everything else (search for a work, matching, chapter checks, the reader, downloads) uses only sources that are on, listed and of a plugin that is on.

Users with `may_manage_matches` search one of a work's sources by hand with `POST /works/{work_id}/sources/{source_id}/search`: the same request and results as the source search, each result marked with `match` when the work already knows it. The source must be usable and within the caller's source grants. Regular users see sources only where they belong to a work: `GET /works/{work_id}` lists the work's sources with their language and health, and `GET /works/{work_id}/series/{series_id}/chapters` lists one source's chapters of it. A user's source grants (`all_sources`, `allowed_sources`) decide which sources may serve their works. Two source endpoints stay open to users: `GET /sources/{source_id}/image`, which only serves links the server signed (a work's series covers, and sources' icons), and `POST /library/from-source`, which still checks the caller's grants and `may_add`.

## Saved library views

Each user keeps named views of their own library under `/me/library-views`: a name, and a `LibraryViewDefinition` with tri-state filters (`include` or `exclude` for `unread`, `started`, `downloaded`, `bookmarked` and `new`), work types and statuses, content ratings, `updated_within_days`, a sort, `reverse` and display settings. The server only stores and validates views; clients apply them to the library (`GET /library` is unchanged). Unknown keys and values are refused, so a new filter or sort needs a server that knows it.

- `GET /me/library-views` lists the caller's views in their order; `POST` adds one at the end (`201`); `GET`, `PATCH` and `DELETE /me/library-views/{id}` read, change and delete one; `PUT /me/library-views/order` takes all of the caller's view IDs in the new order.
- Every user with views has exactly one default. The first view is the default; `default: true` on create or change moves the mark. Deleting the default passes it to the first remaining view.
- Limits: 50 views, names of 1 to 60 characters (trimmed) unique per user ignoring case, definitions of at most 4 KiB, at most 20 types and 20 statuses of up to 40 characters each, `updated_within_days` from 1 to 365.
- `content_ratings` keeps only works with those ratings (`work.content_rating`, the meta rating the ceiling also uses): any of `safe`, `suggestive`, `erotic`, `pornographic` and `unknown` (no rating in the metadata), each at most once. Empty or absent means any rating; an empty list is stored as absent, so definitions saved before the filter existed are unchanged. A view only narrows the library: `GET /library` never lists works above the caller's content-rating ceiling, so a view can't bring them back. Values above the ceiling are accepted (an admin may lower it later) and match nothing.
- Error codes (all `409` except validation): `name_taken`, `view_limit`, `default_required` (`default: false` on the default view), `last_view` (deleting the only view), `order_mismatch` (the order doesn't list each view exactly once). Invalid names and definitions answer `400 invalid_request`.
- Views are private: another user's view answers `404` on every call, like a missing one, and there is no admin API for them. Deleting a user deletes their views.
- Changes publish `library_views.changed` on the owner's event streams only; other devices refetch the list.

## Imports

Each user imports their own library and reading state from a Suwayomi or Mihon backup under `/me/imports` (tag `imports`); see [import.md](import.md) for how series are matched.

- `POST /me/imports` takes the backup as the multipart field `file`. This one operation accepts bodies up to 64 MiB (`413 too_large` above); every other operation keeps the 1 MiB limit. The server parses the file during the request and keeps only the parsed series, never the file. A file that isn't a usable backup answers `422 bad_backup` and creates nothing.
- The answer is `202` with an import in state `previewing`. The preview (task `import.preview`) matches each series in the background; the import then moves to `preview`, or `failed` with a `message`. Nothing in the library changes until the import is applied.
- Uploading the same file again (same SHA-256) answers `200` with the earlier import, unless that one failed or was cancelled. While another import of the user is running, `409 import_running`. Each user keeps at most 20 imports: with 20, `409 too_many_imports` (delete one first).
- The server reads one backup at a time, because a read may take a few hundred MiB. When another read is in progress, the upload waits a few seconds and then answers `429 busy` with `Retry-After`.
- `GET /me/imports` lists the caller's imports, newest first, in pages (`page_size`, `page_token`). `GET /me/imports/{id}` returns one, with the backup's totals (series, chapters read, bookmarks, categories, tracker links), progress (series matched out of the total, or applied while applying), series per outcome and package hints. `GET /me/imports/{id}/items?outcome=` pages through its series in file order. `DELETE /me/imports/{id}` deletes the record (stopping a preview that is still running) but not what the import added.
- `POST /me/imports/{id}/apply` starts applying an import in state `preview` (task `import.apply`): `202` with the import in state `applying`, then `done`. A cancelled or failed import whose preview finished can be applied again; it resumes. The user's grants are checked again as they are then. Another state answers `409 import_state`; another running import of the user, `409 import_running`.
- `POST /me/imports/{id}/cancel` stops a preview or an apply: `200` with the import `cancelled`. What was applied stays, and applying again resumes. An import that isn't running answers `409 import_not_running`.
- `POST /me/imports/{id}/items/{idx}/resolve` with `{"entry_id": …}` gives a `needs_choice`, `not_found` or skipped series its catalogue entry; with `{"skip": true}` it leaves out a series not applied yet. Before apply, the choice goes with the rest; after it, the series is applied right away. `400` on `entry_id` for an entry the user may not see, `422 not_allowed` for one they may not add, `409 import_running` while the import runs, `409 import_item_state` for a series in another state.
- Outcomes per series: `pending`, `ready`, `in_library`, `needs_choice` (with candidates), `not_found`, `source_missing`, `not_allowed` (with a `message`). What apply did is the series' `result`: `applied`, `already_imported` or `skipped` (filter with `?result=`; the import counts them in `results`). Each series also says how many of its chapters were `placed`, are `pending` (waiting for the work's chapters, up to 24 hours) or `unplaced`.
- Imports are private: another user's import answers `404` on every call. `import.progress` and `import.finished` go only to the owner's event streams.

## Changes within v1

Endpoints and fields are only added within v1, with one deliberate exception:

- **Admin-only sources.** `GET /sources` and the source browsing endpoints (`/sources/{source_id}/popular`, `/latest`, `/search`, `/filters`, `/series`, `/chapters`) used to be open to every user and now answer `403 forbidden` to non-admins. Sources are managed and browsed by admins only; users reach them through their works (see [Sources](#sources)).

## Validation

Requests are checked against the spec before they reach a handler: wrong types, missing fields, unknown enum values, lengths and patterns answer `400 invalid_request` with one `errors` entry per field.

Text can't contain the NUL character (U+0000): PostgreSQL can't store it. A NUL in a path or query parameter, or in any string or key of a JSON body (`\u0000`), answers `400 invalid_request` naming the field (e.g. `definition.types.0`), for every operation, before the spec check. Uploads (operations with `x-panelarr-max-body`, such as `POST /me/imports`) stream to their handler: their body is neither buffered for this check nor validated against the spec, and the handler checks it.
