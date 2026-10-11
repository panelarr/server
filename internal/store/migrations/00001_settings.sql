-- +goose Up
-- Instance settings managed in the app: one JSON value per key. Startup
-- settings (database, listen address, directories) stay in the environment.
CREATE TABLE settings (
    key        text PRIMARY KEY CHECK (key ~ '^[a-z][a-z0-9_.]*$'),
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE settings;
