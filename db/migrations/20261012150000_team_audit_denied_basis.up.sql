-- Team delegation (ticket 04) records the delegated writes it refuses as
-- denied rows, by whoever asked: a caller with no basis for the change (no
-- Leader of the Group's Teams) is actor_basis 'none'. Only a denied row
-- carries it; a change that is made is always privileged or leader.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_actor_basis_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_actor_basis_check CHECK (
    (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
    OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)
    OR (actor_basis = 'none' AND actor_via_group_id IS NULL AND outcome = 'denied'));
