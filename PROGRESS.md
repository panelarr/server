# Progress

The plan for the server's first version, in slices. Each slice works end to end, has tests and ends with `make check` green. The first version covers: accounts and first-run setup, adding a work from the metadata service and matching it on sources, unified chapters, the reader API and downloads.

- [x] **0. Scaffold:** Go module, `make` targets, lint, CI, release config, repo docs. `panelarr serve | migrate | healthcheck | version`; env and flag config; JSON/text logs; Prometheus metrics on a private listener (off by default); `/healthz` and `/readyz` with required and optional checks; PostgreSQL with goose, sqlc and pgx; spec-first REST API (`api/openapi.yaml`, oapi-codegen strict server) with RFC 9457 problem errors and a breaking-change check.
- [x] **1. Accounts and setup:** users (argon2id), roles admin and user, grants (`may_add`, `may_download`, `may_manage_matches`, allowed sources, content-rating ceiling); cookie sessions with CSRF protection; scoped personal API keys (hashed, shown once); rate-limited login; first-run gate (the first account becomes the admin); users admin; invite links; secrets encryption (used from slice 2); authorization from the spec with a test matrix over every operation; request validation against the spec; detailed health for admins; metrics via a `metrics:read` key.
- [x] **2. Metadata service:** connection over gRPC (admin key encrypted; or fixed by `PANELARR_META_URL`/`PANELARR_META_KEY`) and the wizard's check (reachable, key valid, admin scope, a provider with entries), with what to do when not ready and a "start sync" action; degraded (not down) while it isn't ready; catalogue search, browse and details through the server, filtered by the user's ceiling (unrated entries shown unless hidden); covers fetched and cached by the server (no SVG); authorization now runs before the generated code parses parameters.
- [x] **3. Source plugins (remote):** register plugin services (checked: reachable, speaks the contract), keys and settings encrypted, settings handed back after a plugin restarts (secret fields kept when left empty); sources synced with health every 5 minutes, absent ones kept; sources on/off; per-user visibility (allowed sources, ceiling, unknown ratings); popular, latest, search with filters, series, chapters and images through the server (signed, cached image links); plugin errors mapped (rate limits → 429 with Retry-After, site failures → 502 with the reason); degraded while a plugin is down.
- [x] **4. Jobs and events:** task queue on PostgreSQL (SKIP LOCKED claims, leases with heartbeat, retries with back-off and retry-after hints, permanent failures, de-duplication, per-kind and per-source caps, cancel, retry, pause, restart-safe); per-source limits from plugins' declared limits; named jobs with runs (schedules, params, cancel, skipped runs, retention, interrupted runs) following the shared jobs model; plugin sync and cleanups as jobs; the event stream with per-user audiences; job and task metrics.
- [x] **5. Works, libraries and matching:** add from the catalogue (one shared work per entry, one library entry per user); matching fans out per source (source limits apply), searches main then alternative titles by the source's language until candidates appear, scores each result's title against every title of the entry (sources may carry only one), fetches details (people, chapter count) of the best; auto-attaches a single exact-title + shared-author result, keeps the rest as candidates with evidence; fix match (attach, detach = reject for good, priority, by source and key, by URL, re-run); add from a source's browse page (clear entry → added with the series, else candidates); per-user visibility of works; library listing; work covers.
- [x] **6. Unified chapters and update checks:** each attached series' chapters kept as listed (per-source tab); canonical chapters by normalized number (numbers read from names when missing; volume by majority; unnumbered chapters kept as extras); a chosen copy per chapter with the reason (pin, preferred languages, healthy source, series priority, newest upload); chapters no source lists stay but unavailable; checks per series as tasks within source limits (scheduled every 6 hours with jitter, on demand, and when a series is attached); per-user "new" badges and events; works refreshed from the catalogue daily (merged entries logged).
- [x] **7. Reader API:** page lists and page images through the source's plugin (chosen copy, a named copy, or the first copy the user may use; source rate limits apply; page lists cached briefly); previous/next chapter in unified order; progress per user (last page marks read, synced to other devices by event), bulk read/unread, "continue" point, history; reader settings at global, source and work level (merged) and app preferences as client-owned JSON; chapter bookmarks per user (one or many at once, flagged and filterable in the chapter list, counted in the library). Queued work asked for again comes forward (no waiting out a retry back-off).
- [x] **8. Storage and downloads:** storage endpoints (local, S3-compatible; checked by a probe; default; encrypted secret; moving files between storages as a task; guarded removal); CBZ writer (stored pages, ComicInfo.xml, SHA-256, atomic, nothing partial left) with a naming template safe for SMB/NTFS; one download task per chapter, de-duplicated across users, within each source's limits; "download new chapters" per library entry; downloads list (own / all) with live progress; admin file deletion; the reader serves downloaded chapters from their file on any storage (ranged reads) and falls back to the source. Fixed a deadlock between concurrent chapter checks of one work (writes now take the work's lock).
- [x] **9. Hardening and packaging:**
  - [x] embedded web app per the web app's contract (root SPA fallback except API, feeds, assets, probes and metrics; immutable assets; its security headers; `make web` for a pinned release with checksum, `make web-local`, `PANELARR_WEB_DIR`);
  - [x] authorization and security review: every endpoint is covered by the matrix; API responses get `nosniff`, a `default-src 'none'` CSP and no framing; page images limited to raster types (no SVG from plugins or archives);
  - [x] container image: Dockerfile (distroless, non-root, `/config` and `/data`, port 8900, health check; sibling modules and the web build as named build contexts) and Dockerfile.release; GoReleaser publishes multi-arch `panelarr/server`. The full-stack compose lives outside this repo.

