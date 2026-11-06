-- Refuses while the register holds a request: dropping it would lose the
-- proof that a request was answered (KVKK md. 13).
DO $$
BEGIN
    IF to_regclass('public.subject_requests') IS NOT NULL
       AND EXISTS (SELECT 1 FROM subject_requests) THEN
        RAISE EXCEPTION 'refusing to drop subject_requests while it holds a request';
    END IF;
END
$$;

DROP TABLE IF EXISTS subject_request_events;
DROP TABLE IF EXISTS subject_requests;
