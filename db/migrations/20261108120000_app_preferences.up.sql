-- App preferences (docs/app-preferences.md, ADR 0081): a person's small
-- per-app settings, such as whether Forms' "Formlarım" list shows details.
-- One row per person, app and key; the value is the app's JSON, which core
-- stores, limits and deletes but never reads. Only a person's explicit
-- choices are rows; the app gives the default.
--
--   * app: the namespace, one of APP_PREFERENCE_CLIENTS' apps. The token's
--     azp picks it; the list is configuration, not a table.
--   * key: the app's own name for one setting (forms.list).
--   * value_bytes: the value's compact JSON size, which core limits (4 KB a
--     value, 32 KB an app). The value's own CHECK is a loose backstop:
--     jsonb's text form adds spaces the compact size does not count.
--   * schema_version: the app's version of the value's shape; core does not
--     read it.
--   * version: one more on every write; the ETag and If-Match read it.
--   * client_id: the azp that wrote the row, for observation.
--
-- No name, address, form content or search text belongs in a value (the
-- rule is the apps'; core does not look). user_id is a Keycloak subject
-- without a foreign key, as in communication_preferences. Account erasure
-- deletes a person's rows (step erase_app_preferences); the trigger below
-- refuses a new row for a person being erased or erased.
CREATE TABLE IF NOT EXISTS app_preferences (
    user_id UUID NOT NULL,
    app TEXT NOT NULL,
    key TEXT NOT NULL,
    value JSONB NOT NULL,
    value_bytes INTEGER NOT NULL,
    schema_version INTEGER NOT NULL DEFAULT 1,
    version BIGINT NOT NULL DEFAULT 1,
    client_id TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT app_preferences_pkey PRIMARY KEY (user_id, app, key),
    CONSTRAINT app_preferences_app_check CHECK (app ~ '^[a-z][a-z0-9-]{1,31}$'),
    CONSTRAINT app_preferences_key_check CHECK (key ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    CONSTRAINT app_preferences_value_bytes_check CHECK (value_bytes BETWEEN 1 AND 4096),
    CONSTRAINT app_preferences_value_size_check CHECK (octet_length(value::text) <= 16384),
    CONSTRAINT app_preferences_schema_version_check CHECK (schema_version BETWEEN 1 AND 1000),
    CONSTRAINT app_preferences_version_check CHECK (version >= 1),
    CONSTRAINT app_preferences_client_check CHECK (length(client_id) BETWEEN 1 AND 255)
);

COMMENT ON TABLE app_preferences IS
    'App preferences (docs/app-preferences.md, ADR 0081): opaque per-app view settings of a person, explicit choices only; account erasure deletes a person''s rows.';

-- A person being erased (a deletion marker) or erased (a users row that is
-- no longer active) gets no new row. The lock is the one account deletion
-- takes for the subject, so a write and a deletion request cannot cross.
CREATE OR REPLACE FUNCTION public.app_preferences_refuse_inactive_subject()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.user_id::TEXT, 6001410475649093715));
    IF EXISTS (
        SELECT 1 FROM public.users WHERE id = NEW.user_id AND account_state <> 'active'
    ) OR EXISTS (
        SELECT 1 FROM public.account_deletion_requests WHERE subject_id = NEW.user_id
    ) THEN
        RAISE EXCEPTION 'account subject is not active'
            USING ERRCODE = '23514', CONSTRAINT = 'active_account_reference';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS app_preferences_require_active_subject ON app_preferences;
CREATE TRIGGER app_preferences_require_active_subject
    BEFORE INSERT OR UPDATE OF user_id ON app_preferences
    FOR EACH ROW EXECUTE FUNCTION public.app_preferences_refuse_inactive_subject();
