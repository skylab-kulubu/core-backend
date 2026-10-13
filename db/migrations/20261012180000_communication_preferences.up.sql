-- Communication preferences (docs/communication-preferences.md): for each
-- channel, which News (duyuru, haber) a person wants, per source (club-wide
-- or one team), with one master switch per channel (the Duyuru tercihi of
-- ADR 0067). Only a person's explicit choices are rows; everything else is
-- the default core computes from the current Keycloak team tree (club, the
-- person's own teams and every team below them on, other teams off).
-- Transactional messages (sign-in codes, tickets, certificates) never read
-- this table.
--
--   * channel: how the message reaches the person. Only 'email' today, as
--     in contact_consents; the in-app inbox, push and SMS join by widening
--     the CHECK (inbox, push, sms) when their channel ships.
--   * scope 'all': the channel's master switch ("no duyuru or haber at
--     all"). group_id and news_kind are NULL.
--   * scope 'club': club-wide News of one kind. group_id is NULL.
--   * scope 'team': one team's News of one kind, the team kept by its
--     Keycloak Group id (renaming or moving a team keeps the choice).
--
-- Not a contact consent (docs/contact-consents.md): no grant, no text
-- version, no proof. No address, no name. user_id is a Keycloak subject; core
-- may hold no users row for it (a Member who never signed in to a core app
-- still unsubscribes from the link in a mail), so there is no foreign key.
-- Account erasure deletes a person's rows (step
-- erase_communication_preferences); the trigger below refuses a new row for
-- a person being erased or erased.
CREATE TABLE IF NOT EXISTS communication_preferences (
    user_id UUID NOT NULL,
    channel TEXT NOT NULL DEFAULT 'email',
    scope TEXT NOT NULL,
    group_id TEXT,
    news_kind TEXT,
    enabled BOOLEAN NOT NULL,
    source TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT communication_preferences_channel_check CHECK (channel IN ('email')),
    CONSTRAINT communication_preferences_category_check CHECK (
        (scope = 'all' AND group_id IS NULL AND news_kind IS NULL)
        OR (scope = 'club' AND group_id IS NULL AND news_kind IS NOT NULL AND news_kind IN ('duyuru', 'haber'))
        OR (scope = 'team' AND group_id IS NOT NULL AND length(group_id) BETWEEN 1 AND 64
            AND news_kind IS NOT NULL AND news_kind IN ('duyuru', 'haber'))
    ),
    CONSTRAINT communication_preferences_source_check CHECK (source IN ('account_center', 'unsubscribe_link', 'admin')),
    CONSTRAINT communication_preferences_key UNIQUE NULLS NOT DISTINCT (user_id, channel, scope, group_id, news_kind)
);

COMMENT ON TABLE communication_preferences IS
    'Communication preferences (docs/communication-preferences.md): explicit choices per channel only, no address; transactional messages never read them; account erasure deletes a person''s rows.';

-- The daily cleanup finds the rows of deleted teams by Group id.
CREATE INDEX IF NOT EXISTS communication_preferences_group_idx ON communication_preferences (group_id) WHERE group_id IS NOT NULL;

-- A person being erased (a deletion marker) or erased (a users row that is
-- no longer active) gets no new row: an unsubscribe link opened after the
-- erasure must not write their id back. The lock is the one account deletion
-- takes for the subject, so a row and a deletion request cannot cross.
CREATE OR REPLACE FUNCTION public.communication_preferences_refuse_inactive_subject()
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

DROP TRIGGER IF EXISTS communication_preferences_require_active_subject ON communication_preferences;
CREATE TRIGGER communication_preferences_require_active_subject
    BEFORE INSERT OR UPDATE OF user_id ON communication_preferences
    FOR EACH ROW EXECUTE FUNCTION public.communication_preferences_refuse_inactive_subject();
