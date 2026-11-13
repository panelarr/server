-- +goose Up
-- The IDs a source is known by in other apps' ecosystems, as its plugin
-- reports them ("<scheme>:<id>", e.g. "mihon:123"). Imports from those
-- apps look sources up by them, hence the GIN index (array containment).
-- Existing rows get an empty list and fill in on their plugin's next sync.
ALTER TABLE sources ADD COLUMN compat_ids text[] NOT NULL DEFAULT '{}';
CREATE INDEX sources_compat_ids_idx ON sources USING gin (compat_ids);

-- Series attached by an import. The constraint only widens: every
-- existing row satisfies the new one.
ALTER TABLE source_series DROP CONSTRAINT source_series_origin_check;
ALTER TABLE source_series ADD CONSTRAINT source_series_origin_check
    CHECK (origin IN ('auto', 'manual', 'url', 'import'));

-- +goose Down
-- Series an import attached count as added by hand once imports are gone.
UPDATE source_series SET origin = 'manual' WHERE origin = 'import';
ALTER TABLE source_series DROP CONSTRAINT source_series_origin_check;
ALTER TABLE source_series ADD CONSTRAINT source_series_origin_check
    CHECK (origin IN ('auto', 'manual', 'url'));
DROP INDEX sources_compat_ids_idx;
ALTER TABLE sources DROP COLUMN compat_ids;
