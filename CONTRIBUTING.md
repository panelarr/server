# Contributing

Thanks for helping. This page covers the development setup and how changes get in.

## Development setup

You need Go (the version in `go.mod`; the `go` command downloads it if yours is older), Docker (tests start PostgreSQL in a container) and `make`.

```sh
make setup   # installs pinned tools (sqlc, oapi-codegen, oasdiff, golangci-lint, govulncheck) into ./bin
make check   # format check, vet, lint, govulncheck and tests: run this before every PR
make help    # lists every target
```

### Sibling modules

Until the Panelarr metadata service and the source API are published as versioned Go modules, `go.mod` points at local clones next to this one (`replace github.com/panelarr/meta => ../meta`). Clone them side by side:

```sh
git clone https://github.com/panelarr/meta ../meta
git clone https://github.com/panelarr/source-api ../source-api
```

### Tests and PostgreSQL

Tests run against a real PostgreSQL, never a substitute. By default they start a `postgres` container with testcontainers. To use a server you already run, point `PANELARR_TEST_DATABASE_URL` at it. The role needs `CREATEDB` (and nothing more): each test creates and drops its own database.

```sh
export PANELARR_TEST_DATABASE_URL='postgres://panelarr:panelarr@localhost:5432/postgres?sslmode=disable'
make test
```

`make test-short` skips everything that needs PostgreSQL or Docker.

### The API is spec-first

`api/openapi.yaml` is the contract of the public client API. Change the spec first, then run `make gen` to regenerate the typed server in `internal/api/apigen`, then implement the new methods. Generated code is committed, and CI fails when it is stale.

The API is versioned: within `/api/v1`, changes must not break existing clients (adding optional fields or endpoints is fine; removing or renaming is not). CI runs `make api-breaking` on every PR to compare the spec with the base branch.

### The web app

The web app lives in its own repository. `make web-local` embeds a local build (`WEB_DIR`, default `../web/dist`); `make web WEB_VERSION=x.y.z` embeds a released one (its checksum is verified). The embedded files are not committed. During development, `PANELARR_WEB_DIR=../web/dist` serves a build without rebuilding the server.

### Database changes

Migrations are plain SQL files in `internal/store/migrations` (goose). Queries live in `internal/store/queries` and are compiled to Go by sqlc. After changing either, run `make gen`. Every migration has a working `Down`.

### No site data

Never commit downloaded chapters, images or records copied from real sites, in code, tests, fixtures or docs. Tests use made-up data and fake plugins.

## Working conventions

- **Tasks:** everything runs through `make`.
- **Branches:** `<type>/<scope>-<slug>`, e.g. `feat/auth-api-keys`.
- **Commits and PR titles:** [Conventional Commits](https://www.conventionalcommits.org/): `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `perf`, `build`, `ci`. Add `!` for breaking changes. PRs are squash-merged, so the PR title becomes the commit message.
- **Reviews:** at least one approval and green checks before merge.
- **AI agents:** agents may open PRs and post reviews. Anything an agent posts from a person's account starts with a **⚠️ AI agent** line. Humans stay responsible for what they merge.
