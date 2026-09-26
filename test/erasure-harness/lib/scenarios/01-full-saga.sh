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
  # The whole Account Center path: its own OIDC login, Sudo mode with the password (the sudo
  # token sky-account mints for account-center with aud core), prepare, the typed confirmation.
  check 'Account Center: login, Sudo mode, prepare and the typed confirmation' ac_bff_delete "$person"
  check 'Account Center answered 303 to its status page' eq "$HTTP_STATUS $AC_SUBMIT_LOCATION" "303 $AC_URL/account-deletion"
  check 'core holds exactly one request for p1, with the global block confirmed' \
    eq "$(pg super_skylab "SELECT count(*) FILTER (WHERE platform_blocked_at IS NOT NULL) FROM account_deletion_requests WHERE subject_id = '$subject'")" 1
  printf '%s\t%s\n' "$person" "$DEL_REQUEST" >"$STATE/s1.request"
  check "Account Center's session is gone after the submit" not ac_bff_session_ok
  ac_bff_status
  check "Account Center's status page reads the request through its receipt cookie" grep -Eqx '200 (blocking|pending|processing|completed)' <<<"$HTTP_STATUS $(jq -r .status <<<"$HTTP_BODY")"
  marker=$(marker_of "$subject")
  ttl=$(aa_redis PTTL "$(marker_key "$subject")")
  check 'marker is 1 with no TTL' eq "$marker/$ttl" '1/-1'

  check 'request completes within 120 s' wait_request "$DEL_REQUEST" completed 120
  steps=$(steps_of "$DEL_REQUEST")
  log "  steps: $steps"
  check 'all nine steps are checkpointed' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST'")" 9
  # core stamps every checkpoint of one pass with the pass's start time, so the order is read
  # from the other side: Keycloak's admin events (disable, logout, delete) and the services'
  # receipt times.
  check_saga_order "$subject" "$DEL_REQUEST"
  if [[ $(pg super_skylab "SELECT count(DISTINCT completed_at) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST'") == 1 ]]; then
    note 'core stamps all nine checkpoints of a one-pass request with the same completed_at (the pass start), not the time each step finished'
  fi
  ac_bff_status
  check "Account Center's status page says completed, partial=false" eq "$HTTP_STATUS/$(jq -r '.status + "/" + (.partial|tostring)' <<<"$HTTP_BODY")" 200/completed/false

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

# check_preserved_records PERSON: the records the club keeps are still there, without the
# person (ADR-0051, decision 3 and 5): Certificate name, serial and PDF; Ticket and Check-in;
# SkyMail send rows; the CMS News byline.
check_preserved_records() {
  local p=$1 name tag=${1^^}
  name=$(full_name "$p")
  check 'core: owned Certificate keeps name, serial and PDF, loses owner and address' \
    eq "$(pg super_skylab "SELECT recipient_name, pdf_key, pdf_sha256 = encode(sha256(pdf), 'hex'), owner_id IS NULL, recipient_email
          FROM certificates WHERE serial = 'HX-$tag-OWNED'" | tr '\t' '|')" "$name|certificates/$p-owned.pdf|t|t|"
  check 'core: guest Certificate keeps name, serial and PDF, loses the address' \
    eq "$(pg super_skylab "SELECT recipient_name, pdf_key, recipient_email FROM certificates WHERE serial = 'HX-$tag-GUEST'" | tr '\t' '|')" \
    "$name|certificates/$p-guest.pdf|"
  check 'core: both Tickets and both Check-ins stay; owner detached, guest fields cleared' \
    eq "$(pg super_skylab "SELECT count(*) FILTER (WHERE owner_id IS NULL),
            count(*) FILTER (WHERE guest_first_name = '' AND guest_last_name = '' AND guest_email = '' AND guest_phone_number = ''),
            (SELECT count(*) FROM ticket_checkins WHERE ticket_id IN ('$(uuid_of "$p-ticket-owned")', '$(uuid_of "$p-ticket-guest")'))
          FROM tickets WHERE id IN ('$(uuid_of "$p-ticket-owned")', '$(uuid_of "$p-ticket-guest")')" | tr '\t' '|')" '2|2|2'
  check 'core: the user row is an anonymized tombstone' \
    eq "$(pg super_skylab "SELECT account_state, email, first_name, last_name, school_email, phone FROM users WHERE id = '${P_ID[$p]}'" | tr '\t' '|')" \
    'anonymized|||||'
  check 'SkyMail: sent and failed rows to the person stay for the counts, pending one is gone' \
    eq "$(pg skymail "SELECT string_agg(status::text || ':' || recipient_full_name || ':' || recipient_email, ',' ORDER BY status)
          FROM mail_queue WHERE task_id = '$(uuid_of "$p-task-to")'")" 'sent:Silinmiş kullanıcı:,failed:Silinmiş kullanıcı:'
  check "SkyMail: the person's send to the bystander stays, sender replaced, body naming the person cleared" \
    eq "$(pg skymail "SELECT t.sent_by, q.recipient_email, q.body = '' AND q.body_html IS NULL
          FROM mail_tasks t JOIN mail_queue q ON q.task_id = t.id WHERE t.id = '$(uuid_of "$p-task-by")'" | tr '\t' '|')" \
    "$DELETED_SUBJECT|${P_SCHOOL[p8]}|t"
  check 'SkyMail: the template version stays, its author replaced' \
    eq "$(pg skymail "SELECT author_sub, author_name, html_content <> '' FROM template_versions WHERE id = '$(uuid_of "$p-template-v1")'" | tr '\t' '|')" \
    "$DELETED_SUBJECT|Silinmiş kullanıcı|t"
  check 'CMS: the News item keeps its byline; editor and archiver replaced; UpdatedAt and Version unchanged' \
    eq "$(pg skylab_cms "SELECT \"Data\"->>'author', \"UpdatedBy\", \"Version\", \"UpdatedAt\" < now() - interval '1 hour'
          FROM collection_items WHERE \"Id\" = '$(uuid_of "$p-news")'" | tr '\t' '|')" "$name|$DELETED_SUBJECT|3|t"
  check 'Forms: the response stays, redacted and detached; the form stays with Silinmiş kullanıcı as owner' \
    eq "$(pg forms_db "SELECT r.user_id IS NULL, r.data::text, f.owned_by FROM responses r, forms f
          WHERE r.id = '$(uuid_of "$p-response")' AND f.id = '$(uuid_of "$p-form")'" | tr '\t' '|')" "t|{}|$DELETED_SUBJECT"
}

