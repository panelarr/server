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

Single sign-on has its own settings: see [Single sign-on (OIDC)](#single-sign-on-oidc).

## The web app

The server serves the Panelarr web app at its root, built into the binary (`make web` embeds a pinned web release, `make web-local` a local build; without either, a placeholder page says so and the API works as usual). Any path that isn't the API (`/api/`), a feed (`/opds/`), an asset (`/assets/`), a probe or `/metrics` gets the app's page, which routes in the browser. Content-hashed assets are cached for a year; everything else is revalidated. The app is sent with a strict Content-Security-Policy (same-origin scripts only), `nosniff`, `no-referrer` and no framing; API responses carry `nosniff`, a `default-src 'none'` policy and no framing too.

## Logs

Every line carries `time`, `level`, `msg`, `service` (`server`) and, where it applies, `component` and `request_id`. Passwords, API keys, session tokens, cookies and query strings are never logged. Each request is logged once (`request`): server errors (5xx) at error level, except a plugin, site or service behind the server failing on an image route, which its handler reports. A request whose client went away before it finished (a closed tab) is logged as status 499 at info level, not as an error.

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

The metadata service's own admin is available inside Panelarr's, through the server (the browser never talks to meta), apart from meta's API clients and keys:

- **Jobs** (`/api/v1/system/meta/jobs`): meta's background jobs in the same shape as the server's own (provider syncs, duplicate finding, the search index rebuild, …), with their runs; run now and cancel. Their schedules are set in meta's configuration and shown read-only; cron schedules say their time zone (`time_zone`, UTC for meta unless an expression names one). A job that can't run now (e.g. a feature it needs is off) says why (`disabled_reason`); starting it answers `409 job_disabled`. Runs send no events: poll a running one.
- **Providers** (`/api/v1/system/meta/providers`): each provider with its plugin, health, sync schedules (from meta's configuration, read-only), record count and last sync runs; sync runs in full with their counts and notices (`/sync-runs`); meta's own health per component (`/health`); and fresh data for one entry from its providers (`POST /entries/{id}/refresh`).
- **Duplicates review** (`/api/v1/system/meta/duplicates`): pairs of entries meta suspects are one work, with the signals and the decision model's verdict; confirm (merges the two for good), dismiss (different works) or reopen a dismissal. The decision model's settings (`/duplicate-judge`) can be read, saved, tested and reset; its API key is never shown.
- **Search engine** (`/api/v1/system/meta/search-engine`): the backend meta searches with (its database, or a search engine by URL), with the state of its index (ready, documents, pending changes, errors); settings can be saved (winning over meta's configuration), tested and reset, and the engine's API key is never shown. The index is rebuilt with the `search-rebuild` job.

## Source plugins

Sources come from plugins that run as their own services. An admin adds a plugin by its URL (and API key, stored encrypted); the server checks it speaks the source plugin contract, lists its sources and refreshes them with their health every five minutes. A plugin's settings are edited in the app, kept encrypted, and handed back to the plugin whenever it restarts. A plugin that is down marks the server `degraded`; its sources stay known. A plugin found down is rechecked after 10 seconds, 30 seconds and a minute (then the regular sync takes over), so one that was only restarting shows as ok again within seconds. Plugins apply saved settings in the background (adding stores, installing extensions) and report problems in their health, so the server also checks a plugin 5 and 30 seconds after its settings are saved; each check is sent to admins as a `plugin.health` event. At startup, a plugin that doesn't answer yet (one started together with the server may not even resolve by name for the first seconds) is tried again with growing waits for up to 30 seconds after the server started before it is marked down; one still not answering then is marked down and rechecked as above. Waiting for one plugin doesn't make the others unavailable: every plugin is connected before any is waited for, so one that answers can be used right away (the first syncs still run one after another).

Plugins that host many sites (the `packages` capability, e.g. an extension runtime) get their sources from **packages**. An admin lists what the plugin offers (`GET /api/v1/plugins/{id}/packages`: names, languages, versions, updates, content ratings, where each comes from) and installs, updates or uninstalls packages; each change is a `plugin.package` task (one at a time; the plugin checks signatures and checksums, and a package that fails them or a broken store fails the task at once, while a store that couldn't be reached is tried again), and the plugin is synced as soon as it's done, so the sources appear or go absent within seconds. Its **origins** (the stores in its settings) show their state (last refresh and its error, how many packages, the pinned signing key) and can be refreshed now; installed packages show where they came from, their signing fingerprint, checksum and API version, and can be reloaded (retrying a failed load). Plugins that run scheduled source health checks (the `health_checks` capability) let an admin turn them off per source. Plugins with background jobs of their own (the `jobs` capability, e.g. store refreshes or checking sources now) show them under `/api/v1/plugins/{id}/jobs` in the same shape as the server's and meta's jobs (run IDs are the plugin's own strings); they can be run now and cancelled. Languages decide which sources exist: the plugin's own `languages` setting, then each package's languages (`PUT …/packages/{package}/languages`; empty means every language the plugin allows). Package and source icons come through the server (`icon_url`, signed links): it fetches them from the plugin's address (a plugin may give a path relative to it) or the URL given, sending the plugin's key only to the plugin's own address, keeps raster images up to 2 MiB and refreshes them weekly, so the browser never contacts a site. A failed fetch isn't kept: the next request tries again. A source without an icon, or whose plugin hides it (it answers 404, e.g. for a source in a language turned off), shows the icon of the package that provides it; with neither, the icon answers 404 (logged at debug level). Icons that fail for real answer 502, with a warning at most every ten minutes per source or package. Covers and pages go through the source (the plugin's image call) instead. Each source shows the package that provides it. A source whose language is turned off is kept as absent with `unavailable_reason: language` ("not available"); one whose package is uninstalled is kept as absent without a reason (no longer provided). Either way it isn't used: works matched to it keep their history and read from other sources meanwhile (see [matching](matching.md)).

Each source's ID is `<plugin>:<source>`. Admins turn sources on or off for everyone; each user's grants (allowed sources, content-rating ceiling) decide which they see. Mixed-content sources count as "suggestive", adult-only sources as "pornographic". Images from sources (covers, icons) are fetched through the plugin and cached by the server; their links are signed, so the server never fetches an address a caller made up.

## Network services

Admins set up shared network services once, in Panelarr, under `/api/v1/network`, instead of in each plugin's own admin:

- **Solvers:** FlareSolverr-compatible challenge solvers (FlareSolverr, Byparr) by URL, with how long a session is reused and how long one solve may take. A check asks the solver for its state.
- **Proxies:** HTTP, HTTPS or SOCKS5 proxies, e.g. a VPN container that exposes a proxy (Panelarr never runs tunnels itself). The password is stored encrypted and never shown again. A check connects to the proxy, or requests a URL you give through it.

- **Network profiles** bundle a proxy, a solver, a rate limit, a concurrency cap, the user agent (the plugin's, a fixed one, a random desktop one, or the solver's), a DNS-over-HTTPS resolver, extra headers, a shared cookie jar and timeouts; parts left out are the plugin's defaults. A profile is shared, or **custom** to one plugin, extension, source or metadata provider: then it can only be assigned there, and it goes when its plugin is removed.
- **Assignments** put a profile at a level: everything (global), a plugin, an extension (package), a source, all of meta, or one metadata provider. The most specific wins: for a source, its own assignment, then its extension's, its plugin's, then global; for a provider, its own, then meta's, then global. An assignment can also say "the plugin's own settings", e.g. for one source that should connect directly while its plugin uses a VPN profile. `GET /api/v1/plugins/{id}/network` shows the profile in force for each source and where it comes from.

They are options: nothing is assigned by default, so plugins keep their own settings until an admin assigns a profile. A solver, proxy or profile in use can't be removed. Like profiles, a solver or proxy can be **custom** to one plugin, extension, source or metadata provider, as if set up in that service's own admin: only profiles of the same plugin (or the same provider) can use it, and it goes when its plugin is removed. Panelarr keeps them all and pushes the resulting profiles as usual.

The server **pushes** each source's profile to its plugin, with the proxy's password and the solver inline, and only to plugins that declare they take network profiles (the solver only to those that take solvers too). A plugin whose source falls back to "the plugin's own settings" is told so. Changes are pushed within seconds (a `network.push` task); each plugin sync pushes what changed or failed, and everything is pushed again after a plugin restarts (its `instance_id` changes, it comes back from down, or the server starts). The plugin page shows each source's push state, and `GET /api/v1/sources/{id}/network` asks the plugin what it actually applies. The server's own per-source limits follow the profile's rate limit and concurrency where it sets them, else the plugin's declared limits.

**Metadata providers** get their profile through the metadata service: the server pushes each provider's profile in force (the solver inline) and meta keeps it, hands it only to providers whose plugin takes profiles, and gives it again whenever a plugin reconnects. The server pushes changes within seconds, again when meta is (re)connected, and checks every 5 minutes (the `network.meta` job) that meta still has what it should. `GET /api/v1/system/meta/network` shows each provider's profile in force, its push state and what meta applies.

A source's **own settings** (e.g. an extension's preferences), for sources with the `settings` capability, are read and changed through `GET`/`PATCH /api/v1/sources/{id}/settings` (admins), in the schema the plugin gives. The plugin keeps them; secret fields are never shown and keep their value when left empty.

## Background work

Two kinds of background work show in the admin:

- **Jobs** are named and run on a schedule or by hand, one run at a time: `plugins.sync` (every 5 minutes), `sessions.cleanup` (hourly), `tasks.prune` (every 6 hours; finished tasks are kept a week, failed ones until retried). Each job keeps its last 100 runs with their outcome, counts and notes. A run left running when the server stopped is marked failed ("interrupted").
- **Tasks** are single pieces of work queued by features (matching a work, checking it for updates, downloading a chapter). They retry with growing waits (30 seconds up to an hour), respect a source's "retry after" hint, and fail after their kind's attempts. The same work is never queued twice. At most 8 tasks run at once, each kind has its own cap, and each source caps how many of its tasks run at once and how fast they call it, across all users (from the plugin's declared limits, else 2 at once and 2 requests a second). Admins pause and resume kinds, cancel and retry tasks. Tasks interrupted by a restart go back to the queue.

Live changes reach clients over `GET /api/v1/events` (server-sent events), filtered per user.

## Accounts

The first account created (through the first-run wizard) is the admin. Admins add users with a password or send invite links, and set each user's role and grants. Passwords are hashed with argon2id. Failed logins are limited per username (5 per 15 minutes) and per client address (30 per 15 minutes); failed single sign-ons have their own limit per address and don't count here. Users with a linked identity can also sign in with [single sign-on](#single-sign-on-oidc) when it is configured.

## Single sign-on (OIDC)

Users can sign in with an OpenID Connect identity provider (Authelia, Authentik, Keycloak, Dex and the like) next to their password. It is **off unless `PANELARR_OIDC_ISSUER` is set**: without it there is no "Sign in with SSO" button, `/api/v1/auth/oidc/*` answers `404`, and setup and password login work as always. Password login keeps working while single sign-on is on and while the provider is down, so an admin can always get in.

| Environment | Default | Meaning |
|---|---|---|
| `PANELARR_OIDC_ISSUER` | (empty: off) | The provider's issuer URL, e.g. `https://id.example.com/realms/home`. Use it exactly as the provider publishes it, including a trailing `/` if it has one (Authentik's per-application issuers do): the discovery document must name the same issuer, character for character. Discovery reads `<issuer>/.well-known/openid-configuration`. Must be `https`, except for `localhost` and loopback addresses. |
| `PANELARR_OIDC_CLIENT_ID` | (required with the issuer) | Panelarr's client ID at the provider. |
| `PANELARR_OIDC_CLIENT_SECRET` | (empty) | The client's secret. Empty means a public client (PKCE only). Never logged or shown by the API. |
| `PANELARR_OIDC_CLIENT_SECRET_FILE` | (empty) | Read the secret from this file instead (a mounted secret). Set one of the two, not both. |
| `PANELARR_OIDC_SCOPES` | `openid profile email` | Scopes to ask for, separated by spaces or commas; `openid` is always added. Add `groups` if your provider needs it for the groups claim. |
| `PANELARR_OIDC_BUTTON_LABEL` | `Sign in with SSO` | The login page's button. |
| `PANELARR_OIDC_GROUPS_CLAIM` | `groups` | The claim that lists the user's groups (read from the ID token, or from userinfo when the token has none). |
| `PANELARR_OIDC_ALLOWED_GROUPS` | (empty: anyone) | Comma-separated groups: only their members may sign in with single sign-on, be created by it or link a login. |
| `PANELARR_OIDC_USERNAME_CLAIM` | `preferred_username` | The claim a new user's username comes from. When it is missing or has no usable characters: the part of `email` before `@`, then the subject. |
| `PANELARR_OIDC_AUTO_PROVISION` | `false` | `true` creates a user on the first sign-in of a login nobody is linked to. Set allowed groups with it, or anyone with an account at the provider gets one here. |
| `PANELARR_OIDC_ADMIN_GROUPS` | (empty: no role sync) | Comma-separated groups whose members are admins. When set, the role of a user with a linked login follows these groups on every single sign-on. |
| `PANELARR_OIDC_LOCAL_LOGIN` | `all` | Who may sign in with a password: `all`, or `admins` (everyone else signs in with single sign-on). Password login can't be switched off: it is the way in when the provider is down. |
| `PANELARR_OIDC_LOGOUT` | `local` | `local` ends Panelarr's session only. `provider` also sends the browser to the provider's end-session endpoint (RP-initiated logout) after signing out of a single sign-on session, and back to `<PANELARR_BASE_URL>/login`: register that as a post-logout redirect URI at the provider. |

`PANELARR_BASE_URL` is required with single sign-on: register `<PANELARR_BASE_URL>/api/v1/auth/oidc/callback` as the client's redirect URI at the provider. The server refuses to start with an issuer but no base URL or client ID, or with other `PANELARR_OIDC_*` settings but no issuer.

**Who signs in.** A provider login is linked to a Panelarr user by the provider's issuer and subject (`sub`) only, never by username or email, so a matching name or address at the provider can't take over an account; existing users are never matched automatically. After the allowed groups, the callback decides in this order:

1. A linked login signs its user in (a disabled user is refused: `account_disabled`).
2. A link flow (from the account page: `POST /api/v1/auth/oidc/link`, which answers the provider's URL in `redirect_to`) links the login to the signed-in user and starts no new session. It can only be started by a browser signed in with a session (not an API key), with the session's CSRF token: in the `X-CSRF-Token` header of the `POST`, or, for older clients, in `csrf_token` of a navigation to `GET /api/v1/auth/oidc/start?intent=link` (`403 csrf_failed` without it; a navigation another site starts is also refused by its `Sec-Fetch-Site` header). It links only if that same session is still signed in when the provider sends the browser back (`session_changed` otherwise). A login linked to someone else is refused (`already_linked`), never signed in as that user, and a user has at most one login per provider (`link_exists`). Errors of a link flow go back to its `return_to` with `sso_error`.
3. An invite accepted with single sign-on (`GET /api/v1/auth/oidc/start?invite=<token>`) creates its user with the invite's role and grants, links the login and uses the invite up (`invite_invalid` when it was used or expired meanwhile). The invite token travels only inside the encrypted flow cookie, never to the provider.
4. With `PANELARR_OIDC_AUTO_PROVISION=true`, a new user is created with the default grants and the role from the admin groups. Its username is the username claim cut to the characters usernames allow (letters, digits, `.`, `_`, `-`, at most 58 of them), with `-2`, `-3`… added while it is taken; its display name is the `name` claim.
5. Otherwise the login is refused (`not_linked`: "No Panelarr account is linked to this login; ask an admin for an invite").

Users created by single sign-on have no password: password login fails for them like a wrong password, until they set one on their account page (`POST /api/v1/me/password` with an empty current password; from then on the current one is needed).

**Roles from groups.** With `PANELARR_OIDC_ADMIN_GROUPS` set, every single sign-on sets the user's role: admin when they are in one of the groups, user otherwise. Only users signing in with a linked login are changed, never anyone else. The last enabled admin is never demoted (the role stays and a warning is logged). When the provider sends no groups claim at all (neither in the ID token nor from userinfo), the role is left alone and a warning is logged: a missing claim usually means the `groups` scope or claim isn't configured, and must not demote every admin. An empty list does count as "in no group". Role changes take effect at once, for the user's sessions and API keys.

The admin groups are then the single source of truth for who is an admin among single sign-on users: a role an admin sets by hand, or the role of an invite, lasts only until the user's next single sign-on. An admin invite accepted by someone outside the admin groups creates a user, not an admin, and an admin outside the groups becomes a user at their next single sign-on. The one exception stays: the last enabled admin is never demoted. `GET /api/v1/system/auth` lists the configured groups in `oidc.admin_groups` (empty when roles aren't synced).

**Linked logins.** `GET /api/v1/me/identities` lists the caller's linked logins (provider, subject, email, when linked and last used) and `DELETE /api/v1/me/identities/{id}` unlinks one. Admins use `GET` and `DELETE /api/v1/users/{id}/identities…`; an admin's unlink also ends the user's single sign-on sessions. Unlinking is refused (`409 last_login_method`) when the user would be left without a way to sign in: no other login and no password they may use (with `PANELARR_OIDC_LOCAL_LOGIN=admins`, a non-admin's password doesn't count). The users endpoints mark linked users with `sso: true` and users with a password with `has_password: true`.

**Password login and break-glass.** First-run setup always creates a local admin with a password; single sign-on can't run setup. With `PANELARR_OIDC_LOCAL_LOGIN=admins`, `GET /api/v1/auth/methods` says `password: admins` and a non-admin's password login answers `401 invalid_credentials`, exactly like a wrong password or an unknown user, whether the password was right or not: the answer never confirms a password that can't be used. The login page explains from `password: admins` that password sign-in is for admins only. Admins can always sign in with a password, also while the provider is down; removing the `PANELARR_OIDC_*` settings turns everything back to plain password login. Keep at least one admin with a password.

**Switching to `PANELARR_OIDC_LOCAL_LOGIN=admins`.** From the switch on, a non-admin can only sign in with a linked login. A user who hasn't linked one is locked out, and with `PANELARR_OIDC_AUTO_PROVISION=true` their first single sign-on doesn't find their account (logins are never matched by name or email) and creates a second one instead, named like the first with `-2` added, without their library, progress or grants. Switch in this order:

1. Configure single sign-on with `PANELARR_OIDC_LOCAL_LOGIN=all` (the default), preferably with auto-provisioning still off.
2. Have every user link their login on their account page. Admins see who has with `sso: true` in `GET /api/v1/users`.
3. Then set `PANELARR_OIDC_LOCAL_LOGIN=admins`, and turn on auto-provisioning if you want it for new people.

**Logout.** `POST /api/v1/auth/logout` always ends Panelarr's session. With `PANELARR_OIDC_LOGOUT=provider` and a session started by single sign-on, it answers `200` with `redirect_to`, the provider's `end_session_endpoint` with `client_id` and `post_logout_redirect_uri`; the web app sends the browser there. A provider without an end-session endpoint, or one that can't be reached, means Panelarr's session only (`204`).

**Status.** `GET /api/v1/system/auth` (admins) shows whether single sign-on is on, the issuer, the client ID, whether the client is public, whether the provider can be reached right now, and the scope, claim, provisioning, group, password and logout settings. The client secret is never returned.

**The flow.** `GET /api/v1/auth/oidc/start?return_to=/path` sends the browser to the provider with the authorization code flow, PKCE (S256), a `state` and a `nonce`. These secrets and `return_to` travel in a cookie (`panelarr_oidc`, HTTP-only, sent only to `/api/v1/auth/oidc/`, valid 10 minutes) that the server encrypts and authenticates with its secret key (`PANELARR_SECRET_KEY` or the key file), so no table is needed. The callback checks the cookie and `state`, exchanges the code with the PKCE verifier (and the secret), and verifies the ID token: its signature with the provider's keys, its issuer, its audience (and `azp`: a token for several audiences must name Panelarr's client as its authorized party, and an `azp`, when present, must be Panelarr's client), its expiry (allowing 2 minutes of clock skew) and its nonce. A flow can be finished only once. On success the session is the same as after a password login (the session cookie and the CSRF cookie) and is marked as started by single sign-on; the browser goes back to `return_to`, which must be a path on Panelarr itself (anything else answers `400 invalid_return_to`); it is cleaned of `.`/`..` segments and doubled slashes. On failure no session is started and the browser goes to `/login?sso_error=<code>`; the codes are listed with the endpoint in the [API](../api/openapi.yaml). Failed sign-ins are limited per client address (30 per 15 minutes), apart from password logins, so single sign-on can never lock anyone out of password login. Only callbacks that belong to the browser's own flow (its cookie opened and `state` matched) count: a callback without one is refused (`flow_expired`, `state_mismatch`) but not counted, because any website can make a browser load the callback. For the same reason a callback whose `state` doesn't match leaves the flow cookie alone, so a forged callback can't end a sign-in in progress; a matching one uses the flow up.

**When the provider is down.** It doesn't stop the server: discovery is retried in the background (from 5 seconds up to every 5 minutes) and kept once it works; the signing keys are refreshed when the provider rotates them. Meanwhile the health shows the `oidc` component failing (the server is `degraded`, still ready), and starting a sign-in answers `503 oidc_unavailable`.

**Audit.** Sign-ins with single sign-on are logged like password logins (`component: audit`, `msg: login`, `method: oidc`, with the user and the identity), failed ones as `sso login failed` with a reason code, and callbacks refused before they were a sign-in attempt as `sso callback refused`. So are `user provisioned`, `invite accepted` (`method: oidc`), `identity linked`, `identity unlinked` and `role synced from groups` (with the old and new role), and role syncs skipped for the last admin or a missing groups claim. Tokens, codes and the secret are never logged.

Sessions last until they expire or the user signs out: removing someone at the provider doesn't end their Panelarr sessions or API keys, so disable the user in Panelarr too.

## Database migrations

The server applies pending migrations on start. `panelarr migrate [up|down|status]` does the same by hand; `down` rolls back the latest migration.
