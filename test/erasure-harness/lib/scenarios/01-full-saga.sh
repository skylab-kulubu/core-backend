#!/usr/bin/env bash
# Scenario 1 — the full saga (audit gates 2 and 5): Sudo mode, intake, marker, worker and the
# nine steps; then PII queries in every service database, receipts and counts, the preserved
# records and Keycloak's 404.

# backup_databases NAME: custom-format dumps of every service database (scenario 11 restores them).
backup_databases() {
  local dir=$STATE/backups/$1 db
  mkdir -p "$dir"
  for db in super_skylab skymail skylab_cms forms_db; do
    PGPASSWORD=$POSTGRES_PASSWORD pg_dump -h postgres -U postgres -d "$db" -Fc -f "$dir/$db.dump"
  done
}

wait_request() { # wait_request REQUEST STATUS TIMEOUT
  wait_until "$3" 2 request_is "$1" "$2"
}

scenario_1() {
  local person=p1 subject request receipts marker ttl steps hits
  subject=${P_ID[$person]}
  scenario_begin S1 'full saga: Sudo mode, intake, marker, nine steps, PII 0, receipts, preserved records, Keycloak 404'
  backup_databases pre-s1

  check 'p1 exists in Keycloak before the request' eq "$(kc_user_status "$subject")" 200
  check 'p1 is not blocked before the request' eq "$(marker_of "$subject")" ''
  check 'Account Center login, sudo proof and core intake answered 202' start_deletion "$person"
  printf '%s\t%s\t%s\n' "$person" "$DEL_REQUEST" "$DEL_RECEIPT" >"$STATE/s1.request"
  check 'intake answer reports platformBlocked=true' eq "$(jq -r .platformBlocked <<<"$HTTP_BODY")" true
  check 'intake answer carries no subject or request id' eq "$(jq -r 'keys | sort | join(",")' <<<"$HTTP_BODY")" \
    'completedAt,partial,platformBlocked,receipt,receiptExpiresAt,requestedAt,status,updatedAt'
  marker=$(marker_of "$subject")
  ttl=$(aa_redis PTTL "$(marker_key "$subject")")
  check 'marker is 1 with no TTL' eq "$marker/$ttl" '1/-1'

  check 'request completes within 120 s' wait_request "$DEL_REQUEST" completed 120
  steps=$(steps_of "$DEL_REQUEST")
  log "  steps: $steps"
  check 'all nine steps are checkpointed' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST'")" 9
  check 'delete_identity is the last step' eq "$(pg super_skylab "SELECT step FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST' ORDER BY completed_at DESC LIMIT 1")" delete_identity
  check 'anonymize_core ran after the three service steps' \
    eq "$(pg super_skylab "SELECT bool_and(s.completed_at <= a.completed_at) FROM account_deletion_steps s, account_deletion_steps a
          WHERE s.request_id = '$DEL_REQUEST' AND a.request_id = s.request_id AND a.step = 'anonymize_core'
            AND s.step IN ('erase_skymail','erase_cms','erase_forms')")" t
  core_status "$DEL_RECEIPT"
  check 'receipt status says completed, partial=false' eq "$HTTP_STATUS/$(jq -r '.status + "/" + (.partial|tostring)' <<<"$HTTP_BODY")" 200/completed/false

  # Receipts and counts: each service's receipt equals core's checkpoint counts.
  local service db core_counts service_counts
  for service in skymail cms forms; do
    case $service in skymail) db=skymail ;; cms) db=skylab_cms ;; forms) db=forms_db ;; esac
    core_counts=$(pg super_skylab "SELECT counts::jsonb FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST' AND step = 'erase_$service'")
    service_counts=$(pg "$db" "SELECT counts::jsonb FROM account_erasure_receipts WHERE request_id = '$DEL_REQUEST'")
    log "  $service counts: $service_counts"
    check "$service wrote a receipt with counts" test -n "$service_counts"
    check "core's erase_$service checkpoint keeps the same counts" eq "$(jq -cS . <<<"${core_counts:-null}")" "$(jq -cS . <<<"${service_counts:-null}")"
    check "$service receipt holds no subject and no address" \
      eq "$(pg "$db" "SELECT count(*) FROM account_erasure_receipts WHERE request_id = '$DEL_REQUEST' AND (counts::text ILIKE '%$subject%' OR counts::text ILIKE '%@%')")" 0
  done
  check 'SkyMail erased something (counts are not all zero)' \
    test "$(pg skymail "SELECT sum(value::bigint) FROM account_erasure_receipts, jsonb_each_text(counts) WHERE request_id = '$DEL_REQUEST'")" -gt 0
  check 'CMS erased something' \
    test "$(pg skylab_cms "SELECT sum(value::bigint) FROM account_erasure_receipts, jsonb_each_text(counts) WHERE request_id = '$DEL_REQUEST'")" -gt 0
  check 'Forms erased something' \
    test "$(pg forms_db "SELECT sum(value::bigint) FROM account_erasure_receipts, jsonb_each_text(counts) WHERE request_id = '$DEL_REQUEST'")" -gt 0

  # PII: the addresses and the name are gone from every service database. Two records keep the
  # name on purpose (ADR-0051): the Certificate's recipient name and the News byline.
  hits=$(pii_hits super_skylab "$person" '^public\.certificates$')
  check "core: no row names p1 outside certificates ($(count_lines "$hits") hits: $(sort -u <<<"$hits" | paste -sd, -))" test -z "$hits"
  check 'core: the certificates keep only the name, never an address' \
    eq "$(pg super_skylab "SELECT count(*) FROM certificates WHERE lower(recipient_email) IN ('${P_SCHOOL[$person]}','${P_PERSONAL[$person]}')")" 0
  for db in skymail forms_db; do
    hits=$(pii_hits "$db" "$person")
    check "$db: no row names p1 ($(count_lines "$hits") hits: $(sort -u <<<"$hits" | paste -sd, -))" test -z "$hits"
    hits=$(subject_hits "$db" "$subject")
    check "$db: no row carries p1's subject ($(count_lines "$hits") hits)" test -z "$hits"
  done
  hits=$(pii_hits skylab_cms "$person" '^public\.collection_items$')
  check "cms: no row names p1 outside the News items ($(count_lines "$hits") hits)" test -z "$hits"
  hits=$(subject_hits skylab_cms "$subject")
  check "cms: no row carries p1's subject ($(count_lines "$hits") hits)" test -z "$hits"
  check 'cms: no draft of p1 left in Redis' eq "$(cms_redis --scan --pattern "*$subject*" | wc -l | tr -d ' ')" 0

  # Preserved records.
  check_preserved_records "$person"

  # The bystander keeps everything.
  local db8
  for db8 in super_skylab skymail forms_db; do
    check "bystander p8 still named in $db8" test -n "$(pii_hits "$db8" p8)"
  done

  check 'Keycloak user p1 answers 404' eq "$(kc_user_status "$subject")" 404
  check 'marker stays after completion' eq "$(marker_of "$subject")" 1
  scenario_end
}
