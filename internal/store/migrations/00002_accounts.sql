-- +goose Up
-- Local accounts. Usernames are unique regardless of case. Grants live on
-- the user; admins may always do everything, whatever their grants say.
CREATE TABLE users (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username               text NOT NULL CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9._-]{1,63}$'),
    display_name           text NOT NULL DEFAULT '' CHECK (length(display_name) <= 100),
    password_hash          text NOT NULL,
    role                   text NOT NULL CHECK (role IN ('admin', 'user')),
    disabled               boolean NOT NULL DEFAULT false,
    may_add                boolean NOT NULL DEFAULT true,
    may_download           boolean NOT NULL DEFAULT false,
    may_manage_matches     boolean NOT NULL DEFAULT false,
    content_rating_ceiling text NOT NULL DEFAULT 'suggestive'
        CHECK (content_rating_ceiling IN ('safe', 'suggestive', 'erotic', 'pornographic')),
    all_sources            boolean NOT NULL DEFAULT true,
    allowed_sources        text[] NOT NULL DEFAULT '{}',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    password_changed_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at          timestamptz
);
CREATE UNIQUE INDEX users_username_key ON users (lower(username));

-- Browser sessions. Only a hash of the token is stored.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash   bytea NOT NULL UNIQUE,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token   text NOT NULL,
    user_agent   text NOT NULL DEFAULT '',
    ip           text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- Personal API keys. Only a hash of the key is stored, plus its start to
-- tell keys apart.
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    prefix       text NOT NULL,
    key_hash     bytea NOT NULL UNIQUE,
    scopes       text[] NOT NULL CHECK (cardinality(scopes) > 0 AND scopes <@ ARRAY['read', 'write', 'admin', 'metrics:read']),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    expires_at   timestamptz
);
CREATE INDEX api_keys_user_id_idx ON api_keys (user_id);

-- Invite links. The new user gets the invite's role and grants.
CREATE TABLE invites (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash bytea NOT NULL UNIQUE,
    role       text NOT NULL CHECK (role IN ('admin', 'user')),
    grants     jsonb NOT NULL,
    note       text NOT NULL DEFAULT '' CHECK (length(note) <= 200),
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    used_by    uuid REFERENCES users (id) ON DELETE SET NULL
);

-- +goose Down
DROP TABLE invites;
DROP TABLE api_keys;
DROP TABLE sessions;
DROP TABLE users;
