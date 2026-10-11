-- +goose Up
-- Where downloaded chapters are stored. The S3 secret is encrypted.
CREATE TABLE storages (
    id         text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{0,39}$'),
    name       text NOT NULL,
    kind       text NOT NULL CHECK (kind IN ('local', 's3')),
    config     jsonb NOT NULL DEFAULT '{}',
    secret     text NOT NULL DEFAULT '',
    is_default boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX storages_one_default ON storages (is_default) WHERE is_default;

-- One stored file per chapter, shared by everyone.
CREATE TABLE chapter_files (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    chapter_id      uuid NOT NULL UNIQUE REFERENCES chapters (id) ON DELETE CASCADE,
    storage_id      text NOT NULL REFERENCES storages (id),
    key             text NOT NULL,
    pages           integer NOT NULL,
    bytes           bigint NOT NULL,
    sha256          text NOT NULL,
    from_variant_id uuid REFERENCES source_chapters (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (storage_id, key)
);

-- "Download new chapters" per library entry.
ALTER TABLE library_entries ADD COLUMN auto_download boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE library_entries DROP COLUMN auto_download;
DROP TABLE chapter_files;
DROP TABLE storages;
