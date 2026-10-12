-- +goose Up
-- Whether a source's scheduled health checks are turned off, as its plugin
-- reports it (plugins with the health checks capability).
ALTER TABLE sources ADD COLUMN health_checks_disabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sources DROP COLUMN health_checks_disabled;
