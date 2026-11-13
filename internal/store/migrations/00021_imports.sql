-- +goose Up
-- A user's import of another app's library (a Suwayomi or Mihon backup):
-- the parsed series with what the preview found for each, so the user can
-- review them, apply them and resolve the ones left open without
-- uploading the file again. The file itself is never stored.
CREATE TABLE imports (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    format       text NOT NULL CHECK (format IN ('mihon_backup')),
    file_sha256  text NOT NULL,
    state        text NOT NULL CHECK (state IN ('previewing', 'preview', 'applying', 'done', 'failed', 'cancelled')),
    task_id      uuid REFERENCES tasks (id) ON DELETE SET NULL,
    -- Totals, categories, tracking counts and package hints.
    summary      jsonb NOT NULL DEFAULT '{}',
    message      text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);
CREATE INDEX imports_user_idx ON imports (user_id, created_at DESC);
CREATE INDEX imports_file_idx ON imports (user_id, file_sha256);
-- One running import per user.
CREATE UNIQUE INDEX imports_running_key ON imports (user_id) WHERE state IN ('previewing', 'applying');

-- One series of an import. outcome is 'pending' until the preview matched
-- it, then ready, in_library, needs_choice, not_found, source_missing or
-- not_allowed.
CREATE TABLE import_items (
    import_id        uuid NOT NULL REFERENCES imports (id) ON DELETE CASCADE,
    idx              integer NOT NULL,
    title            text NOT NULL,
    author           text NOT NULL DEFAULT '',
    artist           text NOT NULL DEFAULT '',
    -- The source as the backup names it ("mihon:<id>") and its name there.
    source_compat_id text NOT NULL,
    source_name      text NOT NULL DEFAULT '',
    series_key       text NOT NULL,
    added_at         timestamptz,
    categories       text[] NOT NULL DEFAULT '{}',
    outcome          text NOT NULL DEFAULT 'pending',
    -- What the preview found: the work, else the catalogue entry to add,
    -- and the Panelarr source the series would be attached on.
    work_id          uuid REFERENCES works (id) ON DELETE SET NULL,
    entry_id         text NOT NULL DEFAULT '',
    source_id        text NOT NULL DEFAULT '',
    -- How the backup's source stands for the user: usable, not_allowed or
    -- missing ('' until matched).
    source_state     text NOT NULL DEFAULT '',
    candidates       jsonb NOT NULL DEFAULT '[]',
    -- Chapters with something to import: [{key, number, read, page, read_at, bookmark}].
    chapters         jsonb NOT NULL,
    chapters_read    integer NOT NULL DEFAULT 0,
    bookmarks        integer NOT NULL DEFAULT 0,
    pending          integer NOT NULL DEFAULT 0,
    message          text NOT NULL DEFAULT '',
    PRIMARY KEY (import_id, idx)
);
CREATE INDEX import_items_outcome_idx ON import_items (import_id, outcome, idx);

-- +goose Down
DROP TABLE import_items;
DROP TABLE imports;
