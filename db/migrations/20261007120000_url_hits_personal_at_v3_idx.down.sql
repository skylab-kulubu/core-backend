-- One statement, outside a transaction, as the up. Run the down of
-- 20261007120100 first: it builds url_hits_personal_at_idx again.
DROP INDEX CONCURRENTLY IF EXISTS url_hits_personal_at_v3_idx;
