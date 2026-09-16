DROP INDEX IF EXISTS users_sky_number_uidx;

ALTER TABLE users
    DROP COLUMN IF EXISTS sky_number;
