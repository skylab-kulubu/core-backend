#!/usr/bin/env bash
# Shared helpers of the erasure harness driver. Sourced by main.sh and every scenario; runs in
# the driver container (bash 5, curl, jq, psql, redis-cli, docker CLI), attached to the harness
# network, with the harness directory mounted at the same absolute path as on the host.
# shellcheck disable=SC2034

HARNESS_DIR=${HARNESS_DIR:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)}
STATE=${HARNESS_STATE:-$HARNESS_DIR/.state}
PROJECT=${HARNESS_PROJECT:-skylab-erasure-harness}
EVIDENCE=$STATE/evidence
RESULTS=$STATE/results.tsv
CA=$STATE/pki/ca.crt
ISSUER=https://e.yildizskylab.com/realms/e-skylab
REALM_URL=$ISSUER
CORE=http://core:8080
DELETED_SUBJECT=00000000-0000-4000-8000-000000000000

set -a
# shellcheck source=/dev/null
source "$STATE/compose.env"
# shellcheck source=/dev/null
source "$STATE/images.env"
set +a

mkdir -p "$EVIDENCE"

dc() {
  docker compose -p "$PROJECT" -f "$HARNESS_DIR/compose.yaml" \
    --env-file "$STATE/compose.env" --env-file "$STATE/images.env" "$@"
}

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

