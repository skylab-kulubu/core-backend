SET LOCAL lock_timeout = '5s';

-- An expired management membership's holder leaves the Group 30 days after
-- its end (ADR-0078 karar 3, Yusuf 2026-11-24;
-- docs/management-membership-terms.md, "Removal"), behind
-- MANAGEMENT_TERMS_AUTO_REMOVE.
--
--   * team_audit_records: core's daily job writes the removal as
--     member.remove with actor_basis 'system', the nil subject as actor_id
--     and system:term-removal as client. The system basis so far took only
--     membership.term.expired.
--   * management_term_runs: each run says how it treated the removal (off,
--     counted or applied) and how many holders it took out, or counted.
--     Counts only.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_actor_basis_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_actor_basis_check CHECK (
    (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
    OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)
    OR (actor_basis = 'none' AND actor_via_group_id IS NULL AND outcome = 'denied')
    OR (actor_basis = 'system' AND actor_via_group_id IS NULL
        AND actor_id = '00000000-0000-0000-0000-000000000000'
        AND action IN ('membership.term.expired', 'member.remove'))
    OR (actor_basis = 'event_editor' AND actor_via_group_id IS NOT NULL
        AND action IN ('ticket.cancel', 'ticket.restore', 'ticket.promote', 'ticket.import', 'ticket.import.preview')));

ALTER TABLE management_term_runs ADD COLUMN IF NOT EXISTS removal TEXT NOT NULL DEFAULT 'off';
ALTER TABLE management_term_runs ADD COLUMN IF NOT EXISTS removed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE management_term_runs DROP CONSTRAINT IF EXISTS management_term_runs_removal_check;
ALTER TABLE management_term_runs ADD CONSTRAINT management_term_runs_removal_check
    CHECK (removal IN ('off', 'dry-run', 'apply'));
