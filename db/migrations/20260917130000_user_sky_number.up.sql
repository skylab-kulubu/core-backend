ALTER TABLE users
    ADD COLUMN sky_number TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX users_sky_number_uidx
    ON users (sky_number)
    WHERE sky_number <> '';
