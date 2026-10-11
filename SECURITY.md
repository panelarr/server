# Security policy

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub: open this repository's **Security** tab and choose **Report a vulnerability** (GitHub private vulnerability reporting). Don't open a public issue.

Include what you found, how to reproduce it and what impact you expect. We'll acknowledge the report, keep you updated and credit you in the advisory unless you'd rather stay anonymous.

## Supported versions

The server is in early development. Only the latest commit on `main` gets fixes.

## Threat model in short

- **Internet-facing by default.** Many installs sit behind a reverse proxy on the public internet, so the server treats every request as untrusted.
- **Many users, shared content.** Every API call, including page and file streaming, checks what the caller may see. One user can never read another user's library, progress or keys.
- **Plugins are separate programs.** Source plugins run out of process and are reached over the network. Only add plugins you trust.
- **Secrets** (plugin and service keys, storage credentials) are encrypted at rest, never logged and never returned by the API once set.
- **Database:** the server needs only its own database. It never needs a PostgreSQL superuser and changes no server-wide settings.