## Extensions, source languages, admin-only sources

Sources become an admin concern; plugins that host many sites get an extensions (packages) API with languages.

- [x] **S1. Admin-only sources:** listing sources and browsing one directly (popular, latest, search, filters, series, chapters) are admin endpoints; users see sources only on their works (sources with language and health, a series' chapters, covers). Signed source images and adding from a source stay open to users within their grants. The one deliberate break within v1, noted in the spec and [docs/api.md](docs/api.md).
- [x] **S2. Source filters, absent sources, work-scoped search:** `GET /sources` filters by plugin, enabled and present; sources turned off, absent or of a plugin turned off are skipped by matching, URL resolving, update checks (skipped, not failed), reading and downloads, while admins may still browse a source turned off to test it; chapter copies prefer usable sources and are chosen again for the affected works when a source changes, so works keep their history and move to other sources until it is back; `POST /works/{work_id}/sources/{source_id}/search` for users who may fix matches (within their source grants), results marked when the work knows them.
- [x] **S3. Packages:** for plugins with the `packages` capability, list what they offer (languages, versions, updates, content rating, offers, sources; filtered by the plugin) with the pending task per package; install (from a chosen offer), update and uninstall as `plugin.package` tasks (one at a time, unknown packages refused up front, bad packages fail for good); per-package languages; sources carry their providing package (filter by it), and an absent source whose language is turned off says so (`unavailable_reason: language`, "not available") apart from one no longer provided; the plugin is synced right after every change and again shortly after. Store failures map to `store_fetch_failed` / `store_invalid`, a refused request to `package_rejected`; a task keeps the plugin's reason and fails at once unless the store just couldn't be reached. Package icons through the server (signed links, the plugin's key only to its own address, raster up to 2 MiB, refreshed weekly).

## Shared network services and plugin settings

Solvers, proxies and network profiles live in Panelarr as options; admins assign profiles to a whole plugin, an extension, a source or a metadata provider (the most specific wins), and the server pushes them to plugins that can use them.

- [x] **S1. Solvers and proxies:** admin CRUD under `/network/solvers` and `/network/proxies`, checks before and after saving (a solver's state; a proxy's connection or a request through it), proxy passwords encrypted and never returned.
- [x] **S2. Profiles and assignments:** profiles built from them (shared or custom to one target), assignments at global, plugin, extension, source, meta and provider level, and the effective profile per source and provider.
- [x] **S3. Push to source plugins:** effective profiles pushed to plugins that declare the network profile service (solver only with the solver service), on change, for new sources and after a plugin restart; push state per source; the server's own source limits follow the profile.
- [x] **S4. Per-source settings:** a source's own settings through the API.
- [x] **S5. Metadata providers:** profiles pushed to meta per provider.

## Connected services' admin

Everything an admin does in mihon-source's or meta's own admin can be done from Panelarr's admin, on that plugin's page, apart from their API clients and keys. mihon-source is reached through source-mihon with generic plugin capabilities; meta through its admin API with the key Panelarr holds.

- [ ] **S1. Meta admin:** providers and sync runs (done), jobs (the shared jobs view; done), duplicates review and its judge (done), the search engine.
- [x] **S2. Custom solvers and proxies:** solvers and proxies custom to one plugin, extension, source or provider, like profiles; usable only by that owner's profiles and removed with its plugin.
- [ ] **S3. Plugin jobs:** the shared jobs view for plugins that declare jobs (source.v1).
- [ ] **S4. Package origins and details:** stores' status and refresh, package load details.

## Working assumptions

Settled for now, cheap to change:

- Matching auto-attaches on an exact normalized title (any language) plus an author overlap; everything else becomes a candidate. Thresholds are settings.
- Variant choice: preferred languages first, then healthy sources, per-work source priority, preferred scanlators, latest upload. Settings, not code.
- Chapters group by normalized number; the volume is an attribute. Chapters without a number stay per source.
- Storage: local and S3-compatible first; several named storages with one default.
- Content ratings use the metadata service's scale; source ratings map onto it; "unknown" is shown unless an admin hides it.
- `POST /library/from-source` stays open to users (grants and `may_add` apply) although source browsing is for admins.
- The server keeps no language setting of its own: the plugin's settings and each package's settings decide which sources a plugin lists.
- The sibling modules (`source-api`, `meta`) are used through `replace` directives to local clones until they are published.

## Next

The first version is complete. Now: extensions, source languages and admin-only sources (above). Then what the plan brings (managed plugins, network profiles, search, trackers, backups).