# admin_event_times SUBJECT: "operation<TAB>epoch ms" of Keycloak's admin events on the user.
admin_event_times() {
  kc GET "/admin-events?resourcePath=users/$1*&max=100"
  jq -r '.[] | select(.resourcePath == "users/'"$1"'" or (.resourcePath | startswith("users/'"$1"'/"))) | "\(.operationType)\t\(.resourcePath)\t\(.time)"' <<<"$HTTP_BODY" 2>/dev/null
}

# check_saga_order SUBJECT REQUEST: disable and logout come before any service erased, and the
# Keycloak user is deleted only after all three services confirmed (ADR-0051).
check_saga_order() {
  local subject=$1 request=$2 events disable logout delete first_receipt last_receipt db t
  events=$(admin_event_times "$subject")
  printf '%s\n' "$events" >"$EVIDENCE/s-order-$subject.tsv"
  logout=$(awk -F'\t' '$1 == "ACTION" && $2 ~ /logout$/ { print $3 }' <<<"$events" | sort -n | head -n1)
  # The disable is the last representation write before the logout (core also writes the user
  # earlier, on JIT).
  disable=$(awk -F'\t' -v before="${logout:-99999999999999}" '$1 == "UPDATE" && $2 == "users/'"$subject"'" && $3 <= before { print $3 }' <<<"$events" | sort -n | tail -n1)
  delete=$(awk -F'\t' '$1 == "DELETE" && $2 == "users/'"$subject"'" { print $3 }' <<<"$events" | sort -n | tail -n1)
  first_receipt='' last_receipt=''
  for db in skymail skylab_cms forms_db; do
    t=$(pg "$db" "SELECT (extract(epoch FROM completed_at) * 1000)::bigint FROM account_erasure_receipts WHERE request_id = '$request'")
    [[ -z $t ]] && continue
    [[ -z $first_receipt || $t -lt $first_receipt ]] && first_receipt=$t
    [[ -z $last_receipt || $t -gt $last_receipt ]] && last_receipt=$t
  done
  log "  order (ms): disable=$disable logout=$logout receipts=$first_receipt..$last_receipt delete=$delete"
  check 'Keycloak: the user was disabled before any service erased' test -n "$disable" -a -n "$first_receipt" -a "${disable:-0}" -le "${first_receipt:-0}"
  check 'Keycloak: the sessions were logged out before any service erased' test -n "$logout" -a "${logout:-0}" -le "${first_receipt:-0}"
  check 'Keycloak: the user was deleted after all three services confirmed' test -n "$delete" -a -n "$last_receipt" -a "${delete:-0}" -ge "${last_receipt:-0}"
}
