SET LOCAL lock_timeout = '5s';
ALTER TABLE certificate_jobs DROP COLUMN IF EXISTS lease_token;
