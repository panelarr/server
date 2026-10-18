-- name: CreatePlugin :one
INSERT INTO plugins (id, url, api_key, info, health, health_message, checked_at)
VALUES (@id, @url, @api_key, @info, @health, @health_message, now())
RETURNING *;

-- name: GetPlugin :one
SELECT * FROM plugins WHERE id = $1;

-- name: ListPlugins :many
SELECT * FROM plugins ORDER BY id;

-- name: UpdatePlugin :one
UPDATE plugins SET url = @url, api_key = @api_key, enabled = @enabled, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetPluginState :one
UPDATE plugins SET info = @info, health = @health, health_message = @health_message, checked_at = now()
WHERE id = @id
RETURNING *;

-- name: SetPluginConfig :exec
UPDATE plugins SET config = @config, updated_at = now() WHERE id = @id;

-- name: DeletePlugin :execrows
DELETE FROM plugins WHERE id = $1;

-- name: UpsertSource :exec
INSERT INTO sources (
    id, plugin_id, source_id, name, lang, content_rating, icon_url, home_url, capabilities,
    health, health_reason, health_message, health_checked_at, package_id, package_name, health_checks_disabled, present
) VALUES (
    @id, @plugin_id, @source_id, @name, @lang, @content_rating, @icon_url, @home_url, @capabilities,
    @health, @health_reason, @health_message, @health_checked_at, @package_id, @package_name, @health_checks_disabled, true
)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, lang = EXCLUDED.lang, content_rating = EXCLUDED.content_rating,
    icon_url = EXCLUDED.icon_url, home_url = EXCLUDED.home_url, capabilities = EXCLUDED.capabilities,
    health = EXCLUDED.health, health_reason = EXCLUDED.health_reason, health_message = EXCLUDED.health_message,
    health_checked_at = EXCLUDED.health_checked_at, package_id = EXCLUDED.package_id,
    package_name = EXCLUDED.package_name, health_checks_disabled = EXCLUDED.health_checks_disabled,
    present = true, unavailable_reason = '', updated_at = now();

-- name: SetUnavailableReasons :exec
-- Why a plugin's absent sources are absent: "language" for those its
-- installed packages still provide in a language turned off.
UPDATE sources SET unavailable_reason = CASE WHEN source_id = ANY(@language_off::text[]) THEN 'language' ELSE '' END
WHERE plugin_id = @plugin_id AND NOT present;

-- name: MarkSourcesAbsent :exec
UPDATE sources SET present = false, updated_at = now()
WHERE plugin_id = @plugin_id AND present AND NOT (source_id = ANY(@listed::text[]));

-- name: ListSources :many
SELECT sqlc.embed(sources), plugins.enabled AS plugin_enabled
FROM sources JOIN plugins ON plugins.id = sources.plugin_id
ORDER BY sources.plugin_id, lower(sources.name), sources.id;

-- name: GetSource :one
SELECT sqlc.embed(sources), plugins.enabled AS plugin_enabled
FROM sources JOIN plugins ON plugins.id = sources.plugin_id
WHERE sources.id = $1;

-- name: SetSourceEnabled :execrows
UPDATE sources SET enabled = @enabled, updated_at = now() WHERE id = @id;

-- name: CountSourcesByPlugin :many
SELECT plugin_id, count(*) FILTER (WHERE present) AS listed FROM sources GROUP BY plugin_id;

-- name: GetImageRetrySetting :one
SELECT * FROM image_retry_settings WHERE target_kind = @target_kind AND target_id = @target_id;

-- name: PutImageRetrySetting :one
INSERT INTO image_retry_settings (target_kind, target_id, delays_ms, plugin_id)
VALUES (@target_kind, @target_id, @delays_ms, @plugin_id)
ON CONFLICT (target_kind, target_id) DO UPDATE SET delays_ms = EXCLUDED.delays_ms, updated_at = now()
RETURNING *;

-- name: DeleteImageRetrySetting :execrows
DELETE FROM image_retry_settings WHERE target_kind = @target_kind AND target_id = @target_id;

-- name: ImageRetrySettingsFor :many
-- A source's own setting and its package's.
SELECT * FROM image_retry_settings
WHERE (target_kind = 'source' AND target_id = @source_id) OR (target_kind = 'package' AND target_id = @package_key);
