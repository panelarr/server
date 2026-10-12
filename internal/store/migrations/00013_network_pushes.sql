-- +goose Up
-- What the server last pushed to a source's plugin (or a metadata
-- provider): a hash of the profile, so unchanged profiles aren't pushed
-- again, and the error when the push failed.
CREATE TABLE network_pushes (
    target_kind text NOT NULL CHECK (target_kind IN ('source', 'provider')),
    target_id   text NOT NULL,
    plugin_id   text REFERENCES plugins (id) ON DELETE CASCADE,
    hash        text NOT NULL DEFAULT '',
    error       text NOT NULL DEFAULT '',
    pushed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_kind, target_id)
);

-- +goose Down
DROP TABLE network_pushes;
