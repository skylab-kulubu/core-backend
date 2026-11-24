-- Automatic attendance finalization (docs/certificate-rule.md, SPEC §9):
-- an hour after an Event's last live Session ends, core closes its
-- attendance by itself and queues the eligible Tickets' certificates.
--
-- events.attendance_auto_finalize is the Event's switch (on by default, the
-- organizer may turn it off). An Event that has already ended now and whose
-- attendance is not closed gets it off (S3): it is not closed by the clock,
-- so no old Event's people get certificate mails out of the blue; its
-- organizer closes it by hand. "Ended" is the feedback mail's Event end
-- (its end date, else a day after its start) or its last live Session's
-- end (the Session's end time, else its day's end, else the Event's).
ALTER TABLE events ADD COLUMN IF NOT EXISTS attendance_auto_finalize BOOLEAN NOT NULL DEFAULT true;
UPDATE events e SET attendance_auto_finalize = false
WHERE NOT EXISTS (SELECT 1 FROM certificate_event_state f WHERE f.event_id = e.id)
  AND (
    COALESCE(e.end_date, e.start_date + interval '24 hours') <= now()
    OR (
        SELECT max(COALESCE(s.end_time, d.end_date, e.end_date, e.start_date + interval '24 hours'))
        FROM event_days d
        JOIN sessions s ON s.event_day_id = d.id AND s.archived_at IS NULL AND NOT s.cancelled
        WHERE d.event_id = e.id AND d.archived_at IS NULL
    ) <= now()
  );

-- Who closed the attendance: 'manual' (a person, attendance_finalized_by)
-- or 'auto' (core's clock; attendance_finalized_by is NULL). Every
-- finalization written before was manual.
ALTER TABLE certificate_event_state ADD COLUMN IF NOT EXISTS finalized_source TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE certificate_event_state DROP CONSTRAINT IF EXISTS certificate_event_state_finalized_source_check;
ALTER TABLE certificate_event_state ADD CONSTRAINT certificate_event_state_finalized_source_check
    CHECK (finalized_source IN ('manual', 'auto'));

-- "Uygunları yeniden tara": a batch of the Tickets that became eligible
-- after the attendance closed.
ALTER TABLE certificate_batches DROP CONSTRAINT IF EXISTS certificate_batches_reason_check;
ALTER TABLE certificate_batches ADD CONSTRAINT certificate_batches_reason_check
    CHECK (reason IN ('finalization', 'manual', 'reissue', 'rescan'));
