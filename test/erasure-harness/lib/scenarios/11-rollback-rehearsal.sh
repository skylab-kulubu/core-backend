#!/usr/bin/env bash
# Scenario 11 — rollback rehearsal (core docs/account-lifecycle.md, "Rollback boundary"; spec
# §8): after deletion traffic the down migrations of 20260925120000 and 20260920010000 refuse,
# on a copy of core's database. Then the service databases are restored from the backup taken
# before scenario 1 while the services are stopped, and the §8 replay rule (the command again
# with "emails": [] for every completed request) erases the subject-keyed data again. The
# e-mail-keyed rows the backup brought back stay: the documented limit.

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

scenario_11() {
  local migrations db request subject service url replayed=0 p
  scenario_begin S11 'rollback rehearsal: down migrations refuse after deletions; backup restore with services stopped; replay with emails [] erases subject-keyed data again'
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

  # Backup restore: the service databases as they were before scenario 1, services stopped.
  check 'the pre-deletion backup exists' test -f "$STATE/backups/pre-s1/skymail.dump"
  dc stop skymail cms forms >/dev/null 2>&1
  for db in skymail skylab_cms forms_db; do
    PGPASSWORD=$POSTGRES_PASSWORD pg_restore -h postgres -U postgres -d "$db" --clean --if-exists --no-owner \
      --role="$(case $db in skymail) echo skymail ;; skylab_cms) echo skylab_cms ;; forms_db) echo forms ;; esac)" \
      "$STATE/backups/pre-s1/$db.dump" 2>"$EVIDENCE/s11-restore-$db.log" || true
  done
  check 'the restore brought p1 back into SkyMail (its data predates the erasure)' test -n "$(pii_hits skymail p1)"
  check 'the restored databases hold no receipt of any later request' \
    eq "$(pg skymail "SELECT count(*) FROM account_erasure_receipts")/$(pg skylab_cms "SELECT count(*) FROM account_erasure_receipts")/$(pg forms_db "SELECT count(*) FROM account_erasure_receipts")" 0/0/0
  dc start skymail cms forms >/dev/null 2>&1
  wait_until 120 3 service_ready http://skymail:3000/ready
  wait_until 120 3 service_ready http://cms:5000/health/ready
  wait_until 60 2 service_ready http://forms:8080/api/health

  # The backup also brought back the queue rows that were pending when it was taken. Once they
  # are due SkyMail sends them (they were pending in the seed with a week's delay: time passes
  # here), and until then SkyMail answers the replay 202, since a mail in flight names the person.
  local restored_pending
  restored_pending=$(pg skymail "SELECT count(*) FROM mail_queue WHERE status = 'pending'")
  note "the restore brought back $restored_pending pending SkyMail queue row(s); they go out once due, to the erased persons' addresses too"
  pg skymail "UPDATE mail_queue SET next_attempt_at = now() WHERE status = 'pending'" >/dev/null

  # §8: every completed request is sent again with "emails": [] (the addresses are gone). A 202
  # is repeated after its Retry-After, as core would.
  while IFS=$'\t' read -r request subject; do
    for service in skymail cms forms; do
      case $service in skymail) url=http://skymail:3000 ;; cms) url=http://cms:5000 ;; forms) url=http://forms:8080 ;; esac
      local tries=0
      replay_command "$service" "$url" "$request" "$subject"
      while [[ $HTTP_STATUS == 202 && $tries -lt 12 ]]; do
        tries=$((tries + 1))
        sleep 10
        replay_command "$service" "$url" "$request" "$subject"
      done
      [[ $HTTP_STATUS == 200 ]] && replayed=$((replayed + 1)) || log "        replay $service $request answered $HTTP_STATUS"
    done
  done < <(pg super_skylab "SELECT id, subject_id FROM account_deletion_requests WHERE status = 'completed' ORDER BY created_at")
  local completed
  completed=$(pg super_skylab "SELECT count(*) FROM account_deletion_requests WHERE status = 'completed'")
  check "every completed request replayed to the three services ($replayed of $((completed * 3)) answered 200)" eq "$replayed" "$((completed * 3))"
  for p in "${PERSONS[@]}"; do
    [[ $(pg super_skylab "SELECT status FROM account_deletion_requests WHERE subject_id = '${P_ID[$p]}'") == completed ]] || continue
    for db in skymail skylab_cms forms_db; do
      check "$db: no row carries the subject of $p after the replay" eq "$(count_lines "$(subject_hits "$db" "${P_ID[$p]}")")" 0
    done
  done
  local left
  left=$(count_lines "$(pii_hits skymail p1)")
  note "after restore + replay, SkyMail still holds $left row(s) naming p1 by address or name: the e-mail-keyed data the backup brought back (spec §8: it goes when the backup ages out)"
  check 'the e-mail-keyed residue is what spec §8 predicts (present, not erased by emails: [])' test "$left" -gt 0
  scenario_end
}
