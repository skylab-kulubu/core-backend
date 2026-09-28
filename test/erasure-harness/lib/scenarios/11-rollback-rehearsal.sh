#!/usr/bin/env bash
# Scenario 11 — rollback rehearsal (core docs/account-lifecycle.md, "Rollback boundary"; spec
# §8): after deletion traffic the down migrations of 20260925120000 and 20260920010000 refuse,
# on a copy of core's database. Then the service databases are restored from the backup taken
# before scenario 1 and the §8 replay rule (the command again with "emails": [] for every
# completed request) erases the subject-keyed data again.
#
# CMS and Forms are restored while stopped. SkyMail follows its own restore procedure
# (skymail-backend docs/data-lifecycle.md, "Backup and restore"; account-erasure ticket 16):
#   1. MAIL_SENDER=paused and a redeploy; GET /v1/mail_tasks/summary answers sender_paused true;
#   2. pg_restore, with the restore instant written down just before it;
#   3. a deploy on the restored database, still paused;
#   4. queue-close-restored --before <restore instant>: a dry run, then --apply;
#   5. the §8 replay, 200 for every request;
#   6. MAIL_SENDER removed and a redeploy.
# No mail of the restored queue may reach mailpit at any point: the restored rows are made due at
# once, so a running sender would send them. One row queued after the restore is the control: it
# must be left open by the close and go out once the sender is back.
#
# The e-mail-keyed rows of the people erased after the dump (recipients, list memberships, the
# closed rows to them) stay after an "emails": [] replay. That is the open part of ticket 17,
# which core's replay from the matching backups closes (ticket 18, ADR-0053): printed as
# KNOWN GAP with counts, never as a failure.

RESTORE_NOT_SENT='restore: gönderilmedi'
S11_CONTROL_TO=after-restore@harness.invalid

