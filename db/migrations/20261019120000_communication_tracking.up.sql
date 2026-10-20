-- Open and click tracking preference (docs/communication-preferences.md,
-- "Tracking"; hub ticket iletisim-servisi 16): a person's objection to
-- SkyMail recording whether they opened a News mail and which link they
-- clicked. Legitimate interest with an opt-out (decision T1): without a row
-- the mode decides (COMMUNICATION_TRACKING_MODE, opt_out by default); the
-- Account center writes the row.
--
--   * scope 'tracking': one row per person, on the 'email' channel, with
--     no group_id and no news_kind. No new source and no text version: no
--     consent is asked and no mail carries a "stop tracking" link (T8).
--   * communication_tracking_withdrawals: the outbox of the withdrawals
--     core still has to tell SkyMail (PUT /internal/v1/tracking-withdrawals/{sub}).
--     A row is written in the transaction that turns tracking off and is
--     deleted once SkyMail answered 204. One row per person: a newer
--     request replaces a waiting one (SkyMail's call is idempotent).
--   * communication_tracking_changes: who turned tracking on or off, when
--     and from where (the audit trail of the objection). No address, no
--     name, no IP.
--
-- Account erasure (step erase_communication_preferences) deletes a person's
-- rows in all three tables; SkyMail deletes its own tracking events in its
-- own erasure step.
ALTER TABLE communication_preferences DROP CONSTRAINT IF EXISTS communication_preferences_category_check;
ALTER TABLE communication_preferences ADD CONSTRAINT communication_preferences_category_check CHECK (
    (scope = 'all' AND group_id IS NULL AND news_kind IS NULL)
    OR (scope = 'club' AND group_id IS NULL AND news_kind IS NOT NULL AND news_kind IN ('duyuru', 'haber'))
    OR (scope = 'team' AND group_id IS NOT NULL AND length(group_id) BETWEEN 1 AND 64
        AND news_kind IS NOT NULL AND news_kind IN ('duyuru', 'haber'))
    OR (scope = 'tracking' AND group_id IS NULL AND news_kind IS NULL AND channel = 'email')
);

CREATE TABLE IF NOT EXISTS communication_tracking_withdrawals (
    user_id UUID PRIMARY KEY,
    requested_at TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    claimed_at TIMESTAMPTZ,
    -- The last answer's HTTP status (0: no answer), for whoever reads the
    -- queue; never a body.
    last_status INTEGER NOT NULL DEFAULT 0
);

COMMENT ON TABLE communication_tracking_withdrawals IS
    'Tracking withdrawals core still has to tell SkyMail (docs/communication-preferences.md, Tracking): one row per person, deleted once SkyMail answered 204; no address.';

CREATE INDEX IF NOT EXISTS communication_tracking_withdrawals_due_idx
    ON communication_tracking_withdrawals (next_attempt_at);

CREATE TABLE IF NOT EXISTS communication_tracking_changes (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id UUID NOT NULL,
    enabled BOOLEAN NOT NULL,
    -- The value before the change; NULL when there was no row (the mode's
    -- default applied).
    previous BOOLEAN,
    source TEXT NOT NULL,
    changed_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT communication_tracking_changes_source_check CHECK (source IN ('account_center', 'admin'))
);

COMMENT ON TABLE communication_tracking_changes IS
    'Audit trail of the tracking preference (docs/communication-preferences.md, Tracking): who turned it on or off, when, from where; no address, no IP; account erasure deletes a person''s rows.';

CREATE INDEX IF NOT EXISTS communication_tracking_changes_user_idx
    ON communication_tracking_changes (user_id);
