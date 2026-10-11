# Progress

The plan for the server's first version, in slices. Each slice works end to end, has tests and ends with `make check` green. The first version covers: accounts and first-run setup, adding a work from the metadata service and matching it on sources, unified chapters, the reader API and downloads.

- [x] **0. Scaffold:** Go module, `make` targets, lint, CI, release config, repo docs. `panelarr serve | migrate | healthcheck | version`; env and flag config; JSON/text logs; Prometheus metrics on a private listener (off by default); `/healthz` and `/readyz` with required and optional checks; PostgreSQL with goose, sqlc and pgx; spec-first REST API (`api/openapi.yaml`, oapi-codegen strict server) with RFC 9457 problem errors and a breaking-change check.
- [x] **1. Accounts and setup:** users (argon2id), roles admin and user, grants (`may_add`, `may_download`, `may_manage_matches`, allowed sources, content-rating ceiling); cookie sessions with CSRF protection; scoped personal API keys (hashed, shown once); rate-limited login; first-run gate (the first account becomes the admin); users admin; invite links; secrets encryption (used from slice 2); authorization from the spec with a test matrix over every operation; request validation against the spec; detailed health for admins; metrics via a `metrics:read` key.
- [x] **2. Metadata service:** connection over gRPC (admin key encrypted; or fixed by `PANELARR_META_URL`/`PANELARR_META_KEY`) and the wizard's check (reachable, key valid, admin scope, a provider with entries), with what to do when not ready and a "start sync" action; degraded (not down) while it isn't ready; catalogue search, browse and details through the server, filtered by the user's ceiling (unrated entries shown unless hidden); covers fetched and cached by the server (no SVG); authorization now runs before the generated code parses parameters.
- [x] **3. Source plugins (remote):** register plugin services (checked: reachable, speaks the contract), keys and settings encrypted, settings handed back after a plugin restarts (secret fields kept when left empty); sources synced with health every 5 minutes, absent ones kept; sources on/off; per-user visibility (allowed sources, ceiling, unknown ratings); popular, latest, search with filters, series, chapters and images through the server (signed, cached image links); plugin errors mapped (rate limits → 429 with Retry-After, site failures → 502 with the reason); degraded while a plugin is down.
- [x] **4. Jobs and events:** task queue on PostgreSQL (SKIP LOCKED claims, leases with heartbeat, retries with back-off and retry-after hints, permanent failures, de-duplication, per-kind and per-source caps, cancel, retry, pause, restart-safe); per-source limits from plugins' declared limits; named jobs with runs (schedules, params, cancel, skipped runs, retention, interrupted runs) following the shared jobs model; plugin sync and cleanups as jobs; the event stream with per-user audiences; job and task metrics.
- [ ] **5. Works, libraries and matching:** add from the catalogue (one shared work per entry, one library entry per user); match job over every allowed source by main and alternative titles; auto-attach and candidates with evidence; fix match (attach, detach, re-run, paste URL); add from a source's browse page.
- [ ] **6. Unified chapters and update checks:** canonical chapters across sources with a chosen variant and the reason; pins and per-work source priority; scheduled and manual update checks; per-user "new" badges.
- [ ] **7. Reader API:** page lists and page streaming (from the archive or proxied from the source); progress and history; next chapter; reader settings per user (global, per source, per series) and UI preferences.
- [ ] **8. Storage and downloads:** storage endpoints (local and S3-compatible); CBZ + ComicInfo writer; de-duplicated download queue; downloads API with live progress; "download new chapters"; reader serves archives from any storage.
- [ ] **9. Hardening and packaging:** authorization review, docs, embedded web app, container image (to be agreed first).

## Working assumptions

Settled for now, cheap to change:

- Matching auto-attaches on an exact normalized title (any language) plus an author overlap; everything else becomes a candidate. Thresholds are settings.
- Variant choice: preferred languages first, then healthy sources, per-work source priority, preferred scanlators, latest upload. Settings, not code.
- Chapters group by normalized number; the volume is an attribute. Chapters without a number stay per source.
- Storage: local and S3-compatible first; several named storages with one default.
- Content ratings use the metadata service's scale; source ratings map onto it; "unknown" is shown unless an admin hides it.
- The sibling modules (`source-api`, `meta`) are used through `replace` directives to local clones until they are published.

## Next

Slice 5: works, libraries and matching.
