#!/usr/bin/env bash
# Scenario 11 — rollback rehearsal (core docs/account-lifecycle.md, "Rollback boundary"; spec
# §8): after deletion traffic the down migrations of 20260925120000 and 20260920010000 refuse,
# on a copy of core's database. Then the service databases are restored from the backup taken
# before scenario 1 and erasure is replayed into them for every completed request.
#
# CMS and Forms are restored while stopped. SkyMail follows its own restore procedure
# (skymail-backend docs/data-lifecycle.md, "Backup and restore"; account-erasure ticket 16):
#   1. MAIL_SENDER=paused and a redeploy; GET /v1/mail_tasks/summary answers sender_paused true;
#   2. pg_restore, with the restore instant written down just before it;
#   3. a deploy on the restored database, still paused;
#   4. queue-close-restored --before <restore instant>: a dry run, then --apply;
#   5. the replay, 200 for every request;
#   6. MAIL_SENDER removed and a redeploy.
# No mail of the restored queue may reach mailpit at any point: the restored rows are made due at
# once, so a running sender would send them. One row queued after the restore is the control: it
# must be left open by the close and go out once the sender is back.
#
# The replay (ADR-0053, account-erasure ticket 18). Scenario 1 dumped core, then Keycloak, then
# the services (T_core <= T_keycloak <= T_service). The core and Keycloak dumps are restored into
# snapshot-postgres, a throwaway Postgres on an internal network, and core's own
#   CORE_SNAPSHOT_DATABASE_URL=… KEYCLOAK_SNAPSHOT_DATABASE_URL=… \
#     core-backend replay-from-backup --service <x> --dumped-at <T_x> [--apply]
# runs in core's container with the two DSNs in its environment, never in argv: a dry run, then
# --apply. It reads each person's addresses from the pair and sends the normal Erasure command
# with them, so it erases the e-mail-keyed rows (recipients, list memberships, the closed rows to
# the person, Mail onayı recipients) that the §8 "emails": [] replay cannot find (ticket 17).
#
# A service answers a request it holds a receipt for with that receipt and does nothing again.
# So the "emails": [] replay must not reach a restored service before replay-from-backup does:
# its receipt would make the replay a no-op. SkyMail therefore gets replay-from-backup as its
# step 5 (its procedure says so), and the "emails": [] replay only afterwards, answered from the
# replay's receipts. CMS and Forms get the "emails": [] replay first, as before, and
# replay-from-backup after it answers 200 and changes nothing there: CMS holds no e-mail-keyed
# data, and the Forms stub's guest responses of the erased people stay; the latter is printed
# as KNOWN GAP with counts, never as a failure.

RESTORE_NOT_SENT='restore: gönderilmedi'
S11_CONTROL_TO=after-restore@harness.invalid
S11_BACKUP=pre-s1

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

