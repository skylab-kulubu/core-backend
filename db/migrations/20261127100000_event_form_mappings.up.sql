SET LOCAL lock_timeout = '5s';

-- Form mappings (docs/form-mappings.md): for a form an Event lists as its
-- application or an extra form, which questions hold a guest's first name,
-- last name, full name ("ad soyad" in one question) and e-mail. Core builds
-- a guest Ticket from those answers when Forms' report of an answer names
-- nobody. Only question ids (Forms' schema item ids, at most 100 characters)
-- are kept, never an answer.
--
--   * email_question is required; either first and last name questions, or a
--     full name question, too.
--   * updated_by is who saved it last (a person's id, for the audit), with
--     updated_at.
--
-- PUT /v1/events/{id}/forms/{formId}/mapping writes a row; a row whose form
-- the Event no longer lists is not read (the Event lists the form again: it
-- is read again). The Event's deletion takes its rows. The release before
-- neither reads nor writes the table.
CREATE TABLE IF NOT EXISTS event_form_mappings (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    form_id UUID NOT NULL,
    first_name_question TEXT,
    last_name_question TEXT,
    full_name_question TEXT,
    email_question TEXT NOT NULL,
    updated_by UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, form_id),
    CONSTRAINT event_form_mappings_names_check
        CHECK ((first_name_question IS NOT NULL AND last_name_question IS NOT NULL) OR full_name_question IS NOT NULL),
    CONSTRAINT event_form_mappings_ids_check CHECK (
        btrim(email_question) <> '' AND length(email_question) <= 100
        AND (first_name_question IS NULL OR (btrim(first_name_question) <> '' AND length(first_name_question) <= 100))
        AND (last_name_question IS NULL OR (btrim(last_name_question) <> '' AND length(last_name_question) <= 100))
        AND (full_name_question IS NULL OR (btrim(full_name_question) <> '' AND length(full_name_question) <= 100)))
);
COMMENT ON TABLE event_form_mappings IS
    'Form mappings (docs/form-mappings.md): the questions of an Event''s form that name a guest; ids only, never an answer.';

-- An answer's report names the form, not the Event.
CREATE INDEX IF NOT EXISTS event_form_mappings_form_idx ON event_form_mappings (form_id);
