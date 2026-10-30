-- The terms, the mails waiting and the runs are lost; Keycloak's memberships
-- are untouched (core never took anyone out). An older core never reads
-- these tables.
DROP TABLE IF EXISTS management_term_runs;
DROP TABLE IF EXISTS management_term_mails;
DROP TABLE IF EXISTS management_membership_terms;