# erased_list: erased_addresses as a SQL list of literals.
erased_list() { erased_addresses | awk '{ printf "%s'\''%s'\''", (NR > 1 ? "," : ""), $0 }'; }

# skymail_erased_email_keyed: counts, never addresses, of SkyMail's e-mail-keyed rows of the people
# erased after the dump: "recipients/list memberships/queue rows to them/Mail onayı recipients".
skymail_erased_email_keyed() {
  pg skymail "
    WITH erased(email) AS (SELECT unnest(ARRAY[$(erased_list)]::text[]))
    SELECT (SELECT count(*) FROM recipients WHERE lower(email) IN (SELECT email FROM erased)) || '/' ||
           (SELECT count(*) FROM mailing_list_recipients m JOIN recipients r ON r.id = m.recipient_id
             WHERE lower(r.email) IN (SELECT email FROM erased)) || '/' ||
           (SELECT count(*) FROM mail_queue WHERE lower(recipient_email) IN (SELECT email FROM erased)) || '/' ||
           (SELECT count(*) FROM mail_approval_recipients WHERE lower(email) IN (SELECT email FROM erased))"
}
# rows_holding_erased DB: how many rows of DB hold an address of a person erased after the dump.
rows_holding_erased() {
  local addresses n
  addresses=$(mktemp)
  erased_addresses >"$addresses"
  n=$(db_lines "$1" | grep -ciFf "$addresses" || true)
  rm -f "$addresses"
  printf '%s' "${n:-0}"
}
all_positive() { local IFS=/ n; for n in $1; do [[ $n =~ ^[1-9][0-9]*$ ]] || { log "        expected every count > 0, got [$1]"; return 1; }; done; }

# --- core's replay-from-backup (ADR-0053, ticket 18) --------------------------------------------
# dumped_at DB: the instant scenario 1 wrote down just before it dumped DB.
dumped_at() { awk -F'\t' -v db="$1" '$1 == db { print $2 }' "$STATE/backups/$S11_BACKUP/dumped-at.tsv" 2>/dev/null; }
# not_after A B...: every instant is at or after the one before it (same fixed-width format).
not_after() {
  local previous='' t
  for t in "$@"; do
    [[ -n $t ]] || { log "        an instant is missing"; return 1; }
    [[ -z $previous || ! $previous > $t ]] || { log "        $previous is after $t"; return 1; }
    previous=$t
  done
}

SNAPSHOT_HOST=snapshot-postgres SNAPSHOT_READER=replay_reader
SNAPSHOT_CORE_DB=snapshot_core SNAPSHOT_KEYCLOAK_DB=snapshot_keycloak
SNAPSHOT_SUPERUSER_PASSWORD='' SNAPSHOT_READER_PASSWORD='' SNAPSHOT_NET=''
snapshot_ctr() { dc --profile replay ps -q snapshot-postgres 2>/dev/null; }
# snapshot_psql DB: runs the SQL on stdin in snapshot-postgres over its own socket (no network).
snapshot_psql() { docker exec -i "$(snapshot_ctr)" psql -U postgres -d "$1" -X -A -t -q -v ON_ERROR_STOP=1; }
snapshot_ready() { docker exec "$(snapshot_ctr)" pg_isready -q -h 127.0.0.1 -U postgres >/dev/null 2>&1; }
snapshot_networks() { docker inspect --format '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}} {{end}}' "$1" | xargs; }

snapshot_net_ids() {
  docker network ls -q --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.network=snapshots
}
# snapshot_down: core off the snapshots network; snapshot-postgres, its volumes and the network gone.
snapshot_down() {
  local core_ctr ctr net
  core_ctr=$(dc ps -q core) ctr=$(snapshot_ctr)
  for net in $(snapshot_net_ids); do
    [[ -z $core_ctr ]] || docker network disconnect "$net" "$core_ctr" >/dev/null 2>&1 || true
  done
  [[ -z $ctr ]] || docker logs "$ctr" >>"$EVIDENCE/s11-snapshot-postgres.log" 2>&1 || true
  dc --profile replay rm --stop --force --volumes snapshot-postgres >/dev/null 2>&1 || true
  for net in $(snapshot_net_ids); do docker network rm "$net" >/dev/null 2>&1 || true; done
}
# snapshot_isolation: how snapshot-postgres is reachable and where its data lives.
snapshot_isolation() {
  local ctr internal
  ctr=$(snapshot_ctr)
  [[ -n $ctr && -n $SNAPSHOT_NET ]] || { printf 'not running'; return 0; }
  if [[ $SNAPSHOT_NET == *' '* ]]; then internal="several networks: $SNAPSHOT_NET"
  else internal=$(docker network inspect --format '{{.Internal}}' "$SNAPSHOT_NET"); fi
  printf 'internal=%s ports=%s volumes=%s tmpfs=%s' "$internal" \
    "$(docker inspect --format '{{len .HostConfig.PortBindings}}' "$ctr")" \
    "$(docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}' "$ctr" | wc -w | tr -d ' ')" \
    "$(docker inspect --format '{{range $path, $_ := .HostConfig.Tmpfs}}{{$path}} {{end}}{{range .Mounts}}{{if eq .Type "tmpfs"}}{{.Destination}} {{end}}{{end}}' "$ctr" | xargs)"
}
# snapshot_gone VOLUMES: no snapshot-postgres container, none of its volumes, no snapshots network.
snapshot_gone() {
  local v
  [[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=snapshot-postgres) ]] \
    || { log "        the snapshot-postgres container is still there"; return 1; }
  for v in $1; do
    ! docker volume inspect "$v" >/dev/null 2>&1 || { log "        volume $v is still there"; return 1; }
  done
  [[ -z $(snapshot_net_ids) ]] || { log "        the snapshots network is still there"; return 1; }
}

