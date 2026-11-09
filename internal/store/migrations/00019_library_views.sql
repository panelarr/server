-- +goose Up
-- A user's saved views of their library: a name, and filters, a sort and
-- display settings (the definition, validated by the server). Each user
-- has one default view and orders their views.
CREATE TABLE library_views (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    position    integer NOT NULL,
    is_default  boolean NOT NULL DEFAULT false,
    definition  jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX library_views_name_key ON library_views (user_id, lower(name));
CREATE UNIQUE INDEX library_views_default_key ON library_views (user_id) WHERE is_default;
CREATE INDEX library_views_user_idx ON library_views (user_id, position);

-- +goose Down
DROP TABLE library_views;
