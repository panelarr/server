-- +goose Up
-- How often a failed page image is tried again: on a package (all its
-- sources, target_id "plugin:package") or on one source (its ID). The
-- waits before each extra attempt, in milliseconds; empty means no
-- retries there, which is not the same as no row (inherit).
CREATE TABLE image_retry_settings (
    target_kind text NOT NULL CHECK (target_kind IN ('package', 'source')),
    target_id   text NOT NULL,
    delays_ms   integer[] NOT NULL CHECK (cardinality(delays_ms) <= 5),
    plugin_id   text NOT NULL REFERENCES plugins (id) ON DELETE CASCADE,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_kind, target_id)
);

-- +goose Down
DROP TABLE image_retry_settings;