# snapshot_up: starts snapshot-postgres with a superuser password made for this run, restores
# the pair (core's and Keycloak's dumps of $S11_BACKUP) into two databases through docker exec,
# and grants a SELECT-only role, with a password also made for this run, on them. Every password
# goes through stdin or the environment, never argv.
snapshot_up() {
  local dir=$STATE/backups/$S11_BACKUP db dump
  snapshot_down
  SNAPSHOT_SUPERUSER_PASSWORD=$(openssl rand -hex 16) SNAPSHOT_READER_PASSWORD=$(openssl rand -hex 16)
  SNAPSHOT_POSTGRES_PASSWORD=$SNAPSHOT_SUPERUSER_PASSWORD dc --profile replay up -d snapshot-postgres >/dev/null 2>&1 || return 1
  wait_until 90 2 snapshot_ready || return 1
  snapshot_psql postgres <<SQL || return 1
CREATE ROLE $SNAPSHOT_READER LOGIN PASSWORD '$SNAPSHOT_READER_PASSWORD';
CREATE DATABASE $SNAPSHOT_CORE_DB;
CREATE DATABASE $SNAPSHOT_KEYCLOAK_DB;
SQL
  for db in $SNAPSHOT_CORE_DB $SNAPSHOT_KEYCLOAK_DB; do
    case $db in $SNAPSHOT_CORE_DB) dump=super_skylab ;; *) dump=keycloak ;; esac
    docker exec -i "$(snapshot_ctr)" pg_restore -U postgres -d "$db" --no-owner --no-privileges \
      <"$dir/$dump.dump" 2>"$EVIDENCE/s11-snapshot-restore-$dump.log" || true
    snapshot_psql "$db" <<SQL || return 1
REVOKE ALL ON DATABASE $db FROM PUBLIC;
GRANT CONNECT ON DATABASE $db TO $SNAPSHOT_READER;
GRANT USAGE ON SCHEMA public TO $SNAPSHOT_READER;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO $SNAPSHOT_READER;
SQL
  done
  SNAPSHOT_NET=$(snapshot_networks "$(snapshot_ctr)")
}
snapshot_dsn() { # snapshot_dsn DB
  printf 'postgres://%s:%s@%s:5432/%s?sslmode=disable' "$SNAPSHOT_READER" "$SNAPSHOT_READER_PASSWORD" "$SNAPSHOT_HOST" "$1"
}

# replay_from_backup SERVICE T [--apply]: core's replay-from-backup in core's own container, as the
# restore wizard runs it: docker exec, the two snapshot DSNs passed by name from the environment.
# Sets REPLAY_OUT (stdout and stderr: counts and request ids only) and REPLAY_RC.
REPLAY_OUT='' REPLAY_RC=''
replay_from_backup() {
  local service=$1 at=$2
  shift 2
  REPLAY_OUT=$(CORE_SNAPSHOT_DATABASE_URL=$(snapshot_dsn "$SNAPSHOT_CORE_DB") \
    KEYCLOAK_SNAPSHOT_DATABASE_URL=$(snapshot_dsn "$SNAPSHOT_KEYCLOAK_DB") \
    docker exec -e CORE_SNAPSHOT_DATABASE_URL -e KEYCLOAK_SNAPSHOT_DATABASE_URL "$(dc ps -q core)" \
    /app/core-backend replay-from-backup --service "$service" --dumped-at "$at" "$@" 2>&1)
  REPLAY_RC=$?
  printf '$ replay-from-backup --service %s --dumped-at %s %s\n%s\nexit %s\n\n' \
    "$service" "$at" "$*" "$REPLAY_OUT" "$REPLAY_RC" >>"$EVIDENCE/s11-replay-from-backup.txt"
}
# replay_count LABEL: the number the replay's output prints after "LABEL: ".
replay_count() {
  awk -v label="$1: " '{ line = $0; sub(/^ +/, "", line) } index(line, label) == 1 { print substr(line, length(label) + 1); exit }' <<<"$REPLAY_OUT"
}
# replay_records [OUTCOME]: how many record lines the output holds (with that outcome).
replay_records() { grep -c "^account_erasure_replay .* outcome=${1:-}" <<<"$REPLAY_OUT" || true; }
# replay_summary: the counts of a run, for the check descriptions.
replay_summary() {
  printf 'exit %s, requests %s, 2 addresses %s, missing %s, unreadable %s, done %s, 202 %s, failed %s, open %s' "$REPLAY_RC" \
    "$(replay_count 'requests completed at or after the dump')" "$(replay_count 'with 2 address(es) resolved')" \
    "$(replay_count 'subject in neither snapshot (FAIL)')" "$(replay_count 'addresses unreadable (FAIL)')" \
    "$(replay_count 'done (200)')" "$(replay_count 'in progress (202, run again later)')" \
    "$(replay_count 'failed at the service (FAIL)')" "$(replay_count_open)"
}
replay_count_open() { awk -F': ' '/^requests not completed yet whose / { print $NF; exit }' <<<"$REPLAY_OUT"; }
# replay_dry_run_ok N: a dry run that resolved N requests, two addresses each, and sent nothing.
replay_dry_run_ok() {
  eq "$REPLAY_RC|$(head -n1 <<<"$REPLAY_OUT" | grep -c 'dry run, nothing is sent')|$(replay_count 'requests completed at or after the dump')|$(replay_count 'with 2 address(es) resolved')|$(replay_count 'subject in neither snapshot (FAIL)')|$(replay_count 'addresses unreadable (FAIL)')|$(replay_count_open)|$(replay_records)|$(grep -c '^done (200)' <<<"$REPLAY_OUT")" \
    "0|1|$1|$1|0|0|0|0|0"
}
# replay_apply_ok N: --apply exited 0 with N requests done (200), none 202, none failed, none open.
replay_apply_ok() {
  eq "$REPLAY_RC|$(replay_count 'requests completed at or after the dump')|$(replay_count 'done (200)')|$(replay_records done)|$(replay_records)|$(replay_count 'in progress (202, run again later)')|$(replay_count 'failed at the service (FAIL)')|$(replay_count 'subject in neither snapshot (FAIL)')|$(replay_count 'addresses unreadable (FAIL)')|$(replay_count_open)" \
    "0|$1|$1|$1|$1|0|0|0|0|0"
}
# replay_counts_sum KEY: the sum of one count over the done records (counts=key:n,...).
replay_counts_sum() {
  grep -o "[,=]$1:[0-9]*" <<<"$REPLAY_OUT" | awk -F: '{ n += $2 } END { print n + 0 }'
}

