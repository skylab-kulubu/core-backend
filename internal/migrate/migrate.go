package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
)

type file struct {
	version int64
	name    string
}

var fingerprints = map[int64]string{
	20260916140000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'users'`,
	20260916160000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'events'`,
	20260916170000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'tickets'`,
	20260916180000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'sessions'`,
	20260916190000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'competitors'`,
	20260916200000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'media'`,
	20260917100000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'urls'`,
	20260917110000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'events' AND column_name = 'cover_image_id'`,
	20260917120000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'school_email'`,
	20260917130000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'sky_number'`,
	20260917140000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'username'`,
	20260917150000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'event_images'`,
	20260917160000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'student_card_uid'`,
	20260917170000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'ticket_checkins' AND column_name = 'session_id'`,
	20260917171000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'event_door_staff'`,
	20260917180000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'certificates'`,
	20260918180000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'events' AND column_name = 'extra_form_urls'`,
	20260919120000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'phone'`,
	20260919130000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'url_hits' AND to_regclass('public.url_hits_url_id_at_idx') IS NOT NULL`,
	20260919200000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'certificate_templates'`,
	20260919210000: `
		SELECT 1
		WHERE (
			SELECT count(*)
			FROM (VALUES
				('events', 'archived_at'),
				('events', 'archived_by'),
				('event_days', 'archived_at'),
				('event_days', 'archived_by'),
				('sessions', 'archived_at'),
				('sessions', 'archived_by'),
				('seasons', 'archived_at'),
				('seasons', 'archived_by'),
				('competitors', 'withdrawn_at'),
				('competitors', 'withdrawn_by'),
				('media', 'deleted_at'),
				('media', 'deleted_by'),
				('urls', 'disabled_at'),
				('urls', 'disabled_by')
			) AS expected(table_name, column_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = expected.table_name
			 AND actual.column_name = expected.column_name
		) = 14
		AND (
			SELECT count(*)
			FROM (VALUES
				('events_current_owner_team_idx'),
				('event_days_current_event_idx'),
				('sessions_current_event_day_idx'),
				('seasons_current_start_date_idx'),
				('competitors_current_event_idx'),
				('competitors_current_user_idx'),
				('media_current_created_at_idx'),
				('urls_current_created_by_idx')
			) AS expected(index_name)
			JOIN pg_indexes actual
			  ON actual.schemaname = 'public'
			 AND actual.indexname = expected.index_name
		) = 8`,
	20260919211000: `
		SELECT 1
		WHERE (
			SELECT count(*)
			FROM (VALUES
				('blob_purge_started_at'),
				('blob_purged_at'),
				('blob_purge_checked_at')
			) AS expected(column_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = 'media'
			 AND actual.column_name = expected.column_name
		) = 3
		AND to_regclass('public.media_blob_purge_candidates_idx') IS NOT NULL
		AND (
			SELECT count(DISTINCT actual.trigger_name)
			FROM (VALUES
				('events_require_current_cover_media'),
				('event_images_require_current_media'),
				('users_require_current_profile_media'),
				('certificate_templates_require_current_media'),
				('certificate_template_versions_require_current_media')
			) AS expected(trigger_name)
			JOIN information_schema.triggers actual
			  ON actual.trigger_schema = 'public'
			 AND actual.trigger_name = expected.trigger_name
		) = 5`,
	20260920010000: `
			SELECT 1
			WHERE (
			SELECT count(*)
			FROM (VALUES
				('account_state'),
				('deletion_requested_at'),
				('anonymized_at')
			) AS expected(column_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = 'users'
			 AND actual.column_name = expected.column_name
			) = 3
			AND EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'account_state'
				  AND is_nullable = 'NO' AND column_default = '''active''::text'
			)
			AND to_regclass('public.account_deletion_requests') IS NOT NULL
			AND to_regclass('public.account_deletion_steps') IS NOT NULL
			AND to_regclass('public.account_deletion_outbox') IS NOT NULL
			AND to_regclass('public.media_upload_staging') IS NOT NULL
			AND (
				SELECT count(*) FROM (VALUES
					('object_key', 'text', 'NO'),
					('subject_id', 'uuid', 'NO'),
					('cleanup_after', 'timestamptz', 'NO'),
					('attempt_count', 'int4', 'NO'),
					('last_error_code', 'text', 'NO'),
					('created_at', 'timestamptz', 'NO')
				) expected(column_name, udt_name, is_nullable)
				JOIN information_schema.columns actual
				  ON actual.table_schema='public'
				 AND actual.table_name='media_upload_staging'
				 AND actual.column_name=expected.column_name
				 AND actual.udt_name=expected.udt_name
				 AND actual.is_nullable=expected.is_nullable
			) = 6
			AND (
				SELECT count(*) FROM (VALUES ('lease_token'), ('profile_media_id')) expected(column_name)
				JOIN information_schema.columns actual
				  ON actual.table_schema = 'public'
				 AND actual.table_name = 'account_deletion_requests'
				 AND actual.column_name = expected.column_name
				 AND actual.udt_name = 'uuid'
				 AND actual.is_nullable = 'YES'
			) = 2
			AND (
				SELECT count(*)
				FROM (VALUES ('competitors', 'user_id'), ('media', 'uploaded_by')) expected(table_name, column_name)
				JOIN pg_attribute attribute
				  ON attribute.attrelid = to_regclass('public.' || expected.table_name)
				 AND attribute.attname = expected.column_name
				 AND NOT attribute.attnotnull
				JOIN pg_constraint foreign_key
				  ON foreign_key.conrelid = attribute.attrelid
				 AND foreign_key.contype = 'f'
				 AND attribute.attnum = ANY(foreign_key.conkey)
				 AND cardinality(foreign_key.conkey) = 1
				 AND foreign_key.confrelid = 'public.users'::regclass
				 AND foreign_key.confdeltype = 'n'
			) = 2
			AND (
				SELECT count(*) FROM (VALUES
					('users', 'users_account_state_check', 'c', 'CHECK ((account_state = ANY (ARRAY[''active''::text, ''deletion_pending''::text, ''anonymized''::text])))'),
					('account_deletion_requests', 'account_deletion_requests_pkey', 'p', 'PRIMARY KEY (id)'),
					('account_deletion_requests', 'account_deletion_requests_subject_id_key', 'u', 'UNIQUE (subject_id)'),
					('account_deletion_requests', 'account_deletion_requests_requested_by_fkey', 'f', 'FOREIGN KEY (requested_by) REFERENCES users(id) ON DELETE SET NULL'),
					('account_deletion_requests', 'account_deletion_requests_status_check', 'c', 'CHECK ((status = ANY (ARRAY[''pending''::text, ''processing''::text, ''completed''::text, ''manual_intervention''::text])))'),
					('account_deletion_requests', 'account_deletion_requests_attempt_count_check', 'c', 'CHECK ((attempt_count >= 0))'),
					('account_deletion_steps', 'account_deletion_steps_pkey', 'p', 'PRIMARY KEY (request_id, step)'),
					('account_deletion_steps', 'account_deletion_steps_request_id_fkey', 'f', 'FOREIGN KEY (request_id) REFERENCES account_deletion_requests(id) ON DELETE CASCADE'),
					('account_deletion_steps', 'account_deletion_steps_step_check', 'c', 'CHECK ((step = ANY (ARRAY[''disable_identity''::text, ''logout_sessions''::text, ''anonymize_core''::text, ''erase_profile_media''::text, ''erase_staged_uploads''::text, ''delete_identity''::text])))'),
					('account_deletion_outbox', 'account_deletion_outbox_pkey', 'p', 'PRIMARY KEY (id)'),
					('account_deletion_outbox', 'account_deletion_outbox_request_id_key', 'u', 'UNIQUE (request_id)'),
					('account_deletion_outbox', 'account_deletion_outbox_request_id_fkey', 'f', 'FOREIGN KEY (request_id) REFERENCES account_deletion_requests(id) ON DELETE CASCADE'),
					('account_deletion_outbox', 'account_deletion_outbox_event_type_check', 'c', 'CHECK ((event_type = ''account.deletion_requested''::text))'),
					('account_deletion_outbox', 'account_deletion_outbox_attempt_count_check', 'c', 'CHECK ((attempt_count >= 0))'),
					('media_upload_staging', 'media_upload_staging_pkey', 'p', 'PRIMARY KEY (object_key)'),
					('media_upload_staging', 'media_upload_staging_attempt_count_check', 'c', 'CHECK ((attempt_count >= 0))')
				) expected(table_name, constraint_name, constraint_type, expected_definition)
				JOIN pg_constraint actual
				  ON actual.conrelid = to_regclass('public.' || expected.table_name)
				 AND actual.conname = expected.constraint_name
				 AND actual.contype = expected.constraint_type::char
				 AND pg_get_constraintdef(actual.oid) = expected.expected_definition
			) = 16
			AND (
				SELECT count(*) FROM (VALUES
					('users_account_state_idx', '(account_state, updated_at)', ARRAY[]::text[]),
					('account_deletion_requests_claim_idx', '(next_attempt_at, created_at)', ARRAY['WHERE','pending','processing']::text[]),
					('account_deletion_outbox_pending_idx', '(available_at, created_at)', ARRAY['WHERE','published_at','IS NULL']::text[]),
					('media_upload_staging_cleanup_idx', '(cleanup_after, created_at)', ARRAY[]::text[]),
					('media_upload_staging_subject_idx', '(subject_id, cleanup_after, created_at)', ARRAY[]::text[])
				) expected(index_name, key_fragment, required_fragments)
				JOIN pg_indexes actual
				  ON actual.schemaname = 'public'
				 AND actual.indexname = expected.index_name
				 AND position(expected.key_fragment IN actual.indexdef) > 0
				 AND NOT EXISTS (
					SELECT 1 FROM unnest(expected.required_fragments) fragment
					WHERE position(fragment IN actual.indexdef) = 0
				 )
			) = 5
			AND EXISTS (
				SELECT 1
				FROM pg_proc actual
				JOIN pg_namespace namespace ON namespace.oid=actual.pronamespace
				WHERE namespace.nspname='public'
				  AND actual.proname='require_active_account_reference'
				  AND actual.prorettype='trigger'::regtype
				  AND actual.pronargs=0
				  AND actual.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql')
				  AND btrim(regexp_replace(actual.prosrc, '[[:space:]]+', ' ', 'g')) =
				      btrim(regexp_replace($guard$
DECLARE
    column_name TEXT;
    candidate_subject UUID;
    subject_ids UUID[] := ARRAY[]::UUID[];
BEGIN
    FOREACH column_name IN ARRAY TG_ARGV LOOP
        candidate_subject := NULLIF(to_jsonb(NEW)->>column_name, '')::UUID;
        IF candidate_subject IS NOT NULL THEN
            subject_ids := array_append(subject_ids, candidate_subject);
        END IF;
    END LOOP;

    SELECT COALESCE(array_agg(DISTINCT value ORDER BY value), ARRAY[]::UUID[])
    INTO subject_ids
    FROM unnest(subject_ids) AS value;

    FOREACH candidate_subject IN ARRAY subject_ids LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended(candidate_subject::TEXT, 6001410475649093715));
        IF NOT EXISTS (
            SELECT 1 FROM public.users WHERE id = candidate_subject AND account_state = 'active'
        ) OR EXISTS (
            SELECT 1 FROM public.account_deletion_requests WHERE subject_id = candidate_subject
        ) THEN
            RAISE EXCEPTION 'account subject is not active'
                USING ERRCODE = '23514', CONSTRAINT = 'active_account_reference';
        END IF;
    END LOOP;
    RETURN NEW;
END
$guard$, '[[:space:]]+', ' ', 'g'))
			)
			AND (
				SELECT count(*)
				FROM (VALUES
					('tickets_require_active_owner', 'tickets', 23, ARRAY['owner_id']::text[], ARRAY['owner_id']::text[]),
					('competitors_require_active_subject', 'competitors', 23, ARRAY['user_id','withdrawn_at','withdrawn_by']::text[], ARRAY['user_id','withdrawn_by']::text[]),
					('media_require_active_uploader', 'media', 23, ARRAY['uploaded_by']::text[], ARRAY['uploaded_by']::text[]),
					('media_upload_staging_require_active_subject', 'media_upload_staging', 23, ARRAY['subject_id']::text[], ARRAY['subject_id']::text[]),
					('certificates_require_active_owner', 'certificates', 23, ARRAY['owner_id']::text[], ARRAY['owner_id']::text[]),
					('urls_require_active_creator', 'urls', 23, ARRAY['created_by']::text[], ARRAY['created_by']::text[]),
					('url_hits_require_active_subject', 'url_hits', 23, ARRAY['user_id']::text[], ARRAY['user_id']::text[]),
					('event_door_staff_require_active_subject', 'event_door_staff', 23, ARRAY['user_id']::text[], ARRAY['user_id']::text[]),
					('events_require_active_archiver', 'events', 23, ARRAY['archived_by']::text[], ARRAY['archived_by']::text[]),
					('event_days_require_active_archiver', 'event_days', 23, ARRAY['archived_by']::text[], ARRAY['archived_by']::text[]),
					('sessions_require_active_archiver', 'sessions', 23, ARRAY['archived_by']::text[], ARRAY['archived_by']::text[]),
					('seasons_require_active_archiver', 'seasons', 23, ARRAY['archived_by']::text[], ARRAY['archived_by']::text[]),
					('media_require_active_deleter', 'media', 23, ARRAY['deleted_by']::text[], ARRAY['deleted_by']::text[]),
					('urls_require_active_disabler', 'urls', 23, ARRAY['disabled_by']::text[], ARRAY['disabled_by']::text[]),
					('certificate_templates_require_active_creator', 'certificate_templates', 23, ARRAY['created_by']::text[], ARRAY['created_by']::text[]),
					('certificate_template_versions_require_active_publisher', 'certificate_template_versions', 23, ARRAY['published_by']::text[], ARRAY['published_by']::text[]),
					('certificate_template_bindings_require_active_updater', 'certificate_template_bindings', 23, ARRAY['updated_by']::text[], ARRAY['updated_by']::text[]),
					('certificate_event_state_require_active_finalizer', 'certificate_event_state', 23, ARRAY['attendance_finalized_by']::text[], ARRAY['attendance_finalized_by']::text[]),
					('certificate_batches_require_active_requester', 'certificate_batches', 23, ARRAY['requested_by']::text[], ARRAY['requested_by']::text[]),
					('account_deletion_requests_require_active_subject', 'account_deletion_requests', 7, ARRAY[]::text[], ARRAY['subject_id','requested_by']::text[]),
					('account_deletion_requests_require_active_requester', 'account_deletion_requests', 19, ARRAY['requested_by']::text[], ARRAY['requested_by']::text[])
				) expected(trigger_name, table_name, trigger_type, update_columns, argument_fragments)
				JOIN pg_trigger actual
				  ON actual.tgrelid=to_regclass('public.' || expected.table_name)
				 AND actual.tgname=expected.trigger_name
				 AND actual.tgtype=expected.trigger_type
				 AND actual.tgenabled='O'
				 AND actual.tgqual IS NULL
				 AND NOT actual.tgisinternal
				JOIN pg_proc trigger_function
				  ON trigger_function.oid=actual.tgfoid
				 AND trigger_function.oid=to_regprocedure('public.require_active_account_reference()')
				WHERE actual.tgattr::text = COALESCE((
					SELECT string_agg(attribute.attnum::text, ' ' ORDER BY column_name.ordinal)
					FROM unnest(expected.update_columns) WITH ORDINALITY column_name(name, ordinal)
					JOIN pg_attribute attribute
					  ON attribute.attrelid=actual.tgrelid
					 AND attribute.attname=column_name.name
				), '')
				  AND encode(actual.tgargs, 'hex') = COALESCE((
					SELECT string_agg(encode(convert_to(argument.value, 'UTF8'), 'hex') || '00', '' ORDER BY argument.ordinal)
					FROM unnest(expected.argument_fragments) WITH ORDINALITY argument(value, ordinal)
				  ), '')
			) = 21
			AND NOT EXISTS (
				SELECT 1 FROM pg_trigger
				WHERE tgrelid='public.competitors'::regclass
				  AND tgname='competitors_require_active_withdrawer'
				  AND NOT tgisinternal
			)`,
	20260920120000: `
		SELECT 1
		WHERE EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='account_deletion_requests'
			  AND column_name='platform_blocked_at' AND udt_name='timestamptz' AND is_nullable='YES'
		)
		AND EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='account_deletion_outbox'
			  AND column_name='available_at' AND column_default='''infinity''::timestamp with time zone'
		)
		AND EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname='public' AND indexname='account_deletion_requests_claim_idx'
			  AND indexdef LIKE '%(next_attempt_at, created_at)%'
			  AND indexdef LIKE '%platform_blocked_at IS NOT NULL%'
			  AND indexdef LIKE '%status = ANY%pending%processing%'
		)
		AND EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname='public' AND indexname='account_deletion_requests_projection_idx'
			  AND indexdef LIKE '%(created_at, id)%'
			  AND indexdef LIKE '%platform_blocked_at IS NULL%'
		)`,
	20260920130000: `
		SELECT 1
		WHERE to_regclass('public.account_deletion_self_intakes') IS NOT NULL
		AND (
			SELECT count(*)
			FROM (VALUES
				('request_id', 'uuid', 'NO'),
				('idempotency_key_hash', 'bytea', 'NO'),
				('receipt_lookup_hash', 'bytea', 'NO'),
				('receipt_hash', 'bytea', 'NO'),
				('receipt_expires_at', 'timestamptz', 'NO'),
				('receipt_revoked_at', 'timestamptz', 'YES'),
				('created_at', 'timestamptz', 'NO')
			) expected(column_name, udt_name, is_nullable)
			JOIN information_schema.columns actual
			  ON actual.table_schema='public'
			 AND actual.table_name='account_deletion_self_intakes'
			 AND actual.column_name=expected.column_name
			 AND actual.udt_name=expected.udt_name
			 AND actual.is_nullable=expected.is_nullable
		) = 7
		AND (
			SELECT count(*)
			FROM information_schema.columns
			WHERE table_schema='public'
			  AND table_name='account_deletion_self_intakes'
		) = 7
		AND EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema='public'
			  AND table_name='account_deletion_self_intakes'
			  AND column_name='created_at'
			  AND column_default='now()'
		)
		AND (
			SELECT count(*)
			FROM (VALUES
				('account_deletion_self_intakes_pkey', 'p', 'PRIMARY KEY (request_id)'),
				('account_deletion_self_intakes_request_id_fkey', 'f', 'FOREIGN KEY (request_id) REFERENCES account_deletion_requests(id)'),
				('account_deletion_self_intakes_idempotency_key_hash_key', 'u', 'UNIQUE (idempotency_key_hash)'),
				('account_deletion_self_intakes_receipt_lookup_hash_key', 'u', 'UNIQUE (receipt_lookup_hash)'),
				('account_deletion_self_intakes_idempotency_key_hash_check', 'c', 'CHECK ((octet_length(idempotency_key_hash) = 32))'),
				('account_deletion_self_intakes_receipt_lookup_hash_check', 'c', 'CHECK ((octet_length(receipt_lookup_hash) = 32))'),
				('account_deletion_self_intakes_receipt_hash_check', 'c', 'CHECK ((octet_length(receipt_hash) = 32))'),
				('account_deletion_self_intakes_expiry_check', 'c', 'CHECK ((receipt_expires_at > created_at))'),
				('account_deletion_self_intakes_revocation_check', 'c', 'CHECK (((receipt_revoked_at IS NULL) OR (receipt_revoked_at >= created_at)))')
			) expected(name, kind, definition)
			JOIN pg_constraint actual
			  ON actual.conrelid=to_regclass('public.account_deletion_self_intakes')
			 AND actual.conname=expected.name
			 AND actual.contype=expected.kind::"char"
			 AND pg_get_constraintdef(actual.oid)=expected.definition
		) = 9
		AND EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname='public'
			  AND indexname='account_deletion_self_intakes_expiry_idx'
			  AND indexdef LIKE '%(receipt_expires_at, request_id)%'
			  AND indexdef LIKE '%receipt_revoked_at IS NULL%'
		)`,
	20260922100000: `
		SELECT 1
		WHERE (
			SELECT count(*)
			FROM (VALUES ('utm_source'), ('utm_medium'), ('utm_campaign'), ('utm_term'), ('utm_content')) AS expected(column_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = 'url_hits'
			 AND actual.column_name = expected.column_name
		) = 5`,
	20260925100000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'ytu_linked'`,
	20260925120000: `
		SELECT 1
		WHERE EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name='account_deletion_steps'
			  AND column_name='counts' AND data_type='jsonb'
		)
		AND EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid=to_regclass('public.account_deletion_steps')
			  AND conname='account_deletion_steps_counts_check'
			  AND pg_get_constraintdef(oid) LIKE '%jsonb_typeof(counts)%object%'
		)
		AND (
			SELECT count(*) FROM pg_constraint, unnest(ARRAY['erase_skymail','erase_cms','erase_forms']) AS step(name)
			WHERE conrelid=to_regclass('public.account_deletion_steps')
			  AND conname='account_deletion_steps_step_check'
			  AND pg_get_constraintdef(oid) LIKE '%''' || step.name || '''%'
		) = 3`,
	20260925180000: `
		SELECT 1
		WHERE (
			SELECT count(*)
			FROM (VALUES
				('media', 'serving_policy_applied'),
				('certificate_template_versions', 'asset_serving_policy_applied')
			) AS expected(table_name, column_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = expected.table_name
			 AND actual.column_name = expected.column_name
		) = 2
		AND to_regclass('public.media_serving_policy_pending_idx') IS NOT NULL
		AND to_regclass('public.certificate_template_versions_asset_serving_pending_idx') IS NOT NULL`,
	20260926090000: `
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'media' AND column_name = 'purpose'
		  AND data_type = 'text' AND is_nullable = 'NO' AND column_default = '''legacy''::text'`,
	20260926113000: `
		SELECT 1
		WHERE to_regclass('public.url_retired_aliases') IS NOT NULL
		  AND to_regclass('public.urls_alias_lower_idx') IS NOT NULL`,
	20260926114000: `
		SELECT 1
		WHERE (
			SELECT count(*)
			FROM (VALUES ('form_id'), ('event_id'), ('label')) AS expected(column_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = 'urls'
			 AND actual.column_name = expected.column_name
		) = 3
		AND to_regclass('public.urls_current_form_idx') IS NOT NULL`,
	20260926120000: `
		SELECT 1
		WHERE (
			SELECT count(*) FROM (VALUES
				('id', 'uuid', 'NO'),
				('media_id', 'uuid', 'NO'),
				('owner_service', 'text', 'NO'),
				('owner_type', 'text', 'NO'),
				('role', 'text', 'NO'),
				('created_at', 'timestamptz', 'NO')
			) expected(column_name, udt_name, is_nullable)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = 'media_attachments'
			 AND actual.column_name = expected.column_name
			 AND actual.udt_name = expected.udt_name
			 AND actual.is_nullable = expected.is_nullable
		) = 6
		-- uuid as created here; text once 20260926121000 has run.
		AND EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'media_attachments' AND column_name = 'owner_id'
			  AND udt_name IN ('uuid', 'text') AND is_nullable = 'NO'
		)
		AND EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'media' AND column_name = 'status'
			  AND data_type = 'text' AND is_nullable = 'NO' AND column_default = '''pending''::text'
		)
		AND EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'media' AND column_name = 'expires_at'
			  AND udt_name = 'timestamptz' AND is_nullable = 'YES'
		)
		AND NOT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'media' AND column_name = 'attached'
		)
		AND (
			SELECT count(*) FROM (VALUES
				('media', 'media_status_check', 'c'),
				('media_attachments', 'media_attachments_pkey', 'p'),
				('media_attachments', 'media_attachments_link_key', 'u'),
				('media_attachments', 'media_attachments_media_id_fkey', 'f')
			) expected(table_name, constraint_name, constraint_type)
			JOIN pg_constraint actual
			  ON actual.conrelid = to_regclass('public.' || expected.table_name)
			 AND actual.conname = expected.constraint_name
			 AND actual.contype = expected.constraint_type::"char"
		) = 4
		AND to_regclass('public.media_attachments_owner_idx') IS NOT NULL
		AND EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = 'public' AND indexname = 'media_expiry_idx'
			  AND indexdef LIKE '%(expires_at, id)%'
			  AND indexdef LIKE '%expires_at IS NOT NULL%'
		)
		AND (
			SELECT count(*) FROM (VALUES
				('media_attachments', 'media_attachments_require_current_media', 'require_current_attached_media'),
				('media_attachments', 'media_attachments_status_insert', 'media_attachment_status'),
				('media_attachments', 'media_attachments_status_update', 'media_attachment_status'),
				('media_attachments', 'media_attachments_status_delete', 'media_attachment_status'),
				('events', 'events_media_attachments_insert', 'sync_core_media_attachments'),
				('events', 'events_media_attachments_update', 'sync_core_media_attachments'),
				('events', 'events_media_attachments_delete', 'sync_core_media_attachments'),
				('event_images', 'event_images_media_attachments_insert', 'sync_core_media_attachments'),
				('event_images', 'event_images_media_attachments_update', 'sync_core_media_attachments'),
				('event_images', 'event_images_media_attachments_delete', 'sync_core_media_attachments'),
				('users', 'users_media_attachments_insert', 'sync_core_media_attachments'),
				('users', 'users_media_attachments_update', 'sync_core_media_attachments'),
				('users', 'users_media_attachments_delete', 'sync_core_media_attachments'),
				('certificate_templates', 'certificate_templates_media_attachments_insert', 'sync_core_media_attachments'),
				('certificate_templates', 'certificate_templates_media_attachments_update', 'sync_core_media_attachments'),
				('certificate_templates', 'certificate_templates_media_attachments_delete', 'sync_core_media_attachments'),
				('certificate_template_versions', 'certificate_template_versions_media_attachments_insert', 'sync_core_media_attachments'),
				('certificate_template_versions', 'certificate_template_versions_media_attachments_update', 'sync_core_media_attachments'),
				('certificate_template_versions', 'certificate_template_versions_media_attachments_delete', 'sync_core_media_attachments')
			) expected(table_name, trigger_name, function_name)
			JOIN pg_trigger actual
			  ON actual.tgrelid = to_regclass('public.' || expected.table_name)
			 AND actual.tgname = expected.trigger_name
			 AND actual.tgenabled = 'O'
			 AND NOT actual.tgisinternal
			JOIN pg_proc trigger_function
			  ON trigger_function.oid = actual.tgfoid
			 AND trigger_function.proname = expected.function_name
		) = 19
		AND to_regprocedure('public.core_media_links(jsonb, text, text, text)') IS NOT NULL
		AND to_regprocedure('public.certificate_layout_media_ids(jsonb, jsonb)') IS NOT NULL`,
	// The owner id is text, and core's own links compare and write their
	// owners' UUIDs as text. A rerun of 20260926120000 puts back the UUID
	// comparison; this fingerprint then fails and the migration runs again.
	20260926121000: `
		SELECT 1
		WHERE EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'media_attachments' AND column_name = 'owner_id'
			  AND udt_name = 'text' AND is_nullable = 'NO'
		)
		AND EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass('public.media_attachments')
			  AND conname = 'media_attachments_owner_id_check' AND contype = 'c'
		)
		AND EXISTS (
			SELECT 1 FROM pg_proc
			WHERE proname = 'sync_core_media_attachments'
			  AND prosrc LIKE '%a.owner_id = gone.owner_id::TEXT%'
			  AND prosrc LIKE '%added.owner_id::TEXT%'
		)`,
	20260926130000: `
		SELECT 1
		WHERE (
			SELECT count(*) FROM (VALUES
				('width', 'int4'),
				('height', 'int4'),
				('size_objects', 'jsonb')
			) expected(column_name, udt_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = 'media'
			 AND actual.column_name = expected.column_name
			 AND actual.udt_name = expected.udt_name
			 AND actual.is_nullable = 'YES'
		) = 3
		AND (
			SELECT count(*) FROM pg_constraint
			WHERE conrelid = to_regclass('public.media') AND contype = 'c'
			  AND conname IN ('media_width_check', 'media_height_check', 'media_size_objects_check')
		) = 3
		AND EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = 'public' AND indexname = 'media_size_objects_pending_idx'
			  AND indexdef LIKE '%size_objects IS NULL%'
		)`,
	// The legacy backfill's hold and the purpose check on new Media
	// attachments. A rerun of 20260926120000 puts back its status and
	// current-media functions; this fingerprint then fails and the migration
	// runs again.
	20260926161000: `
		SELECT 1
		WHERE EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'media' AND column_name = 'detach_expiry_held'
			  AND data_type = 'boolean' AND is_nullable = 'NO' AND column_default = 'false'
		)
		AND to_regprocedure('public.media_purpose_fits_role(text, text, text)') IS NOT NULL
		AND to_regprocedure('public.media_role_purposes()') IS NOT NULL
		AND EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'media_legacy_hold' AND column_name = 'released_at'
			  AND udt_name = 'timestamptz' AND is_nullable = 'YES'
		)
		-- Its one state row: the backfill refuses to decide about the hold
		-- without it. The table is read through dynamic SQL, and only once it
		-- exists, so that the check does not fail before the migration.
		AND CASE WHEN to_regclass('public.media_legacy_hold') IS NULL THEN false
			ELSE (xpath('/row/n/text()', query_to_xml('SELECT count(*) AS n FROM public.media_legacy_hold', false, true, '')))[1]::text = '1'
		END
		AND EXISTS (
			SELECT 1 FROM pg_proc
			WHERE proname = 'media_attachment_status' AND prosrc LIKE '%OR detach_expiry_held THEN NULL%'
		)
		AND EXISTS (
			SELECT 1 FROM pg_proc
			WHERE proname = 'require_current_attached_media'
			  AND prosrc LIKE '%FOR KEY SHARE%' AND prosrc LIKE '%media_purpose_fits_role(NEW.owner_service, NEW.role, current_purpose)%'
			  AND prosrc LIKE '%media_attachment_purpose_fits%'
		)`,
}

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("schema_migrations: %w", err)
	}

	files, err := listUp()
	if err != nil {
		return err
	}
	applied, err := loadApplied(ctx, pool)
	if err != nil {
		return err
	}

	for _, f := range files {
		if applied[f.version] {
			continue
		}
		present, err := alreadyPresent(ctx, pool, f.version)
		if err != nil {
			return fmt.Errorf("fingerprint %d: %w", f.version, err)
		}
		if present {
			if err := record(ctx, pool, f.version); err != nil {
				return err
			}
			log.Printf("migrate: recorded existing %d", f.version)
			continue
		}
		body, err := fs.ReadFile(db.UpSQL, path.Join("migrations", f.name))
		if err != nil {
			return err
		}
		if err := execSQL(ctx, pool, string(body)); err != nil {
			return fmt.Errorf("migration %d: %w", f.version, err)
		}
		if _, hasFingerprint := fingerprints[f.version]; hasFingerprint {
			present, err := alreadyPresent(ctx, pool, f.version)
			if err != nil {
				return fmt.Errorf("migration %d postcondition: %w", f.version, err)
			}
			if !present {
				return fmt.Errorf("migration %d postcondition: schema fingerprint is incomplete", f.version)
			}
		}
		if err := record(ctx, pool, f.version); err != nil {
			return err
		}
		log.Printf("migrate: applied %d", f.version)
	}
	return nil
}

func Versions() ([]int64, error) {
	files, err := listUp()
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(files))
	for i, f := range files {
		out[i] = f.version
	}
	return out, nil
}

func ExtraFormSQL() (string, error) {
	body, err := fs.ReadFile(db.UpSQL, "migrations/20260918180000_event_extra_form_urls.up.sql")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func listUp() ([]file, error) {
	entries, err := fs.ReadDir(db.UpSQL, "migrations")
	if err != nil {
		return nil, err
	}
	out := make([]file, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSuffix(name, path.Ext(name))[:14], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration name %s: %w", name, err)
		}
		out = append(out, file{version: v, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func loadApplied(ctx context.Context, pool *pgxpool.Pool) (map[int64]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func alreadyPresent(ctx context.Context, pool *pgxpool.Pool, version int64) (bool, error) {
	q, ok := fingerprints[version]
	if !ok {
		return false, nil
	}
	var n int
	err := pool.QueryRow(ctx, q).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func record(ctx context.Context, pool *pgxpool.Pool, version int64) error {
	_, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, version)
	return err
}

func execSQL(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return nil
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	_, err = conn.Conn().PgConn().Exec(ctx, sql).ReadAll()
	return err
}
