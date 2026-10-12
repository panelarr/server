-- +goose Up
-- Solvers and proxies can be custom to one owner too (a plugin, an
-- extension, a source or a metadata provider), like profiles: shown and
-- usable only there, and removed with their plugin.
ALTER TABLE network_solvers
    ADD COLUMN owner_kind   text NOT NULL DEFAULT '' CHECK (owner_kind IN ('', 'plugin', 'package', 'source', 'provider')),
    ADD COLUMN owner_id     text NOT NULL DEFAULT '',
    ADD COLUMN owner_plugin text REFERENCES plugins (id) ON DELETE CASCADE,
    ADD CHECK ((owner_kind = '') = (owner_id = ''));
ALTER TABLE network_proxies
    ADD COLUMN owner_kind   text NOT NULL DEFAULT '' CHECK (owner_kind IN ('', 'plugin', 'package', 'source', 'provider')),
    ADD COLUMN owner_id     text NOT NULL DEFAULT '',
    ADD COLUMN owner_plugin text REFERENCES plugins (id) ON DELETE CASCADE,
    ADD CHECK ((owner_kind = '') = (owner_id = ''));

-- +goose Down
ALTER TABLE network_proxies DROP COLUMN owner_plugin, DROP COLUMN owner_id, DROP COLUMN owner_kind;
ALTER TABLE network_solvers DROP COLUMN owner_plugin, DROP COLUMN owner_id, DROP COLUMN owner_kind;
