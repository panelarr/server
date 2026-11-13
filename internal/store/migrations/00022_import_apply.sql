-- +goose Up
-- Applying an import: what happened to each series' chapters, and the
-- chapters still waiting for their work's chapters to be known (a newly
-- attached series has none until its first chapter check).
ALTER TABLE import_items
    -- What apply did with the series: '' (not yet), applied,
    -- already_imported (nothing left to change) or skipped (by the user).
    -- outcome stays what the preview found.
    ADD COLUMN result           text NOT NULL DEFAULT ''
        CHECK (result IN ('', 'applied', 'already_imported', 'skipped')),
    -- When apply (or resolving it) went through the series.
    ADD COLUMN applied_at       timestamptz,
    -- Chapters whose read mark, page or bookmark was placed.
    ADD COLUMN placed           integer NOT NULL DEFAULT 0,
    -- Chapters that couldn't be placed (no such chapter in Panelarr).
    ADD COLUMN unplaced         integer NOT NULL DEFAULT 0,
    -- The chapters still to place, like chapters; null when none.
    ADD COLUMN pending_chapters jsonb,
    -- Placing them is given up after this.
    ADD COLUMN pending_until    timestamptz;
-- Works with chapters waiting to be placed: checked whenever a work's
-- chapters are rebuilt.
CREATE INDEX import_items_pending_idx ON import_items (work_id) WHERE pending > 0;

-- +goose Down
DROP INDEX import_items_pending_idx;
ALTER TABLE import_items DROP COLUMN pending_until, DROP COLUMN pending_chapters, DROP COLUMN unplaced,
    DROP COLUMN placed, DROP COLUMN applied_at, DROP COLUMN result;
