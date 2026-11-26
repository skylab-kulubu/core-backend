-- "Sertifikaları otomatik gönder" (docs/certificate-rule.md): a new Event's
-- switch events.attendance_auto_finalize is off unless the organizer turns
-- it on, so no certificate goes out by the clock with a template nobody
-- chose. Only the column's default changes; every Event keeps its value.
ALTER TABLE events ALTER COLUMN attendance_auto_finalize SET DEFAULT false;