# --- what the replay may and may not change --------------------------------------------------------
NO_ID="'00000000-0000-0000-0000-000000000000'"
count_ids() { if [[ $1 == "$NO_ID" ]]; then echo 0; else awk -F, '{ print NF }' <<<"$1"; fi; }
# queue_rows_hash IDS [EXCLUDED_COLUMN...]: md5 and count of the queue rows (IDS: a SQL list of
# literals), whole or without these columns.
queue_rows_hash() {
  local ids=$1 expr="to_jsonb(q)" column
  shift
  for column in "$@"; do expr="$expr - '$column'"; done
  pg skymail "SELECT md5(coalesce(string_agg(($expr)::text, ',' ORDER BY q.id), '')) || '/' || count(*) FROM mail_queue q WHERE id IN ($ids)"
}
# bystander_rows_hash / bystander_rows_count MAIL: SkyMail's recipient, list membership and Mail
# onayı recipient rows of the addresses MAIL (a SQL list).
bystander_rows_hash() {
  pg skymail "SELECT md5(
      coalesce((SELECT string_agg(to_jsonb(r)::text, ',' ORDER BY to_jsonb(r)::text) FROM recipients r WHERE lower(r.email) IN ($1)), '') || '|' ||
      coalesce((SELECT string_agg(to_jsonb(m)::text, ',' ORDER BY to_jsonb(m)::text) FROM mailing_list_recipients m
                JOIN recipients r ON r.id = m.recipient_id WHERE lower(r.email) IN ($1)), '') || '|' ||
      coalesce((SELECT string_agg(to_jsonb(a)::text, ',' ORDER BY to_jsonb(a)::text) FROM mail_approval_recipients a WHERE lower(a.email) IN ($1)), ''))"
}
bystander_rows_count() {
  pg skymail "SELECT (SELECT count(*) FROM recipients WHERE lower(email) IN ($1)) || '/' ||
    (SELECT count(*) FROM mailing_list_recipients m JOIN recipients r ON r.id = m.recipient_id WHERE lower(r.email) IN ($1)) || '/' ||
    (SELECT count(*) FROM mail_approval_recipients WHERE lower(email) IN ($1))"
}
receipts_hash() { # receipts_hash DB: md5 and count of the service's receipts
  pg "$1" "SELECT md5(coalesce(string_agg(to_jsonb(r)::text, ',' ORDER BY to_jsonb(r)::text), '')) || '/' || count(*) FROM account_erasure_receipts r"
}
skymail_log_since() { docker logs --since "$1" "$(dc ps -q skymail)" 2>&1; }
forms_erase_runs() { pg forms_db "SELECT count(*) FROM harness_erase_runs"; }
# forms_puts FILE: how many erase PUTs the Forms stub counted for the requests in FILE (id per line).
forms_puts() {
  local total=0 request n
  while IFS=$'\t' read -r request _; do
    n=$(curl --silent "http://forms:9090/harness/stats/$request" | jq -r '.puts // 0' 2>/dev/null)
    total=$((total + ${n:-0}))
  done <"$1"
  printf '%s' "$total"
}

