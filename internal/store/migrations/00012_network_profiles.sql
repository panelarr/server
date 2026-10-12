-- +goose Up
-- Network profiles built from solvers and proxies, and where admins assign
-- them. A profile is shared, or custom to one owner (a plugin, an
-- extension, a source or a metadata provider). Nothing is assigned by
-- default.
CREATE TABLE network_profiles (
    id                 text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{0,39}$'),
    name               text NOT NULL,
    proxy_id           text REFERENCES network_proxies (id),
    solver_id          text REFERENCES network_solvers (id),
    rate_requests      integer NOT NULL DEFAULT 0 CHECK (rate_requests >= 0),
    rate_period_ms     integer NOT NULL DEFAULT 0 CHECK (rate_period_ms >= 0),
    rate_burst         integer NOT NULL DEFAULT 0 CHECK (rate_burst >= 0),
    rate_scope         text NOT NULL DEFAULT 'source' CHECK (rate_scope IN ('source', 'host')),
    concurrency        integer NOT NULL DEFAULT 0 CHECK (concurrency >= 0),
    user_agent_mode    text NOT NULL DEFAULT 'default' CHECK (user_agent_mode IN ('default', 'fixed', 'random_desktop', 'from_solver')),
    user_agent         text NOT NULL DEFAULT '',
    doh_url            text NOT NULL DEFAULT '',
    headers            jsonb NOT NULL DEFAULT '{}',
    shared_cookie_jar  text NOT NULL DEFAULT '',
    connect_timeout_ms integer NOT NULL DEFAULT 0 CHECK (connect_timeout_ms >= 0),
    read_timeout_ms    integer NOT NULL DEFAULT 0 CHECK (read_timeout_ms >= 0),
    call_timeout_ms    integer NOT NULL DEFAULT 0 CHECK (call_timeout_ms >= 0),
    owner_kind         text NOT NULL DEFAULT '' CHECK (owner_kind IN ('', 'plugin', 'package', 'source', 'provider')),
    owner_id           text NOT NULL DEFAULT '',
    -- The plugin a custom profile belongs to: removing the plugin removes it.
    owner_plugin       text REFERENCES plugins (id) ON DELETE CASCADE,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK ((owner_kind = '') = (owner_id = ''))
);
CREATE INDEX network_profiles_owner_idx ON network_profiles (owner_kind, owner_id);

-- An assignment at one level: "global" and "meta" have an empty target_id;
-- "plugin" a plugin ID, "package" "plugin:package", "source" a source ID,
-- "provider" a metadata provider ID. No profile means "the plugin's own
-- settings", which stops inheritance.
CREATE TABLE network_assignments (
    target_kind text NOT NULL CHECK (target_kind IN ('global', 'plugin', 'package', 'source', 'meta', 'provider')),
    target_id   text NOT NULL DEFAULT '',
    profile_id  text REFERENCES network_profiles (id),
    plugin_id   text REFERENCES plugins (id) ON DELETE CASCADE,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_kind, target_id)
);
CREATE INDEX network_assignments_profile_idx ON network_assignments (profile_id);

-- +goose Down
DROP TABLE network_assignments;
DROP TABLE network_profiles;
