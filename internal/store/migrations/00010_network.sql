-- +goose Up
-- Shared network services (admins): challenge solvers and proxies, used by
-- network profiles. Proxy passwords are encrypted.
CREATE TABLE network_solvers (
    id                   text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{0,39}$'),
    name                 text NOT NULL,
    type                 text NOT NULL CHECK (type IN ('flaresolverr', 'byparr')),
    url                  text NOT NULL,
    session_ttl_seconds  integer NOT NULL DEFAULT 0 CHECK (session_ttl_seconds >= 0),
    max_timeout_seconds  integer NOT NULL DEFAULT 0 CHECK (max_timeout_seconds >= 0),
    as_response_fallback boolean NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE network_proxies (
    id              text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{0,39}$'),
    name            text NOT NULL,
    url             text NOT NULL,
    username        text NOT NULL DEFAULT '',
    password_sealed text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE network_proxies;
DROP TABLE network_solvers;
