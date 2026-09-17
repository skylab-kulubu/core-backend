DROP INDEX IF EXISTS users_student_card_uid_uidx;

ALTER TABLE users
    DROP COLUMN IF EXISTS student_card_uid;
