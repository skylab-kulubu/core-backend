ALTER TABLE users
    ADD COLUMN student_card_uid TEXT;

CREATE UNIQUE INDEX users_student_card_uid_uidx
    ON users (student_card_uid)
    WHERE student_card_uid IS NOT NULL AND student_card_uid <> '';
