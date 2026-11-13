-- +goose Up
-- Logins at an OpenID Connect provider linked to Panelarr users. A login
-- is the provider's issuer and its subject (`sub`), which the provider
-- promises never to reuse: users are never matched by name or email.
CREATE TABLE user_identities (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    issuer        text NOT NULL,
    subject       text NOT NULL,
    -- The email the provider reported at the last login, shown to people
    -- only; never used to find a user.
    email         text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    UNIQUE (issuer, subject)
);
CREATE INDEX user_identities_user_idx ON user_identities (user_id);

-- How a session was started.
ALTER TABLE sessions
    ADD COLUMN auth_method text NOT NULL DEFAULT 'password' CHECK (auth_method IN ('password', 'oidc'));

-- +goose Down
ALTER TABLE sessions DROP COLUMN auth_method;
DROP TABLE user_identities;
