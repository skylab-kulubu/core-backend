-- url_hits_personal_at_v3_idx (20261007120000) serves url_hits_scrub v3; v2's
-- index serves nothing now. One statement, outside a transaction: dropping it
-- concurrently never blocks a click.
DROP INDEX CONCURRENTLY IF EXISTS url_hits_personal_at_idx;
