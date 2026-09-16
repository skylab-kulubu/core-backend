ALTER TABLE users
    DROP COLUMN IF EXISTS profile_picture_url,
    DROP COLUMN IF EXISTS profile_picture_id,
    DROP COLUMN IF EXISTS department,
    DROP COLUMN IF EXISTS faculty,
    DROP COLUMN IF EXISTS university,
    DROP COLUMN IF EXISTS linkedin,
    DROP COLUMN IF EXISTS username;
