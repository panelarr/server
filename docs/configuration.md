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
| `PANELARR_METRICS_ENABLED` | `--metrics` | `false` | Serve Prometheus metrics. |
| `PANELARR_METRICS_LISTEN` | `--metrics-listen` | (empty) | Address of the metrics listener, e.g. `127.0.0.1:9464`. Required while metrics are on, so they are never open on the API port. |

Flags win over environment variables.

## Logs

Every line carries `time`, `level`, `msg`, `service` (`server`) and, where it applies, `component` and `request_id`. Passwords, API keys, session tokens, cookies and query strings are never logged.

## Health

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | Liveness: the process serves HTTP. Always 200 while it runs. |
| `GET /readyz` | Readiness: 200 when every required dependency (the database) works, 503 otherwise. Optional dependencies only mark the server `degraded` and keep it ready. Errors are not shown; it needs no authentication. |

`panelarr healthcheck` probes `/healthz` for container health checks in images without a shell.

## Database migrations

The server applies pending migrations on start. `panelarr migrate [up|down|status]` does the same by hand; `down` rolls back the latest migration.
