-- The staging rows of the pending objects stay: the sweeper still deletes
-- the objects and aborts their multipart uploads when they expire. Only the
-- uploads' details (purpose, file name, sizes) go, so none can complete.
-- The table references media_upload_staging (20260920010000): roll this
-- back before that migration's down.
DROP TABLE IF EXISTS media_direct_uploads;
