-- +goose Up
-- The package (e.g. an extension) that provides a source, for plugins that
-- install packages; empty for others. An absent source whose package is
-- installed but whose language is turned off is "language" unavailable
-- rather than gone.
ALTER TABLE sources
    ADD COLUMN package_id         text NOT NULL DEFAULT '',
    ADD COLUMN package_name       text NOT NULL DEFAULT '',
    ADD COLUMN unavailable_reason text NOT NULL DEFAULT '' CHECK (unavailable_reason IN ('', 'language'));

-- +goose Down
ALTER TABLE sources DROP COLUMN unavailable_reason, DROP COLUMN package_name, DROP COLUMN package_id;
