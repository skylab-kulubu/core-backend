-- Contact consents (ADR-0062, docs/contact-consents.md): a person's explicit
-- consent (KVKK art. 5/1) for one purpose: invitations to future SKY LAB
-- events, or keeping a team application for future recruitment (Forms).
-- One row is one grant: it is recorded, confirmed, and ends (withdrawn,
-- expired, or superseded: a pending grant a product's verified grant for
-- the same address replaced) without ever being reopened; a new grant after
-- an end is a new row, so the history is the proof.
--
-- The subject is exactly one of:
--   * a core user (user_id): a person who consented for themselves while
--     signed in. The address is read from users when it is needed, so no
--     copy is kept;
--   * an e-mail address (guest): email is the normalized address, kept only
--     while the grant is open (it is what invitations are sent to);
--     email_hmac is its keyed HMAC-SHA256 (CONTACT_CONSENT_KEY), kept with the
--     row as the proof key once the address is gone.
--
-- No IP address, user agent or name is kept: the evidence is the purpose,
-- the text version shown, the app and client it came from, the Event it was
-- given on, how the address was shown to be the person's, and the times.
CREATE TABLE IF NOT EXISTS contact_consents (
    id UUID PRIMARY KEY,
    purpose TEXT NOT NULL,
    channel TEXT NOT NULL DEFAULT 'email',
    user_id UUID REFERENCES users (id) ON DELETE CASCADE,
    email TEXT,
    email_hmac BYTEA,
    source TEXT NOT NULL,
    client_id TEXT NOT NULL DEFAULT '',
    text_version TEXT NOT NULL,
    event_id UUID REFERENCES events (id) ON DELETE SET NULL,
    granted_at TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ,
    confirmed_via TEXT,
    confirmation_sent_at TIMESTAMPTZ,
    -- How many confirmation mails a pending grant got: one when recorded,
    -- then at most one a day when it is given again, three in all.
    confirmation_mails SMALLINT NOT NULL DEFAULT 0,
    renewed_at TIMESTAMPTZ,
    renewal_requested_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    ended_reason TEXT,
    ended_via TEXT,
    CONSTRAINT contact_consents_purpose_check CHECK (purpose IN ('event_invitations', 'recruitment_pool')),
    CONSTRAINT contact_consents_channel_check CHECK (channel IN ('email')),
    CONSTRAINT contact_consents_source_check CHECK (source IN ('guest_apply', 'forms', 'place', 'guessr', 'self')),
    CONSTRAINT contact_consents_client_check CHECK (client_id ~ '^([A-Za-z0-9][A-Za-z0-9._-]{0,254})?$'),
    CONSTRAINT contact_consents_text_version_check CHECK (text_version ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    CONSTRAINT contact_consents_subject_check CHECK (
        (user_id IS NOT NULL AND email IS NULL AND email_hmac IS NULL)
        OR (user_id IS NULL AND email_hmac IS NOT NULL AND octet_length(email_hmac) = 32)
    ),
    CONSTRAINT contact_consents_email_check CHECK (email IS NULL OR (email <> '' AND email = lower(btrim(email)))),
    CONSTRAINT contact_consents_confirmed_check CHECK (
        (confirmed_at IS NULL) = (confirmed_via IS NULL)
        AND (confirmed_via IS NULL OR confirmed_via IN ('account', 'service', 'link'))
    ),
    CONSTRAINT contact_consents_ended_check CHECK (
        (ended_at IS NULL) = (ended_reason IS NULL)
        AND (ended_at IS NULL) = (ended_via IS NULL)
        AND (ended_reason IS NULL OR ended_reason IN ('withdrawn', 'expired', 'superseded'))
        AND (ended_via IS NULL OR ended_via IN ('link', 'one_click', 'self', 'service', 'renewal_unanswered'))
    ),
    CONSTRAINT contact_consents_confirmation_mails_check CHECK (confirmation_mails BETWEEN 0 AND 3),
    -- An open guest grant has its address; an ended one never keeps it.
    CONSTRAINT contact_consents_open_email_check CHECK (
        user_id IS NOT NULL OR (ended_at IS NULL) = (email IS NOT NULL)
    )
);

COMMENT ON TABLE contact_consents IS
    'Explicit contact consents (docs/contact-consents.md). No IP, user agent or name. Ended rows keep no address; account erasure deletes a person''s rows.';

-- One open grant per subject and purpose.
CREATE UNIQUE INDEX IF NOT EXISTS contact_consents_open_user_idx
    ON contact_consents (purpose, user_id) WHERE ended_at IS NULL AND user_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS contact_consents_open_email_idx
    ON contact_consents (purpose, email_hmac) WHERE ended_at IS NULL AND email_hmac IS NOT NULL;
-- Erasure and a person's own list find every row of a subject.
CREATE INDEX IF NOT EXISTS contact_consents_user_idx ON contact_consents (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS contact_consents_email_hmac_idx ON contact_consents (email_hmac) WHERE email_hmac IS NOT NULL;
-- The invitation audience: confirmed, open grants of a purpose, by id.
CREATE INDEX IF NOT EXISTS contact_consents_audience_idx
    ON contact_consents (purpose, id) WHERE ended_at IS NULL AND confirmed_at IS NOT NULL;

-- user_id is a current-identity link like any other (account-lifecycle.md):
-- only an active subject with no deletion marker may be named.
DROP TRIGGER IF EXISTS contact_consents_require_active_subject ON contact_consents;
CREATE TRIGGER contact_consents_require_active_subject
    BEFORE INSERT OR UPDATE OF user_id ON contact_consents
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('user_id');
