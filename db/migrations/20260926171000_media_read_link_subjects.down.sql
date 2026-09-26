-- The trigger uses the account lifecycle guard (20260920010000): roll this
-- back before that migration's down.
DROP TRIGGER IF EXISTS media_read_links_require_active_subject ON media_read_links;
