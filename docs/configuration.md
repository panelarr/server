# Configuration

Panelarr follows the *arr* convention: only what the server needs before it can reach its database is set through environment variables (or flags). Everything else is managed in the app and stored in the database.

| Environment | Flag | Default | Meaning |
|---|---|---|---|
| `PANELARR_DB` | `--db` | (required) | PostgreSQL URL, e.g. `postgres://panelarr:secret@db:5432/panelarr`. The role needs to own its database (or have `CREATE` on it); no superuser. |
| `PANELARR_LISTEN` | `--listen` | `:8900` | HTTP listen address. |
| `PANELARR_CONFIG` | `--config-dir` | `<user config dir>/panelarr` | Directory for the server's own files: the encryption key for stored secrets, caches, logs. Use `/config` in containers. |
| `PANELARR_DATA` | `--data-dir` | `<config dir>/data` | Default library location for downloads. Use `/data` in containers. |
| `PANELARR_BASE_URL` | `--base-url` | (empty) | Public URL, e.g. `https://panelarr.example.com`. With `https`, session cookies are marked `Secure`. |
| `PANELARR_LOG_FORMAT` | `--log-format` | `json` | `json` (one object per line) or `text`. |
| `PANELARR_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn` or `error`. |
| `PANELARR_SECRET_KEY` | | (empty) | Key that encrypts stored secrets (service and plugin keys, storage credentials): base64 of 32 bytes, e.g. `openssl rand -base64 32`. Empty: a key file `secret.key` in the config directory, created on first use. Back it up with the database: without it, stored secrets can't be read. |
| `PANELARR_TRUSTED_PROXIES` | `--trusted-proxies` | loopback and private networks | Comma-separated networks (or addresses) of reverse proxies whose `X-Forwarded-For` header is believed, or `none`. The client address is used for login limits and the session list. |
| `PANELARR_METRICS_ENABLED` | `--metrics` | `false` | Serve Prometheus metrics. |
| `PANELARR_METRICS_LISTEN` | `--metrics-listen` | (empty) | Address of a separate metrics listener, e.g. `127.0.0.1:9464`. Empty: `/metrics` is served on the API port, only to API keys with the `metrics:read` scope. |

Flags win over environment variables.

## Logs

Every line carries `time`, `level`, `msg`, `service` (`server`) and, where it applies, `component` and `request_id`. Passwords, API keys, session tokens, cookies and query strings are never logged.

## Health

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | Liveness: the process serves HTTP. Always 200 while it runs. |
| `GET /readyz` | Readiness: 200 when every required dependency (the database) works, 503 otherwise. Optional dependencies only mark the server `degraded` and keep it ready. Errors are not shown; it needs no authentication. |
| `GET /api/v1/system/health` | Detailed health for admins: every component, whether it's required, its latency and its error. |

`panelarr healthcheck` probes `/healthz` for container health checks in images without a shell.

## Accounts

The first account created (through the first-run wizard) is the admin. Admins add users with a password or send invite links, and set each user's role and grants. Passwords are hashed with argon2id. Failed logins are limited per username (5 per 15 minutes) and per client address (30 per 15 minutes).

## Database migrations

The server applies pending migrations on start. `panelarr migrate [up|down|status]` does the same by hand; `down` rolls back the latest migration.
