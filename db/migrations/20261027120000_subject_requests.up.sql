-- KVKK md. 11 data subject requests (docs/subject-requests.md; hub
-- .scratch/kvkk-basvuru-kaydi): the register the board (YK) keeps of the
-- requests that reach info@, by post or by hand. The least data that proves
-- the request was answered within 30 days (KVKK md. 13): the requester's
-- name and where the answer went, never an ID number, an address of
-- residence, a verification code or a document. No attachments.
--
--   * due_on is received_on + 30 days, computed here, never entered.
--   * user_id names the account when there is one; deleting the account
--     empties it and keeps the request (the proof outlives the account).
--   * subject_request_events is the audit trail: who created or changed a
--     request, when, which fields and the status after; never a value.
--
-- The periodic destruction run deletes a request three years after it was
-- answered or rejected (rule subject_requests_age, ADR-0062), its trail
-- with it. An open request is never deleted.
CREATE TABLE IF NOT EXISTS subject_requests (
    id UUID PRIMARY KEY,
    received_on DATE NOT NULL,
    channel TEXT NOT NULL CHECK (channel IN ('email', 'post', 'in_person')),
    requester_name TEXT NOT NULL CHECK (btrim(requester_name) <> '' AND char_length(requester_name) <= 200),
    requester_contact TEXT NOT NULL CHECK (btrim(requester_contact) <> '' AND char_length(requester_contact) <= 320),
    user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    request_types TEXT[] NOT NULL CHECK (
        cardinality(request_types) BETWEEN 1 AND 10
        AND request_types <@ ARRAY['learn', 'info', 'purpose', 'recipients', 'rectify', 'erase',
            'notify_recipients', 'object_automated', 'compensation', 'other']::TEXT[]),
    status TEXT NOT NULL CHECK (status IN ('received', 'awaiting_verification', 'answered', 'rejected')),
    due_on DATE GENERATED ALWAYS AS (received_on + 30) STORED,
    answered_on DATE,
    answer_summary TEXT NOT NULL DEFAULT '' CHECK (char_length(answer_summary) <= 2000),
    responder TEXT NOT NULL DEFAULT '' CHECK (char_length(responder) <= 100),
    created_at TIMESTAMPTZ NOT NULL,
    created_by UUID,
    updated_at TIMESTAMPTZ NOT NULL,
    updated_by UUID,
    -- An answered or rejected request says when; an open one has no date.
    CONSTRAINT subject_requests_answered_check CHECK (
        (status IN ('answered', 'rejected')) = (answered_on IS NOT NULL)
        AND (answered_on IS NULL OR answered_on >= received_on))
);

COMMENT ON TABLE subject_requests IS
    'KVKK md. 11 data subject requests (docs/subject-requests.md): name and contact only; deleted three years after the answer (subject_requests_age).';

-- The open requests by due date (the panel list), and the destruction
-- run's anchor.
CREATE INDEX IF NOT EXISTS subject_requests_open_idx ON subject_requests (due_on) WHERE answered_on IS NULL;
CREATE INDEX IF NOT EXISTS subject_requests_answered_idx ON subject_requests (answered_on) WHERE answered_on IS NOT NULL;

CREATE TABLE IF NOT EXISTS subject_request_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id UUID NOT NULL REFERENCES subject_requests (id) ON DELETE CASCADE,
    occurred_at TIMESTAMPTZ NOT NULL,
    actor_id UUID,
    action TEXT NOT NULL CHECK (action IN ('create', 'update')),
    -- The names of the fields the change wrote, never their values.
    fields TEXT[] NOT NULL DEFAULT '{}',
    status TEXT NOT NULL CHECK (status IN ('received', 'awaiting_verification', 'answered', 'rejected'))
);

CREATE INDEX IF NOT EXISTS subject_request_events_request_idx ON subject_request_events (request_id, occurred_at);