# leak_needles: what no replay output or log may carry: every test person's addresses, name,
# surname and subject id (p1's new sub of scenario 8 too), and every part of the three DSNs.
leak_needles() {
  local p
  for p in "${PERSONS[@]}"; do
    printf '%s\n' "${P_SCHOOL[$p]}" "${P_PERSONAL[$p]}" "$(full_name "$p")" "${P_LAST[$p]}" "${P_ID[$p]}"
  done
  [[ ! -s $STATE/s8.new-sub ]] || cat "$STATE/s8.new-sub"
  printf '%s\n' "$SNAPSHOT_READER_PASSWORD" "$SNAPSHOT_SUPERUSER_PASSWORD" "$SNAPSHOT_READER" "$SNAPSHOT_HOST" \
    "$SNAPSHOT_CORE_DB" "$SNAPSHOT_KEYCLOAK_DB" "$CORE_DB_PASSWORD" "$POSTGRES_PASSWORD" 'postgres://' 'sslmode='
}
# leaks FILE: how many lines of FILE carry a needle (the lines themselves are never printed).
leaks() { grep -ciFf <(leak_needles | grep .) "$1" || true; }

scenario_11() {
  local migrations db request subject service url replayed=0 first_not_200=0 bodies_cleared=0 p
  scenario_begin S11 'rollback rehearsal: down migrations refuse after deletions; backup restore with SkyMail paused, its restored queue closed unsent; core replay-from-backup with the paired core and Keycloak dumps erases the erased people'\''s SkyMail data again, e-mail-keyed rows included; no restored mail sent'
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

  check 'the pre-deletion backup exists' test -f "$STATE/backups/$S11_BACKUP/skymail.dump"
  check 'the backup holds the core and Keycloak dumps paired with the service dumps' \
    test -s "$STATE/backups/$S11_BACKUP/super_skylab.dump" -a -s "$STATE/backups/$S11_BACKUP/keycloak.dump"
  log "  dumped at: core $(dumped_at super_skylab), Keycloak $(dumped_at keycloak), SkyMail $(dumped_at skymail), CMS $(dumped_at skylab_cms), Forms $(dumped_at forms_db)"
  check 'the pair was dumped first: core, then Keycloak, then SkyMail, CMS, Forms (T_core <= T_keycloak <= T_service)' \
    not_after "$(dumped_at super_skylab)" "$(dumped_at keycloak)" "$(dumped_at skymail)" "$(dumped_at skylab_cms)" "$(dumped_at forms_db)"

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
      "$STATE/backups/$S11_BACKUP/$db.dump" 2>"$EVIDENCE/s11-restore-$db.log" || true
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

  # Step 5: the replay, for every request completed after the dump.
  local completed requests_file
  completed=$(pg super_skylab "SELECT count(*) FROM account_deletion_requests WHERE status = 'completed'")
  requests_file=$(mktemp)
  pg super_skylab "SELECT id, subject_id FROM account_deletion_requests WHERE status = 'completed' ORDER BY created_at" >"$requests_file"

  # 5a. CMS and Forms: §8's "emails": [] replay (the addresses are gone from core). Each must
  # answer 200 at once; a 202 is still repeated after its Retry-After, as core would.
  while IFS=$'\t' read -r request subject; do
    for service in cms forms; do
      case $service in cms) url=http://cms:5000 ;; forms) url=http://forms:8080 ;; esac
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
      else
        log "        replay $service $request answered $HTTP_STATUS"
      fi
    done
  done <"$requests_file"
  check "every completed request replayed with emails [] to CMS and Forms ($replayed of $((completed * 2)) answered 200)" eq "$replayed" "$((completed * 2))"
  check 'every emails [] replay answered 200 at once, none 202' eq "$first_not_200" 0

  # 5b. SkyMail: core's replay-from-backup with the pair (ADR-0053), before any other replay
  # reaches it. First what the restore brought back of the people erased after the dump.
  local skymail_t before_keyed before_rows names bystander_mail named_ids plain_ids
  local bystander_static bystander_named bystander_plain
  local -A closed_row_of
  skymail_t=$(dumped_at skymail)
  before_keyed=$(skymail_erased_email_keyed)
  before_rows=$(rows_holding_erased skymail)
  note "the restored SkyMail holds, of the $completed people erased after the dump: recipients/list memberships/queue rows to them/Mail onayı recipients $before_keyed; rows holding their addresses $before_rows"
  check 'the restore brought back their e-mail-keyed SkyMail rows (every kind > 0)' all_positive "$before_keyed"
  check 'SkyMail holds no receipt before the replay (its dump predates every request)' \
    eq "$(pg skymail "SELECT count(*) FROM account_erasure_receipts")" 0
  for p in "${PERSONS[@]}"; do
    [[ $(pg super_skylab "SELECT status FROM account_deletion_requests WHERE subject_id = '${P_ID[$p]}'") == completed ]] || continue
    closed_row_of[$p]=$(pg skymail "SELECT id FROM mail_queue WHERE id IN ($restored_list)
      AND lower(recipient_email) IN (lower('${P_SCHOOL[$p]}'), lower('${P_PERSONAL[$p]}'))")
  done
  # The bystander p8's rows: the recipient, list membership and Mail onayı recipient must stay as
  # they are; of the mail to p8, the rows naming an erased person lose their subject and body only.
  bystander_mail="lower('${P_SCHOOL[p8]}'), lower('${P_PERSONAL[p8]}')"
  names=$(for p in "${!closed_row_of[@]}"; do printf "%s'%%%s%%'" "${names_sep:-}" "$(full_name "$p")"; names_sep=,; done)
  named_ids=$(pg skymail "SELECT coalesce(string_agg(quote_literal(id), ',' ORDER BY id), quote_literal('00000000-0000-0000-0000-000000000000')) FROM mail_queue
    WHERE lower(recipient_email) IN ($bystander_mail) AND (subject || ' ' || body || ' ' || coalesce(body_html, '')) ILIKE ANY (ARRAY[$names])")
  plain_ids=$(pg skymail "SELECT coalesce(string_agg(quote_literal(id), ',' ORDER BY id), quote_literal('00000000-0000-0000-0000-000000000000')) FROM mail_queue
    WHERE lower(recipient_email) IN ($bystander_mail) AND id NOT IN ($named_ids)")
  bystander_static=$(bystander_rows_hash "$bystander_mail")
  bystander_named=$(queue_rows_hash "$named_ids" subject body body_html updated_at)
  bystander_plain=$(queue_rows_hash "$plain_ids")
  check "the bystander has a recipient, a list membership and a Mail onayı recipient ($(bystander_rows_count "$bystander_mail"))" \
    eq "$(bystander_rows_count "$bystander_mail")" 1/1/1
  check "mail to the bystander naming an erased person is there to watch ($(count_ids "$named_ids") row(s), $(count_ids "$plain_ids") naming nobody erased)" \
    ge "$(count_ids "$named_ids")" 1

  check 'snapshot-postgres is up with the paired core and Keycloak dumps restored' snapshot_up
  check "snapshot-postgres sits only on the internal network, publishes no port, keeps its data in memory ($(snapshot_isolation))" \
    eq "$(snapshot_isolation)" 'internal=true ports=0 volumes=0 tmpfs=/var/lib/postgresql/data'
  check 'the driver has no route to snapshot-postgres' not pg_isready -q -h "$SNAPSHOT_HOST" -t 3
  local person_ids
  person_ids=$(for p in "${PERSONS[@]}"; do printf "%s'%s'" "${ids_sep:-}" "${P_ID[$p]}"; ids_sep=,; done)
  check 'the core snapshot holds every test person' \
    eq "$(snapshot_psql "$SNAPSHOT_CORE_DB" <<<"SELECT count(*) FROM users WHERE id IN ($person_ids) AND account_state = 'active'")" "${#PERSONS[@]}"
  check 'the Keycloak snapshot holds every test person in realm e-skylab' \
    eq "$(snapshot_psql "$SNAPSHOT_KEYCLOAK_DB" <<<"SELECT count(*) FROM user_entity u JOIN realm r ON r.id = u.realm_id WHERE r.name = 'e-skylab' AND u.id IN ($person_ids)")" "${#PERSONS[@]}"
  local core_ctr replay_start
  core_ctr=$(dc ps -q core)
  docker network connect "$SNAPSHOT_NET" "$core_ctr" >/dev/null 2>&1
  check 'core is on the snapshots network for the replay' grep -qw -- "$SNAPSHOT_NET" <<<"$(snapshot_networks "$core_ctr")"

  replay_start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  log "  replay-from-backup --service skymail --dumped-at $skymail_t (the SkyMail dump's instant)"
  replay_from_backup skymail "$skymail_t"
  check "replay-from-backup --service skymail, dry run: $(replay_summary)" replay_dry_run_ok "$completed"
  check 'the dry run sent nothing: no receipt, the same e-mail-keyed rows, no erasure in SkyMail'"'"'s log' \
    eq "$(pg skymail "SELECT count(*) FROM account_erasure_receipts")|$(skymail_erased_email_keyed)|$(rows_holding_erased skymail)|$(skymail_log_since "$replay_start" | grep -c 'account erasure')" \
    "0|$before_keyed|$before_rows|0"
  replay_from_backup skymail "$skymail_t" --apply
  check "replay-from-backup --service skymail --apply: $(replay_summary)" replay_apply_ok "$completed"
  note "SkyMail's replay: bodies_cleared $(replay_counts_sum bodies_cleared), queue_rows_cleared $(replay_counts_sum queue_rows_cleared), queue_rows_deleted $(replay_counts_sum queue_rows_deleted), recipients_deleted $(replay_counts_sum recipients_deleted), list_memberships_deleted $(replay_counts_sum list_memberships_deleted), approval_recipients_removed $(replay_counts_sum approval_recipients_removed)"
  check 'SkyMail wrote one receipt per request' eq "$(pg skymail "SELECT count(*) FROM account_erasure_receipts")" "$completed"
  check 'SkyMail holds none of their recipients, list memberships, queue rows or Mail onayı recipients' \
    eq "$(skymail_erased_email_keyed)" 0/0/0/0
  check 'no SkyMail row holds an address of the people erased after the dump' eq "$(rows_holding_erased skymail)" 0
  for p in "${!closed_row_of[@]}"; do
    # The person's restored mail, closed unsent: the row stays (the send's counts), nothing of them.
    check "SkyMail: $p's closed row keeps nothing of $p (address, name, subject, body)" \
      eq "$(pg skymail "SELECT status || '/' || recipient_email || '/' || recipient_full_name || '/' || subject || '/' || body || '/' || coalesce(body_html, 'NULL')
            FROM mail_queue WHERE id = '${closed_row_of[$p]}'")" 'failed//Silinmiş kullanıcı///NULL'
  done
  check "the bystander's recipient, list membership and Mail onayı recipient are byte-identical" \
    eq "$(bystander_rows_hash "$bystander_mail")" "$bystander_static"
  check "the bystander's mail naming no erased person is byte-identical" eq "$(queue_rows_hash "$plain_ids")" "$bystander_plain"
  check "the bystander's mail naming an erased person changed only in subject and body, now empty" \
    eq "$(queue_rows_hash "$named_ids" subject body body_html updated_at)/$(pg skymail "SELECT count(*) FILTER (WHERE subject <> '' OR body <> '' OR body_html IS NOT NULL) FROM mail_queue WHERE id IN ($named_ids)")" \
    "$bystander_named/0"

  # 5c. CMS and Forms, after their "emails": [] replay: the receipts answer, nothing changes.
  local t receipts_before rows_before runs_before puts_before
  for service in cms forms; do
    case $service in cms) db=skylab_cms t=$(dumped_at skylab_cms) ;; forms) db=forms_db t=$(dumped_at forms_db) ;; esac
    receipts_before=$(receipts_hash "$db") rows_before=$(rows_holding_erased "$db")
    runs_before=$(forms_erase_runs) puts_before=$(forms_puts "$requests_file")
    replay_from_backup "$service" "$t"
    check "replay-from-backup --service $service, dry run: $(replay_summary)" replay_dry_run_ok "$completed"
    [[ $service != forms ]] || check 'the Forms dry run sent nothing (the stub counted no PUT)' eq "$(forms_puts "$requests_file")" "$puts_before"
    replay_from_backup "$service" "$t" --apply
    check "replay-from-backup --service $service --apply, after the emails [] replay: $(replay_summary)" replay_apply_ok "$completed"
    check "$service answered from the emails [] replay's receipts: receipts and rows with erased addresses unchanged ($rows_before)" \
      eq "$(receipts_hash "$db")|$(rows_holding_erased "$db")" "$receipts_before|$rows_before"
    [[ $service != forms ]] || check 'the Forms stub got one PUT per request and ran no erasure again' \
      eq "$(($(forms_puts "$requests_file") - puts_before))/$(forms_erase_runs)" "$completed/$runs_before"
  done
  check 'no CMS row holds an address of the people erased after the dump' eq "$(rows_holding_erased skylab_cms)" 0

  # 5d. The "emails": [] replay reaching SkyMail after replay-from-backup is answered from its receipts.
  local skymail_receipts again=0
  skymail_receipts=$(receipts_hash skymail)
  while IFS=$'\t' read -r request subject; do
    replay_command skymail http://skymail:3000 "$request" "$subject"
    [[ $HTTP_STATUS != 200 ]] || again=$((again + 1))
  done <"$requests_file"
  check "an emails [] replay to SkyMail afterwards answers 200 from its receipts ($again of $completed), changing nothing" \
    eq "$again|$(receipts_hash skymail)|$(rows_holding_erased skymail)" "$completed|$skymail_receipts|0"

  # 5e. Nothing the replay printed or made the others log names a person or a DSN part.
  local logs output_leaks log_leaks snapshot_volumes
  logs=$EVIDENCE/s11-replay-logs.txt
  : >"$logs"
  for service in core skymail cms forms; do docker logs --since "$replay_start" "$(dc ps -q "$service")" >>"$logs" 2>&1; done
  docker logs "$(snapshot_ctr)" >>"$logs" 2>&1
  output_leaks=$(leaks "$EVIDENCE/s11-replay-from-backup.txt") log_leaks=$(leaks "$logs")
  check "the replay's output carries no address, name, subject id or DSN part ($output_leaks line(s))" eq "$output_leaks" 0
  check "the logs of core, SkyMail, CMS, Forms and snapshot-postgres since the replay carry none either ($log_leaks line(s))" eq "$log_leaks" 0

  # 5f. The snapshots are destroyed: container, volumes, network; core leaves the network first.
  snapshot_volumes=$(docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}' "$(snapshot_ctr)" | xargs)
  snapshot_down
  check 'snapshot-postgres, its volumes and the snapshots network are gone' snapshot_gone "$snapshot_volumes"
  SNAPSHOT_READER_PASSWORD='' SNAPSHOT_SUPERUSER_PASSWORD=''
  rm -f "$requests_file"

  for p in "${!closed_row_of[@]}"; do
    for db in skymail skylab_cms forms_db; do
      check "$db: no row carries the subject of $p after the replay" eq "$(count_lines "$(subject_hits "$db" "${P_ID[$p]}")")" 0
    done
  done
  check "the bystander's closed row is byte-identical after the replay" eq "$(queue_row_hash "$bystander_row")" "$bystander_closed"
  check "the bystander's closed row still says what it said" \
    eq "$(pg skymail "SELECT subject <> '' AND body <> '' AND error = '$RESTORE_NOT_SENT' FROM mail_queue WHERE id = '$bystander_row'")" t

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
  # The erasure cleared the error of the erased people's closed rows with the rest of the row.
  check 'after unpausing, no restored row went out (all still failed, none sent)' \
    eq "$(pg skymail "SELECT count(*) FROM mail_queue WHERE id IN ($restored_list) AND status = 'failed'")" "$restored_pending"
  check 'no mail of the restored queue reached mailpit at any point' eq "$(mailpit_to "$addresses")" "$baseline"
  check "the bystander's closed row is byte-identical after unpausing" eq "$(queue_row_hash "$bystander_row")" "$bystander_closed"
  rm -f "$addresses" "$control_to"

  local forms_rows
  forms_rows=$(rows_holding_erased forms_db)
  if [[ $forms_rows != 0 ]]; then
    known_gap 18 "Forms (stub) got the emails [] replay before replay-from-backup, which then only got those receipts back: $forms_rows forms_db row(s) still hold addresses of the $completed people erased after the dump (guest responses). A restored service must get replay-from-backup instead of the emails [] replay, as SkyMail does here."
  fi
  scenario_end
}
