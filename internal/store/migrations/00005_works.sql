-- +goose Up
-- One work per actual work, created from a metadata entry and shared by
-- everyone. The display fields are a copy of the entry's, refreshed from
-- the metadata service.
CREATE TABLE works (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    meta_entry_id     text NOT NULL UNIQUE,
    title             text NOT NULL,
    sort_title        text NOT NULL,
    type              text NOT NULL DEFAULT '',
    status            text NOT NULL DEFAULT '',
    content_rating    text NOT NULL DEFAULT 'unknown'
        CHECK (content_rating IN ('safe', 'suggestive', 'erotic', 'pornographic', 'unknown')),
    year              integer,
    original_language text NOT NULL DEFAULT '',
    cover_url         text NOT NULL DEFAULT '',
    meta_synced_at    timestamptz NOT NULL DEFAULT now(),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX works_sort_title_idx ON works (sort_title);

-- What each user follows. Removing an entry never deletes files.
CREATE TABLE library_entries (
    user_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    work_id  uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, work_id)
);
CREATE INDEX library_entries_work_idx ON library_entries (work_id);

-- Series on sources that are (or may be) the work: attached ones feed its
-- chapters; candidates wait for someone to confirm; rejected ones are
-- remembered so matching doesn't attach them again.
CREATE TABLE source_series (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    work_id    uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    source_id  text NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    key        text NOT NULL,
    title      text NOT NULL,
    url        text NOT NULL DEFAULT '',
    cover_ref  text NOT NULL DEFAULT '',
    state      text NOT NULL CHECK (state IN ('attached', 'candidate', 'rejected')),
    origin     text NOT NULL CHECK (origin IN ('auto', 'manual', 'url')),
    score      real NOT NULL DEFAULT 0,
    evidence   jsonb NOT NULL DEFAULT '{}',
    priority   integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (work_id, source_id, key)
);
-- At most one attached series per work and source.
CREATE UNIQUE INDEX source_series_one_attached ON source_series (work_id, source_id) WHERE state = 'attached';

-- How matching went per work and source.
CREATE TABLE work_source_matches (
    work_id    uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    source_id  text NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    status     text NOT NULL CHECK (status IN ('pending', 'matched', 'candidates', 'no_match', 'failed')),
    message    text NOT NULL DEFAULT '',
    queries    text[] NOT NULL DEFAULT '{}',
    checked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (work_id, source_id)
);

-- +goose Down
DROP TABLE work_source_matches;
DROP TABLE source_series;
DROP TABLE library_entries;
DROP TABLE works;