# The migrations of the commit the running core image was built from (build.sh keeps its tree).
core_migrations_dir() {
  local dir=$STATE/build/core-${HARNESS_IMAGE_CORE##*:}/db/migrations
  [[ -d $dir ]] && printf '%s' "$dir"
}

# down_refused FILE: runs one down migration on the rehearsal copy in one transaction; prints
# the error, succeeds only when the migration refused.
down_refused() {
  local out
  if out=$(PGPASSWORD=$POSTGRES_PASSWORD psql -h postgres -U postgres -d core_rehearsal -X -q -v ON_ERROR_STOP=1 \
    --single-transaction -f "$1" 2>&1); then
    log "        $(basename "$1") applied without refusing"
    return 1
  fi
  log "        $(basename "$1"): $(grep -m1 -E 'ERROR|FATAL' <<<"$out" | cut -c1-220)"
  grep -q ERROR <<<"$out"
}

erasure_token() { # erasure_token SCOPE_SERVICE: core-erasure's client-credentials token
  hcurl --fail --data-urlencode grant_type=client_credentials --data-urlencode client_id=core-erasure \
    --data-urlencode "client_secret=$(kc_client_secret core-erasure)" \
    --data-urlencode "scope=openid account-erase-$1" "$REALM_URL/protocol/openid-connect/token" | jq -r .access_token
}

replay_command() { # replay_command SERVICE BASE_URL REQUEST SUBJECT
  local token
  token=$(erasure_token "$1")
  http PUT "$2/internal/v1/account-erasures/$3" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    --data "$(jq -cn --arg r "$3" --arg s "$4" '{request_id: $r, subject_id: $s, emails: []}')"
}

# --- SkyMail's restore procedure ------------------------------------------------------------------
# skymail_redeploy paused|on: recreates SkyMail with MAIL_SENDER=paused, or with no MAIL_SENDER at
# all, as a Dokploy redeploy with the changed environment does; waits for /ready.
skymail_redeploy() {
  save_logs skymail
  if [[ $1 == paused ]]; then
    MAIL_SENDER=paused dc up -d --no-deps skymail >/dev/null 2>&1
  else
    (unset MAIL_SENDER; dc up -d --no-deps skymail >/dev/null 2>&1)
  fi
  wait_until 120 3 service_ready http://skymail:3000/ready
}
# skymail_env_sender: MAIL_SENDER as the running container has it, "unset" when it has none.
skymail_env_sender() {
  local value
  value=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$(dc ps -q skymail)" | sed -n 's/^MAIL_SENDER=//p')
  printf '%s' "${value:-unset}"
}
skymail_logged() { docker logs "$(dc ps -q skymail)" 2>&1 | grep -qF "$1"; } # skymail_logged TEXT
# skymail_sender_paused: sender_paused of GET /v1/mail_tasks/summary, read as the bystander p8.
skymail_sender_paused() {
  http GET "http://skymail:3000/v1/mail_tasks/summary" -H "Authorization: Bearer $(user_token p8 skymail)"
  if [[ $HTTP_STATUS == 200 ]]; then jq -r '.sender_paused' <<<"$HTTP_BODY"; else printf 'status %s' "$HTTP_STATUS"; fi
}
# queue_close_restored ARGS...: the subcommand in SkyMail's own container, as `docker exec` runs it
# in production. Sets QCR_OUT (counts and times only) and QCR_RC.
QCR_OUT='' QCR_RC=''
queue_close_restored() {
  QCR_OUT=$(dc exec -T skymail /app/skymail-backend queue-close-restored "$@" 2>&1)
  QCR_RC=$?
  printf '$ queue-close-restored %s\n%s\nexit %s\n\n' "$*" "$QCR_OUT" "$QCR_RC" >>"$EVIDENCE/s11-queue-close-restored.txt"
}
qcr_line() { awk -F': ' -v key="$1" '$1 == key { print $2; exit }' <<<"$QCR_OUT"; }

# mailpit_to FILE: how many messages in mailpit went to an address listed in FILE (one per line,
# lower case). "error" when mailpit cannot be read.
mailpit_to() {
  local listing
  listing=$(curl --silent --fail "http://mailpit:8025/api/v1/messages?limit=1000") || { echo error; return 0; }
  jq -r '.messages[].To[]?.Address | ascii_downcase' <<<"$listing" | grep -cxFf "$1" || true
}
mailpit_has() { local n; n=$(mailpit_to "$1"); [[ $n =~ ^[0-9]+$ ]] && ((n > 0)); } # mailpit_has FILE

# queue_row_hash ID [EXCLUDED_COLUMN...]: md5 of the whole queue row, or of all its columns but these.
queue_row_hash() {
  local id=$1 expr="to_jsonb(q)" column
  shift
  for column in "$@"; do expr="$expr - '$column'"; done
  pg skymail "SELECT md5(($expr)::text) FROM mail_queue q WHERE id = '$id'"
}

# erased_addresses: the school and personal addresses of every person whose request is completed,
# lower case, one per line.
erased_addresses() {
  local p
  for p in "${PERSONS[@]}"; do
    [[ $(pg super_skylab "SELECT status FROM account_deletion_requests WHERE subject_id = '${P_ID[$p]}'") == completed ]] || continue
    printf '%s\n%s\n' "${P_SCHOOL[$p],,}" "${P_PERSONAL[$p],,}"
  done
}

# known_gap_email_keyed: counts, never addresses, of what the restore brought back for the people
# erased after the dump and an "emails": [] replay cannot reach.
known_gap_email_keyed() {
  local addresses list counts persons db rows='' recipients memberships queue closed approval
  addresses=$(mktemp)
  erased_addresses >"$addresses"
  persons=$(($(grep -c . "$addresses") / 2))
  list=$(awk '{ printf "%s'\''%s'\''", (NR > 1 ? "," : ""), $0 }' "$addresses")
  counts=$(pg skymail "
    WITH erased(email) AS (SELECT unnest(ARRAY[$list]::text[]))
    SELECT (SELECT count(*) FROM recipients WHERE lower(email) IN (SELECT email FROM erased)),
           (SELECT count(*) FROM mailing_list_recipients m JOIN recipients r ON r.id = m.recipient_id
             WHERE lower(r.email) IN (SELECT email FROM erased)),
           (SELECT count(*) FROM mail_queue WHERE lower(recipient_email) IN (SELECT email FROM erased)),
           (SELECT count(*) FROM mail_queue WHERE lower(recipient_email) IN (SELECT email FROM erased)
               AND error = '$RESTORE_NOT_SENT'),
           (SELECT count(*) FROM mail_approval_recipients WHERE lower(email) IN (SELECT email FROM erased))")
  for db in skymail skylab_cms forms_db; do
    rows+="${rows:+, }$db $(db_lines "$db" | grep -ciFf "$addresses" || true)"
  done
  rm -f "$addresses"
  IFS=$'\t' read -r recipients memberships queue closed approval <<<"$counts"
  known_gap 18 "after the emails [] replay the restored databases still hold e-mail-keyed data of the $persons people erased after the dump: SkyMail recipients $recipients, list memberships $memberships, queue rows to them $queue (closed by the restore $closed), Mail onayı recipients $approval; rows holding their addresses: $rows"
}

scenario_11() {
  local migrations db request subject service url replayed=0 first_not_200=0 bodies_cleared=0 p
  scenario_begin S11 'rollback rehearsal: down migrations refuse after deletions; backup restore with SkyMail paused, its restored queue closed unsent, replay with emails [] answers 200 and erases subject-keyed data again; no restored mail sent'
  migrations=$(core_migrations_dir)
  check 'core migrations of the built commit are at hand' test -n "$migrations"

  pg postgres "DROP DATABASE IF EXISTS core_rehearsal" >/dev/null
  pg postgres "CREATE DATABASE core_rehearsal" >/dev/null
  PGPASSWORD=$POSTGRES_PASSWORD pg_dump -h postgres -U postgres -d super_skylab -Fc \
    | PGPASSWORD=$POSTGRES_PASSWORD pg_restore -h postgres -U postgres -d core_rehearsal --no-owner 2>/dev/null || true
  check 'rehearsal copy of core holds the completed requests' \
    test "$(pg core_rehearsal "SELECT count(*) FROM account_deletion_requests WHERE status = 'completed'")" -gt 0
  check 'down of 20260925120000 (service steps) refuses while step rows exist' \
    down_refused "$migrations/20260925120000_account_erasure_service_steps.down.sql"
  check 'down of 20260920010000 (account lifecycle) refuses after deletion traffic' \
    down_refused "$migrations/20260920010000_account_lifecycle.down.sql"
  check 'the refused downs changed nothing' \
    eq "$(pg core_rehearsal "SELECT count(*) FROM account_deletion_requests")" "$(pg super_skylab "SELECT count(*) FROM account_deletion_requests")"
  pg postgres "DROP DATABASE core_rehearsal" >/dev/null

  check 'the pre-deletion backup exists' test -f "$STATE/backups/pre-s1/skymail.dump"

  # SkyMail step 1: pause, redeploy, see it paused, before the dump lands.
  check 'SkyMail redeployed with MAIL_SENDER=paused' skymail_redeploy paused
  check 'the running SkyMail container has MAIL_SENDER=paused' eq "$(skymail_env_sender)" paused
  check 'SkyMail logged at startup that its sender is paused' \
    wait_until 20 2 skymail_logged 'Mail sender paused by MAIL_SENDER=paused'
  check 'GET /v1/mail_tasks/summary answers sender_paused true before the restore' eq "$(skymail_sender_paused)" true

  # Step 2: the restore. SkyMail keeps running, paused (its API stays up); CMS and Forms stop.
  local restored_at
  dc stop cms forms >/dev/null 2>&1
  sleep 1
  restored_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  log "  restore instant $restored_at"
  for db in skymail skylab_cms forms_db; do
    PGPASSWORD=$POSTGRES_PASSWORD pg_restore -h postgres -U postgres -d "$db" --clean --if-exists --no-owner \
      --role="$(case $db in skymail) echo skymail ;; skylab_cms) echo skylab_cms ;; forms_db) echo forms ;; esac)" \
      "$STATE/backups/pre-s1/$db.dump" 2>"$EVIDENCE/s11-restore-$db.log" || true
  done
  check 'the restore brought p1 back into SkyMail (its data predates the erasure)' test -n "$(pii_hits skymail p1)"
  check 'the restored databases hold no receipt of any later request' \
    eq "$(pg skymail "SELECT count(*) FROM account_erasure_receipts")/$(pg skylab_cms "SELECT count(*) FROM account_erasure_receipts")/$(pg forms_db "SELECT count(*) FROM account_erasure_receipts")" 0/0/0

  # Step 3: the deploy on the restored database, still paused (the environment did not change).
  dc restart skymail >/dev/null 2>&1
  check 'SkyMail is back on the restored database' wait_until 120 3 service_ready http://skymail:3000/ready
  check 'still sender_paused true after the deploy' eq "$(skymail_sender_paused)" true
  dc start cms forms >/dev/null 2>&1
  wait_until 120 3 service_ready http://cms:5000/health/ready
  wait_until 60 2 service_ready http://forms:8080/api/health

  # The backup brought back the queue rows that were pending when it was taken, to erased people
  # among them, and another person's row (the bystander p8's). They are made due now: a running
  # sender would send them within its 10-second tick.
  local restored_pending restored_ids restored_list addresses baseline bystander_row bystander_content
  restored_pending=$(pg skymail "SELECT count(*) FROM mail_queue WHERE status IN ('pending', 'processing')")
  restored_ids=$(pg skymail "SELECT id FROM mail_queue WHERE status IN ('pending', 'processing') ORDER BY id")
  restored_list=$(awk '{ printf "%s'\''%s'\''", (NR > 1 ? "," : ""), $0 }' <<<"$restored_ids")
  note "the restore brought back $restored_pending pending SkyMail queue row(s)"
  check 'the restore brought back pending queue rows' ge "$restored_pending" 1
  [[ -n $restored_list ]] || restored_list="'00000000-0000-0000-0000-000000000000'"
  addresses=$(mktemp)
  pg skymail "SELECT DISTINCT lower(recipient_email) FROM mail_queue WHERE id IN ($restored_list)" >"$addresses"
  baseline=$(mailpit_to "$addresses")
  pg skymail "UPDATE mail_queue SET next_attempt_at = now() WHERE id IN ($restored_list)" >/dev/null
  bystander_row=$(pg skymail "SELECT id FROM mail_queue WHERE id IN ($restored_list) AND lower(recipient_email) = lower('${P_PERSONAL[p8]}')")
  check "one restored row is the bystander p8's own (a third person's mail)" test -n "$bystander_row"
  bystander_content=$(queue_row_hash "$bystander_row" status error)
  log "  paused with the restored rows due; waiting past two dispatcher ticks"
  sleep 25
  check 'paused, the due restored rows stay pending' \
    eq "$(pg skymail "SELECT count(*) FROM mail_queue WHERE id IN ($restored_list) AND status = 'pending'")" "$restored_pending"
  check 'paused, no mail of the restored queue reached mailpit' eq "$(mailpit_to "$addresses")" "$baseline"

  # Mail queued after the restore: the control. The close must leave it, the unpaused sender send it.
  local control_task control_row control_to
  control_to=$(mktemp)
  printf '%s\n' "$S11_CONTROL_TO" >"$control_to"
  control_task=$(uuid_of "s11-control-task-$restored_at") control_row=$(uuid_of "s11-control-row-$restored_at")
  pg skymail "
    INSERT INTO mail_tasks (id, sent_by, template_id, body_variables)
    VALUES ('$control_task', '${P_ID[p8]}', '$(uuid_of p8-template)', '{}'::jsonb);
    INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, attempts, next_attempt_at)
    VALUES ('$control_row', '$control_task', 'Harness kontrol', '$S11_CONTROL_TO', 'Geri yüklemeden sonra',
            'Geri yüklemeden sonra kuyruğa girdi.', 'pending', 0, now());" >/dev/null
  check 'a control mail is queued after the restore instant' \
    eq "$(pg skymail "SELECT created_at > '$restored_at'::timestamptz FROM mail_queue WHERE id = '$control_row'")" t

  # Step 4: close the restored queue, a dry run first.
  queue_close_restored --before "$restored_at"
  check "queue-close-restored dry run exits 0 ($QCR_RC)" eq "$QCR_RC" 0
  check 'the dry run says it changed nothing' grep -q '^queue-close-restored: dry run, nothing changed' <<<"$QCR_OUT"
  check 'the dry run counts the restored rows as pending, none processing after the deploy' \
    eq "$(qcr_line pending)/$(qcr_line processing)" "$restored_pending/0"
  check 'the dry run leaves the mail queued since the restore alone' eq "$(qcr_line 'queued at or after --before, left alone')" 1
  check 'the dry run changed no row' \
    eq "$(pg skymail "SELECT count(*) FROM mail_queue WHERE status = 'pending'")" "$((restored_pending + 1))"
  queue_close_restored --before "$restored_at" --apply
  check "queue-close-restored --apply exits 0 ($QCR_RC)" eq "$QCR_RC" 0
  check '--apply says it closed without sending' grep -qF "closed without sending, error \"$RESTORE_NOT_SENT\"" <<<"$QCR_OUT"
  check '--apply closed every restored row' eq "$(qcr_line pending)/$(qcr_line processing)" "$restored_pending/0"
  check "every restored row is failed with error '$RESTORE_NOT_SENT', attempts as they were (0)" \
    eq "$(pg skymail "SELECT count(*) FROM mail_queue WHERE id IN ($restored_list) AND status = 'failed' AND error = '$RESTORE_NOT_SENT' AND attempts = 0")" "$restored_pending"
  check 'the control mail queued after the restore is still pending' \
    eq "$(pg skymail "SELECT status FROM mail_queue WHERE id = '$control_row'")" pending
  queue_close_restored --before "$restored_at"
  check 'run again, it finds nothing to close' eq "$QCR_RC/$(qcr_line pending)/$(qcr_line processing)" 0/0/0
  local bystander_closed
  bystander_closed=$(queue_row_hash "$bystander_row")
  check "the bystander's closed row changed only in status and error" \
    eq "$(queue_row_hash "$bystander_row" status error)" "$bystander_content"

  # Step 5: §8, every completed request is sent again with "emails": [] (the addresses are gone).
  # Each must answer 200 at once; a 202 is still repeated after its Retry-After, as core would.
  while IFS=$'\t' read -r request subject; do
    for service in skymail cms forms; do
      case $service in skymail) url=http://skymail:3000 ;; cms) url=http://cms:5000 ;; forms) url=http://forms:8080 ;; esac
      local tries=0
      replay_command "$service" "$url" "$request" "$subject"
      if [[ $HTTP_STATUS != 200 ]]; then
        first_not_200=$((first_not_200 + 1))
        log "        replay $service $request first answered $HTTP_STATUS"
      fi
      while [[ $HTTP_STATUS == 202 && $tries -lt 12 ]]; do
        tries=$((tries + 1))
        sleep 10
        replay_command "$service" "$url" "$request" "$subject"
      done
      if [[ $HTTP_STATUS == 200 ]]; then
        replayed=$((replayed + 1))
        [[ $service != skymail ]] || bodies_cleared=$((bodies_cleared + $(jq -r '.counts.bodies_cleared // 0' <<<"$HTTP_BODY")))
      else
        log "        replay $service $request answered $HTTP_STATUS"
      fi
    done
  done < <(pg super_skylab "SELECT id, subject_id FROM account_deletion_requests WHERE status = 'completed' ORDER BY created_at")
  local completed
  completed=$(pg super_skylab "SELECT count(*) FROM account_deletion_requests WHERE status = 'completed'")
  check "every completed request replayed to the three services ($replayed of $((completed * 3)) answered 200)" eq "$replayed" "$((completed * 3))"
  check 'every replay answered 200 at once, none 202 (the restored queue is closed)' eq "$first_not_200" 0
  note "SkyMail's replays cleared $bodies_cleared mail bod(y/ies) naming an erased person (counts.bodies_cleared)"
  for p in "${PERSONS[@]}"; do
    [[ $(pg super_skylab "SELECT status FROM account_deletion_requests WHERE subject_id = '${P_ID[$p]}'") == completed ]] || continue
    for db in skymail skylab_cms forms_db; do
      check "$db: no row carries the subject of $p after the replay" eq "$(count_lines "$(subject_hits "$db" "${P_ID[$p]}")")" 0
    done
    # The person's restored row named them in its body; closed, it is a failed row, so the replay
    # clears what it says, found by the name the person's actor rows hold.
    check "SkyMail: the closed row that named $p has its subject and body cleared" \
      eq "$(pg skymail "SELECT count(*) FILTER (WHERE subject = '' AND body = '' AND body_html IS NULL) || '/' || count(*)
            FROM mail_queue WHERE id IN ($restored_list) AND lower(recipient_email) IN (lower('${P_SCHOOL[$p]}'), lower('${P_PERSONAL[$p]}'))")" 1/1
  done
  check "the bystander's closed row is byte-identical after the replay" eq "$(queue_row_hash "$bystander_row")" "$bystander_closed"
  check "the bystander's closed row still says what it said" \
    eq "$(pg skymail "SELECT subject <> '' AND body <> '' FROM mail_queue WHERE id = '$bystander_row'")" t

  # Step 6: before unpausing, the only send still going out is the one queued since the restore.
  http GET "http://skymail:3000/v1/mail_tasks?status=sending&_start=0&_end=100" -H "Authorization: Bearer $(user_token p8 skymail)"
  check 'GET /v1/mail_tasks?status=sending lists only the send queued after the restore' \
    eq "$HTTP_STATUS $(jq -r '[.[].id] | join(",")' <<<"$HTTP_BODY" 2>/dev/null)" "200 $control_task"
  check 'SkyMail redeployed without MAIL_SENDER' skymail_redeploy on
  check 'the running SkyMail container has no MAIL_SENDER' eq "$(skymail_env_sender)" unset
  check 'GET /v1/mail_tasks/summary answers sender_paused false' eq "$(skymail_sender_paused)" false
  check 'the control mail queued after the restore reaches mailpit once the sender is back' \
    wait_until 60 2 mailpit_has "$control_to"
  check 'the control row is sent' eq "$(pg skymail "SELECT status FROM mail_queue WHERE id = '$control_row'")" sent
  log "  unpaused; waiting past two more dispatcher ticks"
  sleep 25
  check 'after unpausing, no restored row went out (all still failed, none sent)' \
    eq "$(pg skymail "SELECT count(*) FROM mail_queue WHERE id IN ($restored_list) AND status = 'failed' AND error = '$RESTORE_NOT_SENT'")" "$restored_pending"
  check 'no mail of the restored queue reached mailpit at any point' eq "$(mailpit_to "$addresses")" "$baseline"
  check "the bystander's closed row is byte-identical after unpausing" eq "$(queue_row_hash "$bystander_row")" "$bystander_closed"
  rm -f "$addresses" "$control_to"

  known_gap_email_keyed
  scenario_end
}
