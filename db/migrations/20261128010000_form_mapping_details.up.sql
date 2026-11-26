SET LOCAL lock_timeout = '5s';

-- A form mapping's details (docs/form-mappings.md#the-details): which
-- questions of an Event's form hold a guest's phone number, university,
-- department and grade (year of study). Each is optional; the guest's
-- Ticket takes the answers into the columns a Walk-in fills
-- (20261127220000: guest_phone_number, guest_university,
-- guest_department, guest_grade). Question ids only, never an answer.
--
-- event_form_respondents keeps a guest's details until their Ticket is
-- sent (an Event whose "Otomatik bilet gönder" is off): empty for none and
-- for a signed-in person's row; the row's deletion takes them, as its name.
--
-- The release before neither reads nor writes the new columns (they are
-- nullable or have defaults).
ALTER TABLE event_form_mappings ADD COLUMN IF NOT EXISTS phone_question TEXT;
ALTER TABLE event_form_mappings ADD COLUMN IF NOT EXISTS university_question TEXT;
ALTER TABLE event_form_mappings ADD COLUMN IF NOT EXISTS department_question TEXT;
ALTER TABLE event_form_mappings ADD COLUMN IF NOT EXISTS grade_question TEXT;
ALTER TABLE event_form_mappings DROP CONSTRAINT IF EXISTS event_form_mappings_details_check;
ALTER TABLE event_form_mappings ADD CONSTRAINT event_form_mappings_details_check CHECK (
    (phone_question IS NULL OR (btrim(phone_question) <> '' AND length(phone_question) <= 100))
    AND (university_question IS NULL OR (btrim(university_question) <> '' AND length(university_question) <= 100))
    AND (department_question IS NULL OR (btrim(department_question) <> '' AND length(department_question) <= 100))
    AND (grade_question IS NULL OR (btrim(grade_question) <> '' AND length(grade_question) <= 100)));

ALTER TABLE event_form_respondents ADD COLUMN IF NOT EXISTS phone_number TEXT NOT NULL DEFAULT '';
ALTER TABLE event_form_respondents ADD COLUMN IF NOT EXISTS university TEXT NOT NULL DEFAULT '';
ALTER TABLE event_form_respondents ADD COLUMN IF NOT EXISTS department TEXT NOT NULL DEFAULT '';
ALTER TABLE event_form_respondents ADD COLUMN IF NOT EXISTS grade TEXT NOT NULL DEFAULT '';
ALTER TABLE event_form_respondents DROP CONSTRAINT IF EXISTS event_form_respondents_details_check;
ALTER TABLE event_form_respondents ADD CONSTRAINT event_form_respondents_details_check CHECK (
    char_length(phone_number) <= 32 AND char_length(university) <= 200 AND char_length(department) <= 200
    AND grade IN ('', 'prep', '1', '2', '3', '4', 'masters', 'phd', 'graduate', 'other')
    AND (user_id IS NULL OR (phone_number = '' AND university = '' AND department = '' AND grade = '')));
