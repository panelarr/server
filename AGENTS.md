# Agent instructions

## Before you work

1. Your task brief was given to you by your operator, including where to read its full context. Read the brief and the documents it lists before changing code.
2. Read this repo's own docs: `README.md`, `docs/`, `CONTRIBUTING.md`.
3. Treat the decisions referenced in your brief as requirements. If the code can't satisfy one, stop and report the conflict to your operator. Don't diverge silently.

## Rules for public artifacts

Everything in this repo is public: code, comments, docs, commit messages, branch names, PR and issue text.

- Explain the *why* in your own words. Design reasoning worth keeping publicly goes in `docs/`.
- Design IDs from the project's knowledge base may appear as `Refs:` commit footers, never as the only explanation.

## Confidential terms

Your operator gives you a list of aliases for confidential names (a local file, never committed). Write **only the alias** in code, comments, docs, commits, branches and PR text. Never name real extensions, extension stores or sites. If unsure whether a name is confidential, ask.

## Working here

- Everything runs through `make` (`make help`). Before reporting, run `make check`.
- Branch per slice: `<type>/<scope>-<slug>`. Conventional Commits.
- Commit / merge / push rights come from your brief. If unsure: commit on your branch only, never push.
- After each slice, report to your operator: what you built, what you assumed, how to test it.
- Text you post on the forge from a human's account starts with: `> ⚠️ **AI agent:** written by an AI agent, posted from <owner>'s account.`

## This repo

- **Progress:** [PROGRESS.md](PROGRESS.md) lists the planned slices, what's done and what's next. Re-read it whenever you lose context.
- **Spec-first API:** `api/openapi.yaml` is the public client API's contract. Change it first, `make gen`, then implement. No breaking changes within `/api/v1`.
- **Data layer:** PostgreSQL only, with sqlc + pgx v5 + goose. Migrations are plain SQL in `internal/store/migrations`, embedded and applied at startup and by `panelarr migrate`. No superuser, no server-wide settings.
- **Authorization everywhere:** every handler, including page, file and event streams, checks what the caller may see. Add a case to the authorization tests for every new endpoint.
- **Licence:** GPL-3.0-or-later (`// SPDX-License-Identifier: GPL-3.0-or-later` in every Go file).
- **Secrets:** never log passwords, API keys, session tokens, cookies or DSN passwords, and never return a stored secret from the API.