# --- persons --------------------------------------------------------------------------------------
declare -gA P_ID P_USER P_FIRST P_LAST P_SCHOOL P_PERSONAL
while IFS=$'\t' read -r key id user first last school personal _; do
  [[ -z $key || $key == \#* ]] && continue
  P_ID[$key]=$id P_USER[$key]=$user P_FIRST[$key]=$first P_LAST[$key]=$last
  P_SCHOOL[$key]=$school P_PERSONAL[$key]=$personal
done <"$HARNESS_DIR/lib/persons.tsv"
PERSONS=(p1 p2 p3 p4 p5 p6 p7 p8 p9)
full_name() { printf '%s %s' "${P_FIRST[$1]}" "${P_LAST[$1]}"; }

# --- results --------------------------------------------------------------------------------------
SCENARIO=''
SCENARIO_FAILURES=0
scenario_begin() {
  SCENARIO=$1
  SCENARIO_FAILURES=0
  log "=== scenario $SCENARIO: $2"
  printf '%s\tRUNNING\t%s\t%s\n' "$SCENARIO" "$(date -u +%FT%TZ)" "$2" >>"$RESULTS"
}
check() { # check "description" command...
  local description=$1
  shift
  if "$@"; then
    log "  ok    $description"
  else
    log "  FAIL  $description"
    SCENARIO_FAILURES=$((SCENARIO_FAILURES + 1))
  fi
}
note() { log "  note  $*"; printf '%s\tNOTE\t%s\t%s\n' "$SCENARIO" "$(date -u +%FT%TZ)" "$*" >>"$RESULTS"; }
scenario_end() {
  local status=PASS
  ((SCENARIO_FAILURES == 0)) || status=FAIL
  printf '%s\t%s\t%s\t%s failed checks\n' "$SCENARIO" "$status" "$(date -u +%FT%TZ)" "$SCENARIO_FAILURES" >>"$RESULTS"
  printf '\n==> SCENARIO %s: %s\n\n' "$SCENARIO" "$status"
  [[ $status == PASS ]]
}
eq() { [[ "$1" == "$2" ]] || { log "        expected [$2], got [$1]"; return 1; }; }
ge() { [[ "$1" =~ ^[0-9]+$ && "$1" -ge "$2" ]] || { log "        expected >= $2, got [$1]"; return 1; }; }

# wait_until TIMEOUT_SECONDS INTERVAL command...: polls until the command succeeds.
wait_until() {
  local timeout=$1 interval=$2 started
  shift 2
  started=$(date +%s)
  until "$@"; do
    (($(date +%s) - started < timeout)) || return 1
    sleep "$interval"
  done
}

# --- HTTP -----------------------------------------------------------------------------------------
# All public names resolve to the edge inside the harness network; the harness CA is the only
# trust anchor, so a request that leaves the network cannot succeed.
hcurl() { curl --silent --show-error --cacert "$CA" "$@"; }

# http_status VAR_BODY METHOD URL [curl args...]: sets HTTP_STATUS and HTTP_BODY.
HTTP_STATUS='' HTTP_BODY=''
http() {
  local method=$1 url=$2 out
  shift 2
  out=$(hcurl --request "$method" --write-out $'\n%{http_code}' "$@" "$url") || { HTTP_STATUS=000 HTTP_BODY=''; return 0; }
  HTTP_STATUS=${out##*$'\n'}
  HTTP_BODY=${out%$'\n'*}
}

jwt_payload() {
  local segment
  segment=$(cut -d. -f2 <<<"$1" | tr '_-' '/+')
  case $((${#segment} % 4)) in 2) segment="$segment==" ;; 3) segment="$segment=" ;; esac
  base64 -d <<<"$segment" 2>/dev/null
}

# --- databases and Redis --------------------------------------------------------------------------
pg() { # pg DB SQL: one value per line, tab-separated columns
  PGPASSWORD=$POSTGRES_PASSWORD psql -h postgres -U postgres -d "$1" -X -A -t -F $'\t' -v ON_ERROR_STOP=1 -c "$2"
}
pgf() { # pgf DB FILE
  PGPASSWORD=$POSTGRES_PASSWORD psql -h postgres -U postgres -d "$1" -X -q -v ON_ERROR_STOP=1 -f "$2"
}
aa_redis() { # the recovery operator: read-only checks
  REDISCLI_AUTH=$REDIS_RECOVERY_OPERATOR_PASSWORD redis-cli --no-auth-warning --tls \
    --cacert "$CA" --cert "$STATE/pki/clients/recovery-operator.crt" --key "$STATE/pki/clients/recovery-operator.key" \
    --sni account-access-redis -h account-access-redis -p 6379 --user recovery-operator "$@"
}
marker_key() { printf 'skylab:account-access:v1:blocked:%s' "$(printf '%s\0%s' "$ISSUER" "$1" | sha256sum | cut -c1-64)"; }
marker_of() { aa_redis GET "$(marker_key "$1")"; }
cms_redis() { redis-cli -h cms-redis -p 6379 "$@"; }

# --- Keycloak -------------------------------------------------------------------------------------
kc_admin_token() {
  hcurl --fail --data-urlencode grant_type=password --data-urlencode client_id=admin-cli \
    --data-urlencode username=admin --data-urlencode "password=$KC_ADMIN_PASSWORD" \
    https://e.yildizskylab.com/realms/master/protocol/openid-connect/token | jq -r .access_token
}
KC_ADMIN_TOKEN=''
KC_ADMIN_TOKEN_AT=0
kc() { # kc METHOD PATH [curl args]: admin REST on realm e-skylab (PATH relative to the realm)
  local method=$1 path=$2
  shift 2
  if ((($(date +%s) - KC_ADMIN_TOKEN_AT) > 45)); then
    KC_ADMIN_TOKEN=$(kc_admin_token)
    KC_ADMIN_TOKEN_AT=$(date +%s)
  fi
  http "$method" "https://e.yildizskylab.com/admin/realms/e-skylab$path" \
    -H "Authorization: Bearer $KC_ADMIN_TOKEN" -H 'Content-Type: application/json' "$@"
}
kc_client_uuid() { kc GET "/clients?clientId=$1"; jq -r '.[0].id // empty' <<<"$HTTP_BODY"; }
kc_client_secret() { kc GET "/clients/$(kc_client_uuid "$1")/client-secret"; jq -r .value <<<"$HTTP_BODY"; }
kc_user_status() { kc GET "/users/$1"; printf '%s' "$HTTP_STATUS"; }

# user_token PERSON SERVICE: the person's access token for one service (harness-<service> client,
# direct grant). Prints the token.
user_token() {
  hcurl --fail --data-urlencode grant_type=password --data-urlencode "client_id=harness-$2" \
    --data-urlencode "username=${P_USER[$1]}" --data-urlencode "password=$HARNESS_PERSON_PASSWORD" \
    --data-urlencode scope=openid "$REALM_URL/protocol/openid-connect/token" | jq -r .access_token
}

# login_page_action HTML: the form action of a Keycloak login page, either the keycloakify
# theme's kcContext.url.loginAction or the plain theme's form.
login_page_action() {
  local action
  action=$(grep -Eo '"loginAction"[[:space:]]*:[[:space:]]*"[^"]*"' <<<"$1" | head -n1 \
    | sed -E 's/^"loginAction"[[:space:]]*:[[:space:]]*//' | jq -r . 2>/dev/null || true)
  if [[ -z $action ]]; then
    action=$(grep -Eo '<form[^>]*id="kc-form-login"[^>]*' <<<"$1" | grep -Eo 'action="[^"]*"' | head -n1 \
      | sed -E 's/^action="//; s/"$//; s/&amp;/\&/g' || true)
  fi
  printf '%s' "$action"
}

# ac_login PERSON: an Account Center session the way my. starts one (PAR, the realm's login form,
# code exchange with PKCE). Sets AC_ACCESS, AC_ID, AC_REFRESH.
AC_ACCESS='' AC_ID='' AC_REFRESH=''
ac_login() {
  local person=$1 secret verifier challenge par request_uri page action status location code response jar headers
  secret=$ACCOUNT_CENTER_OIDC_CLIENT_SECRET
  jar=$(mktemp) headers=$(mktemp)
  verifier="harness-$person-$(openssl rand -hex 24)"
  challenge=$(printf '%s' "$verifier" | openssl dgst -binary -sha256 | openssl base64 -A | tr '+/' '-_' | tr -d '=')
  par=$(hcurl --fail --user "account-center:$secret" \
    --data-urlencode client_id=account-center --data-urlencode response_type=code --data-urlencode scope=openid \
    --data-urlencode redirect_uri=https://my.yildizskylab.com/api/auth/callback \
    --data-urlencode "code_challenge=$challenge" --data-urlencode code_challenge_method=S256 \
    --data-urlencode "state=harness-$person" --data-urlencode "nonce=harness-$person-$(openssl rand -hex 8)" \
    "$REALM_URL/protocol/openid-connect/ext/par/request")
  request_uri=$(jq -r '.request_uri | @uri' <<<"$par")
  page=$(hcurl --fail --location --cookie-jar "$jar" --cookie "$jar" \
    "$REALM_URL/protocol/openid-connect/auth?client_id=account-center&request_uri=$request_uri")
  action=$(login_page_action "$page")
  [[ -n $action ]] || { log "ac_login $person: no login action"; rm -f "$jar" "$headers"; return 1; }
  status=$(hcurl --output /dev/null --dump-header "$headers" --write-out '%{http_code}' \
    --cookie-jar "$jar" --cookie "$jar" \
    --data-urlencode "username=${P_USER[$person]}" --data-urlencode "password=$HARNESS_PERSON_PASSWORD" \
    --data-urlencode credentialId= "$action")
  location=$(awk 'tolower($1) == "location:" { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print }' "$headers")
  rm -f "$jar" "$headers"
  [[ $status == 302 ]] || { log "ac_login $person: login returned $status"; return 1; }
  code=$(sed -n 's/.*[?&]code=\([^&]*\).*/\1/p' <<<"$location")
  [[ -n $code ]] || { log "ac_login $person: redirect without a code"; return 1; }
  response=$(hcurl --fail --user "account-center:$secret" \
    --data-urlencode grant_type=authorization_code --data-urlencode client_id=account-center \
    --data-urlencode "code=$code" --data-urlencode redirect_uri=https://my.yildizskylab.com/api/auth/callback \
    --data-urlencode "code_verifier=$verifier" "$REALM_URL/protocol/openid-connect/token")
  AC_ACCESS=$(jq -r .access_token <<<"$response")
  AC_ID=$(jq -r .id_token <<<"$response")
  AC_REFRESH=$(jq -r .refresh_token <<<"$response")
  [[ -n $AC_ACCESS && $AC_ACCESS != null ]]
}

# sudo_token ACCESS: the sky-account Sudo mode token after the password proof.
sudo_token() {
  http POST "$REALM_URL/sky-account/v1/sudo/password" -H "Authorization: Bearer $1" \
    -H 'Content-Type: application/json' --data "$(jq -cn --arg p "$HARNESS_PERSON_PASSWORD" '{password: $p}')"
  [[ $HTTP_STATUS == 200 ]] || { log "sudo/password returned $HTTP_STATUS"; return 1; }
  jq -r .sudoToken <<<"$HTTP_BODY"
}

new_idempotency_key() { openssl rand 32 | base64 -w0 | tr '+/' '-_' | tr -d '='; }

# --- core -----------------------------------------------------------------------------------------
# core_self_delete ACCESS SUDO KEY: core's self-service intake with the sudo proof.
core_self_delete() {
  http POST "$CORE/v1/account-deletion-requests/self" -H "Authorization: Bearer $1" -H "X-Sky-Sudo: $2" \
    -H "Idempotency-Key: $3"
}
core_status() { http GET "$CORE/v1/account-deletion-requests/status" -H "Authorization: DeletionReceipt $1"; }
core_retry() { http POST "$CORE/v1/account-deletion-requests/status/retry" -H "Authorization: DeletionReceipt $1"; }
request_of() { pg super_skylab "SELECT id FROM account_deletion_requests WHERE subject_id = '$1'"; }
request_status() { pg super_skylab "SELECT status FROM account_deletion_requests WHERE id = '$1'"; }
request_field() { pg super_skylab "SELECT $2 FROM account_deletion_requests WHERE id = '$1'"; }
step_done() { [[ $(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$1' AND step = '$2'") == 1 ]]; }
steps_of() { pg super_skylab "SELECT string_agg(step, ',' ORDER BY completed_at, step) FROM account_deletion_steps WHERE request_id = '$1'"; }
request_is() { [[ $(request_status "$1") == "$2" ]]; }
metric() { hcurl "$CORE/v1/metrics" | awk -v name="$1" '$1 == name { print $2 }'; }

# start_deletion PERSON: logs the person in as Account Center, takes a sudo proof and sends
# core's intake. Sets DEL_KEY, DEL_ACCESS, DEL_SUDO, DEL_RECEIPT, DEL_REQUEST.
DEL_KEY='' DEL_ACCESS='' DEL_SUDO='' DEL_RECEIPT='' DEL_REQUEST=''
start_deletion() {
  local person=$1
  ac_login "$person" || return 1
  DEL_ACCESS=$AC_ACCESS
  DEL_SUDO=$(sudo_token "$DEL_ACCESS") || return 1
  DEL_KEY=$(new_idempotency_key)
  core_self_delete "$DEL_ACCESS" "$DEL_SUDO" "$DEL_KEY"
  [[ $HTTP_STATUS == 202 || $HTTP_STATUS == 200 ]] || { log "intake returned $HTTP_STATUS"; return 1; }
  DEL_RECEIPT=$(jq -r .receipt <<<"$HTTP_BODY")
  DEL_REQUEST=$(request_of "${P_ID[$person]}")
  [[ -n $DEL_REQUEST ]]
}

# --- PII scans ------------------------------------------------------------------------------------
# db_lines DB: every data row of every table as "table<TAB>row", from pg_dump --data-only.
db_lines() {
  local dump
  dump=$(mktemp)
  if ! PGPASSWORD=$POSTGRES_PASSWORD pg_dump -h postgres -U postgres -d "$1" --data-only --no-owner --no-privileges \
    >"$dump" 2>"$dump.err"; then
    cat "$dump.err" >&2
    rm -f "$dump" "$dump.err"
    return 1
  fi
  awk '/^COPY / { table = $2; next } /^\\\.$/ { table = ""; next } table != "" { print table "\t" $0 }' "$dump"
  rm -f "$dump" "$dump.err"
}
# pii_needles PERSON: the strings that name the person (addresses, full name, surname).
pii_needles() {
  printf '%s\n' "${P_SCHOOL[$1]}" "${P_PERSONAL[$1]}" "$(full_name "$1")" "${P_LAST[$1]}"
}
# pii_hits DB PERSON [ALLOWED_TABLE_REGEX]: rows of DB naming the person, outside the tables the
# decision keeps on purpose (Certificate recipient name, News byline). Prints "table" per hit.
pii_hits() {
  local db=$1 person=$2 allowed=${3:-'^$'} needles lines
  needles=$(pii_needles "$person")
  lines=$(db_lines "$db") || { echo "dump-of-$db-failed"; return 0; }
  grep -iF -f <(printf '%s\n' "$needles") <<<"$lines" | cut -f1 | grep -Ev "$allowed" || true
}
# subject_hits DB SUBJECT [ALLOWED_TABLE_REGEX]: rows of DB carrying the subject id.
subject_hits() {
  local db=$1 subject=$2 allowed=${3:-'^$'} lines
  lines=$(db_lines "$db") || { echo "dump-of-$db-failed"; return 0; }
  grep -iF "$subject" <<<"$lines" | cut -f1 | grep -Ev "$allowed" || true
}
count_lines() { if [[ -z $1 ]]; then echo 0; else grep -c . <<<"$1"; fi; }
steps_done() { local r=$1 s; shift; for s in "$@"; do step_done "$r" "$s" || return 1; done; }
# request_where REQUEST SQL_BOOLEAN: true when the boolean holds for the request row.
request_where() { [[ $(pg super_skylab "SELECT ($2) FROM account_deletion_requests WHERE id = '$1'") == t ]]; }
not() { ! "$@"; }
# save_logs SERVICE: appends the container's log to .state/evidence/logs-archive before the
# container is recreated, so the log scan (scenario 9) still sees what it wrote.
save_logs() {
  mkdir -p "$EVIDENCE/logs-archive"
  dc logs --no-color --no-log-prefix --timestamps "$1" >>"$EVIDENCE/logs-archive/$1.log" 2>&1 || true
}
