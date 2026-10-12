# Configuration

Panelarr follows the *arr* convention: only what the server needs before it can reach its database is set through environment variables (or flags). Everything else is managed in the app and stored in the database.

| Environment | Flag | Default | Meaning |
|---|---|---|---|
| `PANELARR_DB` | `--db` | (required) | PostgreSQL URL, e.g. `postgres://panelarr:secret@db:5432/panelarr`. The role needs to own its database (or have `CREATE` on it); no superuser. |
| `PANELARR_LISTEN` | `--listen` | `:8900` | HTTP listen address. |
| `PANELARR_CONFIG` | `--config-dir` | `<user config dir>/panelarr` | Directory for the server's own files: the encryption key for stored secrets, caches, logs. Use `/config` in containers. |
| `PANELARR_DATA` | `--data-dir` | `<config dir>/data` | The default storage's directory (`<data>/library`, created on first start) and temporary files of downloads in progress (`<data>/.tmp`). Use `/data` in containers. |
| `PANELARR_BASE_URL` | `--base-url` | (empty) | Public URL, e.g. `https://panelarr.example.com`. With `https`, session cookies are marked `Secure`. |
| `PANELARR_LOG_FORMAT` | `--log-format` | `json` | `json` (one object per line) or `text`. |
| `PANELARR_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn` or `error`. |
| `PANELARR_SECRET_KEY` | | (empty) | Key that encrypts stored secrets (service and plugin keys, storage credentials): base64 of 32 bytes, e.g. `openssl rand -base64 32`. Empty: a key file `secret.key` in the config directory, created on first use. Back it up with the database: without it, stored secrets can't be read. |
| `PANELARR_META_URL` | | (empty) | The metadata service, e.g. `http://meta:8901`. Set it (with `PANELARR_META_KEY`) to fix the connection in the environment; leave it empty to connect the service in the first-run wizard instead. |
| `PANELARR_META_KEY` | | (empty) | An API key of the metadata service with the `admin` scope. |
| `PANELARR_WEB_DIR` | | (empty) | Serve the web app from this directory (a local build) instead of the one built into the binary. For development. |
| `PANELARR_TRUSTED_PROXIES` | `--trusted-proxies` | loopback and private networks | Comma-separated networks (or addresses) of reverse proxies whose `X-Forwarded-For` header is believed, or `none`. The client address is used for login limits and the session list. |
| `PANELARR_METRICS_ENABLED` | `--metrics` | `false` | Serve Prometheus metrics. |
| `PANELARR_METRICS_LISTEN` | `--metrics-listen` | (empty) | Address of a separate metrics listener, e.g. `127.0.0.1:9464`. Empty: `/metrics` is served on the API port, only to API keys with the `metrics:read` scope. |

Flags win over environment variables.

## The web app

The server serves the Panelarr web app at its root, built into the binary (`make web` embeds a pinned web release, `make web-local` a local build; without either, a placeholder page says so and the API works as usual). Any path that isn't the API (`/api/`), a feed (`/opds/`), an asset (`/assets/`), a probe or `/metrics` gets the app's page, which routes in the browser. Content-hashed assets are cached for a year; everything else is revalidated. The app is sent with a strict Content-Security-Policy (same-origin scripts only), `nosniff`, `no-referrer` and no framing; API responses carry `nosniff`, a `default-src 'none'` policy and no framing too.

## Logs

Every line carries `time`, `level`, `msg`, `service` (`server`) and, where it applies, `component` and `request_id`. Passwords, API keys, session tokens, cookies and query strings are never logged.

## Health

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | Liveness: the process serves HTTP. Always 200 while it runs. |
| `GET /readyz` | Readiness: 200 when every required dependency (the database) works, 503 otherwise. Optional dependencies only mark the server `degraded` and keep it ready. Errors are not shown; it needs no authentication. |
| `GET /api/v1/system/health` | Detailed health for admins: every component, whether it's required, its latency and its error. |

`panelarr healthcheck` probes `/healthz` for container health checks in images without a shell.

## The metadata service

Panelarr's library needs the metadata service: every work is created from one of its entries. The server talks to it over gRPC with an admin-scoped key (stored encrypted). Until the service is reachable and has a provider with entries, the server reports `degraded` (still ready) and catalogue calls answer `503 meta_unavailable`; reading what's already downloaded keeps working. Admins see what's missing at `GET /api/v1/system/meta`.

Entries without a known content rating are shown to users by default. To hide them, set the instance setting `catalog.hide_unknown_ratings` to `true` (an admin page follows).

Covers are fetched by the server and cached under `<config>/cache/images` (safe to delete).

## Source plugins

Sources come from plugins that run as their own services. An admin adds a plugin by its URL (and API key, stored encrypted); the server checks it speaks the source plugin contract, lists its sources and refreshes them with their health every five minutes. A plugin's settings are edited in the app, kept encrypted, and handed back to the plugin whenever it restarts. A plugin that is down marks the server `degraded`; its sources stay known. A plugin found down is rechecked after 10 seconds, 30 seconds and a minute (then the regular sync takes over), so one that was only restarting shows as ok again within seconds; at startup, a plugin that doesn't answer yet gets a few seconds before it is marked down.

Each source's ID is `<plugin>:<source>`. Admins turn sources on or off for everyone; each user's grants (allowed sources, content-rating ceiling) decide which they see. Mixed-content sources count as "suggestive", adult-only sources as "pornographic". Images from sources (covers, icons) are fetched through the plugin and cached by the server; their links are signed, so the server never fetches an address a caller made up.

## Background work

Two kinds of background work show in the admin:

- **Jobs** are named and run on a schedule or by hand, one run at a time: `plugins.sync` (every 5 minutes), `sessions.cleanup` (hourly), `tasks.prune` (every 6 hours; finished tasks are kept a week, failed ones until retried). Each job keeps its last 100 runs with their outcome, counts and notes. A run left running when the server stopped is marked failed ("interrupted").
- **Tasks** are single pieces of work queued by features (matching a work, checking it for updates, downloading a chapter). They retry with growing waits (30 seconds up to an hour), respect a source's "retry after" hint, and fail after their kind's attempts. The same work is never queued twice. At most 8 tasks run at once, each kind has its own cap, and each source caps how many of its tasks run at once and how fast they call it, across all users (from the plugin's declared limits, else 2 at once and 2 requests a second). Admins pause and resume kinds, cancel and retry tasks. Tasks interrupted by a restart go back to the queue.

Live changes reach clients over `GET /api/v1/events` (server-sent events), filtered per user.

## Accounts

The first account created (through the first-run wizard) is the admin. Admins add users with a password or send invite links, and set each user's role and grants. Passwords are hashed with argon2id. Failed logins are limited per username (5 per 15 minutes) and per client address (30 per 15 minutes).

## Database migrations

The server applies pending migrations on start. `panelarr migrate [up|down|status]` does the same by hand; `down` rolls back the latest migration.
