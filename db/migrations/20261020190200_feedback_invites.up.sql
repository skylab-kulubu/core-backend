-- Feedback invites (etkinlik geri bildirim formu, ticket 04; Yusuf's
-- decisions K4 and K5 of 2026-10-20): one row per Ticket and target (the
-- Event, or one of its Sessions) whose feedback form was mailed to the
-- Ticket's holder, so that the reminder goes only to whom has not answered.
--
--   * code: the invite's own code (128 random bits, base64url, 22
--     characters), in the mail's link as ?davet=<code>. Stored plain: the
--     reminder sends the same link again, and the code can do one thing
--     only, mark the invite answered.
--   * sent_at, reminded_at, answered_at: when it was mailed, reminded,
--     answered (by its code, or by the signed-in answerer's Ticket).
--   * reminder_cancelled_at: the form closed (ticket 07); set once, the
--     reminder never goes after it.
--
-- A row goes 90 days after it was sent (the retention sweep's
-- feedback_invites_age rule), and with the person's account erasure. The
-- counts per target stay in feedback_invite_counts, kept by triggers: they
-- name nobody.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS feedback_invites (
    id UUID PRIMARY KEY,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    session_id UUID REFERENCES sessions (id) ON DELETE CASCADE,
    form_id UUID NOT NULL,
    code TEXT NOT NULL CHECK (code ~ '^[A-Za-z0-9_-]{22}$'),
    sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reminded_at TIMESTAMPTZ,
    answered_at TIMESTAMPTZ,
    reminder_cancelled_at TIMESTAMPTZ,
    CONSTRAINT feedback_invites_code_key UNIQUE (code),
    CONSTRAINT feedback_invites_target_key UNIQUE NULLS NOT DISTINCT (ticket_id, event_id, session_id)
);

CREATE INDEX IF NOT EXISTS feedback_invites_form_idx ON feedback_invites (form_id);
CREATE INDEX IF NOT EXISTS feedback_invites_sent_idx ON feedback_invites (sent_at);

CREATE TABLE IF NOT EXISTS feedback_invite_counts (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    session_id UUID REFERENCES sessions (id) ON DELETE CASCADE,
    sent INTEGER NOT NULL DEFAULT 0 CHECK (sent >= 0),
    answered INTEGER NOT NULL DEFAULT 0 CHECK (answered >= 0),
    CONSTRAINT feedback_invite_counts_target_key UNIQUE NULLS NOT DISTINCT (event_id, session_id)
);

-- An invite written counts as sent (and answered, when it is); an invite
-- answered for the first time counts as answered. Deleting an invite (its
-- age, an account erasure) leaves the counts.
CREATE OR REPLACE FUNCTION public.feedback_invite_count()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO public.feedback_invite_counts AS c (event_id, session_id, sent, answered)
        VALUES (NEW.event_id, NEW.session_id, 1, (NEW.answered_at IS NOT NULL)::int)
        ON CONFLICT ON CONSTRAINT feedback_invite_counts_target_key
        DO UPDATE SET sent = c.sent + 1, answered = c.answered + EXCLUDED.answered;
    ELSIF OLD.answered_at IS NULL AND NEW.answered_at IS NOT NULL THEN
        UPDATE public.feedback_invite_counts
        SET answered = answered + 1
        WHERE event_id = NEW.event_id AND session_id IS NOT DISTINCT FROM NEW.session_id;
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS feedback_invites_count ON feedback_invites;
CREATE TRIGGER feedback_invites_count
    AFTER INSERT OR UPDATE OF answered_at ON feedback_invites
    FOR EACH ROW EXECUTE FUNCTION public.feedback_invite_count();
