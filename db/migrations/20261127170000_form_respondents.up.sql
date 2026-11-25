SET LOCAL lock_timeout = '5s';

-- Unticketed respondents (docs/form-respondents.md; hub SPEC
-- form-alan-eslestirme §7): a person who answered the form an Event lists
-- and holds no Ticket on it yet. The organizer sends their Tickets from the
-- panel; the row goes once they hold one.
--
--   * event_form_respondents: one row an answer on an Event. A signed-in
--     answer keeps only user_id (the panel reads the name from the person
--     directory); a guest's keeps the first and last name and the e-mail
--     (Forms' named guest, or the Event's form mapping). Never an answer's
--     text. forms_status is accepted or pending (a declined answer's row is
--     deleted); source is collect (answered before the form was linked) or
--     new (came after, with "Otomatik bilet gönder" off, or pending).
--     The rows of a form go when the Event lets the form go, the person's
--     on account erasure, every row two years after the Event's end
--     (retention rule form_respondents).
--   * events.form_responses_auto_ticket: "Otomatik bilet gönder", on by
--     default as before; off, a newly accepted answer is kept as a row.
--   * form_pulls kind collect: the gathering of a form's answers when it is
--     linked. It writes rows, never a Ticket.
--   * respondent_ticket_jobs: an organizer's "send them all" (all: true),
--     run in the background; counts only.
--
-- The release before neither reads nor writes any of them.
CREATE TABLE IF NOT EXISTS event_form_respondents (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    form_id UUID NOT NULL,
    form_response_id UUID NOT NULL,
    user_id UUID,
    first_name TEXT,
    last_name TEXT,
    email TEXT,
    submitted_at TIMESTAMPTZ,
    forms_status TEXT NOT NULL,
    source TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT event_form_respondents_status_check CHECK (forms_status IN ('accepted', 'pending')),
    CONSTRAINT event_form_respondents_source_check CHECK (source IN ('collect', 'new')),
    CONSTRAINT event_form_respondents_person_check CHECK (
        (user_id IS NOT NULL AND first_name IS NULL AND last_name IS NULL AND email IS NULL)
        OR (user_id IS NULL AND btrim(first_name) <> '' AND btrim(last_name) <> '' AND btrim(email) <> ''
            AND length(first_name) <= 100 AND length(last_name) <= 100 AND length(email) <= 254))
);
COMMENT ON TABLE event_form_respondents IS
    'Unticketed respondents (docs/form-respondents.md): who answered an Event''s form and holds no Ticket on it; name and e-mail only, never an answer.';

CREATE UNIQUE INDEX IF NOT EXISTS event_form_respondents_response_idx ON event_form_respondents (event_id, form_response_id);
CREATE INDEX IF NOT EXISTS event_form_respondents_list_idx ON event_form_respondents (event_id, submitted_at, id);
CREATE INDEX IF NOT EXISTS event_form_respondents_answer_idx ON event_form_respondents (form_response_id);
CREATE INDEX IF NOT EXISTS event_form_respondents_user_idx ON event_form_respondents (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS event_form_respondents_email_idx ON event_form_respondents (email) WHERE email IS NOT NULL;

ALTER TABLE events ADD COLUMN IF NOT EXISTS form_responses_auto_ticket BOOLEAN NOT NULL DEFAULT true;

ALTER TABLE form_pulls DROP CONSTRAINT IF EXISTS form_pulls_kind_check;
ALTER TABLE form_pulls ADD CONSTRAINT form_pulls_kind_check CHECK (kind IN ('backfill', 'catchup', 'collect'));
ALTER TABLE form_pulls DROP CONSTRAINT IF EXISTS form_pulls_shape_check;
ALTER TABLE form_pulls ADD CONSTRAINT form_pulls_shape_check CHECK (
    (kind IN ('backfill', 'collect') AND event_id IS NOT NULL AND changed_since IS NULL)
    OR (kind = 'catchup' AND event_id IS NULL AND requested_by IS NULL AND changed_since IS NOT NULL));
-- A collect job a rollback parked (the down migration) runs again.
UPDATE form_pulls SET next_attempt_at = now() WHERE kind = 'collect' AND next_attempt_at = 'infinity';

CREATE TABLE IF NOT EXISTS respondent_ticket_jobs (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    form_id UUID,
    requested_by UUID,
    state TEXT NOT NULL DEFAULT 'queued',
    recorded INTEGER NOT NULL DEFAULT 0,
    waitlisted INTEGER NOT NULL DEFAULT 0,
    full_count INTEGER NOT NULL DEFAULT 0,
    skipped INTEGER NOT NULL DEFAULT 0,
    after_id UUID,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT respondent_ticket_jobs_state_check CHECK (state IN ('queued', 'running', 'done', 'failed'))
);
COMMENT ON TABLE respondent_ticket_jobs IS
    'An organizer''s "send every unticketed respondent a Ticket" (docs/form-respondents.md): counts only; requested_by is the organizer''s subject; after_id the last respondent row handled.';

-- One waiting job an Event and form (or the whole Event): the same request
-- twice finds the first.
CREATE UNIQUE INDEX IF NOT EXISTS respondent_ticket_jobs_open_idx ON respondent_ticket_jobs (event_id, COALESCE(form_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE state IN ('queued', 'running');
CREATE INDEX IF NOT EXISTS respondent_ticket_jobs_due_idx ON respondent_ticket_jobs (next_attempt_at) WHERE state IN ('queued', 'running');
