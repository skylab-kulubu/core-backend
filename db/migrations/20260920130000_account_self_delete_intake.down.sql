-- Intake writers lock the lifecycle request before inserting the capability.
-- Take the same table order and hold ACCESS EXCLUSIVE through the preflight
-- and drop so no durable receipt can commit between them.
LOCK TABLE account_deletion_requests, account_deletion_self_intakes
    IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF to_regclass('public.account_deletion_self_intakes') IS NOT NULL
       AND EXISTS (SELECT 1 FROM account_deletion_self_intakes) THEN
        RAISE EXCEPTION 'cannot remove self-delete intake after a durable capability exists';
    END IF;
END
$$;

DROP TABLE IF EXISTS account_deletion_self_intakes;
