# Panelarr server

The core of [Panelarr](https://github.com/panelarr), a self-hosted, *arr*-style app for curating, downloading, managing and reading manga, manhwa, manhua and comics, with real multi-user support.

The server owns users and their libraries, works and the sources matched to them, chapters, downloads and the job queue. It serves the REST API that the Panelarr web app (and any other client) uses. Metadata comes from the Panelarr metadata service; sources come from optional, out-of-process source plugins.

> **Status:** early development. The first version is being built in slices; see [PROGRESS.md](PROGRESS.md). Nothing is released yet.

## Quick start (from source)

You need Go (the version in `go.mod`) and a PostgreSQL database.

```sh
make setup build
export PANELARR_DB='postgres://panelarr:secret@localhost:5432/panelarr?sslmode=disable'
./bin/panelarr serve
curl http://localhost:8900/api/v1/system/info
```

The server migrates its database on start. A container image will follow.

## Configuration

Startup settings come from `PANELARR_*` environment variables or flags; everything else is managed in the app. See [docs/configuration.md](docs/configuration.md).

| Variable | Default | |
|---|---|---|
| `PANELARR_DB` | (required) | PostgreSQL URL |
| `PANELARR_LISTEN` | `:8900` | HTTP listen address |
| `PANELARR_CONFIG` | user config dir | the server's own files (`/config` in containers) |
| `PANELARR_DATA` | `<config>/data` | default library location (`/data` in containers) |
| `PANELARR_BASE_URL` | | public URL |
| `PANELARR_LOG_FORMAT` / `PANELARR_LOG_LEVEL` | `json` / `info` | logging |
| `PANELARR_METRICS_ENABLED` / `PANELARR_METRICS_LISTEN` | off | Prometheus metrics on a separate listener |

## Docs

- [API](docs/api.md): the public REST API ([`api/openapi.yaml`](api/openapi.yaml))
- [Configuration](docs/configuration.md): settings, logs, health, migrations
- [Architecture](docs/architecture.md): how the code is laid out
- [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [Code of conduct](CODE_OF_CONDUCT.md)

## Licence

[GPL-3.0-or-later](LICENSE).
