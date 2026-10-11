-- +goose Up
-- Source plugins the server talks to over the network (remote mode). The
-- ID is the host's name for this plugin instance and namespaces its
-- sources. Secrets (the API key, the plugin-level config) are stored
-- encrypted.
CREATE TABLE plugins (
    id             text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{0,39}$'),
    url            text NOT NULL,
    api_key        text NOT NULL DEFAULT '',
    config         text NOT NULL DEFAULT '',
    enabled        boolean NOT NULL DEFAULT true,
    info           jsonb NOT NULL DEFAULT '{}',
    health         text NOT NULL DEFAULT 'unknown' CHECK (health IN ('ok', 'degraded', 'down', 'unknown')),
    health_message text NOT NULL DEFAULT '',
    checked_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Sources as the plugins list them, under "plugin:source" IDs. A source a
-- plugin stops listing is kept (works may refer to it) but marked absent.
CREATE TABLE sources (
    id                text PRIMARY KEY,
    plugin_id         text NOT NULL REFERENCES plugins (id) ON DELETE CASCADE,
    source_id         text NOT NULL,
    name              text NOT NULL,
    lang              text NOT NULL DEFAULT '',
    content_rating    text NOT NULL DEFAULT 'unknown' CHECK (content_rating IN ('safe', 'mixed', 'nsfw', 'unknown')),
    icon_url          text NOT NULL DEFAULT '',
    home_url          text NOT NULL DEFAULT '',
    capabilities      text[] NOT NULL DEFAULT '{}',
    health            text NOT NULL DEFAULT 'unknown' CHECK (health IN ('ok', 'failing', 'unknown')),
    health_reason     text NOT NULL DEFAULT '',
    health_message    text NOT NULL DEFAULT '',
    health_checked_at timestamptz,
    enabled           boolean NOT NULL DEFAULT true,
    present           boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (plugin_id, source_id)
);

-- +goose Down
DROP TABLE sources;
DROP TABLE plugins;
