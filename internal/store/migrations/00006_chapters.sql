-- +goose Up
-- When each attached series was last checked for chapters.
ALTER TABLE source_series
    ADD COLUMN checked_at timestamptz,
    ADD COLUMN check_error text NOT NULL DEFAULT '';

-- Canonical chapters of a work: one per chapter number across all its
-- sources. Chapters without a number stay per source ("extras", keyed by
-- the source chapter). A chapter is kept when its sources drop it (users'
-- progress and files may point at it); it is then unavailable.
CREATE TABLE chapters (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    work_id              uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    number               double precision,
    extra_key            text NOT NULL DEFAULT '',
    volume               text NOT NULL DEFAULT '',
    title                text NOT NULL DEFAULT '',
    preferred_variant_id uuid,
    pinned_variant_id    uuid,
    reason               text NOT NULL DEFAULT '',
    available            boolean NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CHECK ((number IS NULL) <> (extra_key = ''))
);
CREATE UNIQUE INDEX chapters_number_key ON chapters (work_id, number) WHERE number IS NOT NULL;
CREATE UNIQUE INDEX chapters_extra_key ON chapters (work_id, extra_key) WHERE number IS NULL;

-- Chapters exactly as each attached series lists them: the variants.
CREATE TABLE source_chapters (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_series_id uuid NOT NULL REFERENCES source_series (id) ON DELETE CASCADE,
    key              text NOT NULL,
    name             text NOT NULL,
    number           double precision,
    volume           text NOT NULL DEFAULT '',
    scanlator        text NOT NULL DEFAULT '',
    lang             text NOT NULL DEFAULT '',
    url              text NOT NULL DEFAULT '',
    uploaded_at      timestamptz,
    position         integer NOT NULL DEFAULT 0,
    present          boolean NOT NULL DEFAULT true,
    chapter_id       uuid REFERENCES chapters (id) ON DELETE SET NULL,
    first_seen_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_series_id, key)
);
CREATE INDEX source_chapters_chapter_idx ON source_chapters (chapter_id);

ALTER TABLE chapters
    ADD CONSTRAINT chapters_preferred_fk FOREIGN KEY (preferred_variant_id) REFERENCES source_chapters (id) ON DELETE SET NULL,
    ADD CONSTRAINT chapters_pinned_fk FOREIGN KEY (pinned_variant_id) REFERENCES source_chapters (id) ON DELETE SET NULL;

-- "New" chapters for a user are those found after they last looked.
ALTER TABLE library_entries ADD COLUMN seen_at timestamptz NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE library_entries DROP COLUMN seen_at;
ALTER TABLE chapters DROP CONSTRAINT chapters_pinned_fk, DROP CONSTRAINT chapters_preferred_fk;
DROP TABLE source_chapters;
DROP TABLE chapters;
ALTER TABLE source_series DROP COLUMN check_error, DROP COLUMN checked_at;
