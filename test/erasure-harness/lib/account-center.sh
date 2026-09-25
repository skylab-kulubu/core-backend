#!/usr/bin/env bash
# Account Center's BFF driven the way a browser drives it: its own OIDC login (the realm's
# login form), Sudo mode with the password, prepare, the typed confirmation, and the status
# page's receipt cookie. Every call carries the exact Origin; mutations carry the session's
# CSRF token. Sourced by main.sh.

AC_URL=https://my.yildizskylab.com
AC_JAR=''
AC_CSRF=''

ac_curl() { hcurl --cookie "$AC_JAR" --cookie-jar "$AC_JAR" "$@"; }

# ac_location HEADERS_FILE: the Location header of a saved response.
ac_location() { awk 'tolower($1) == "location:" { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print }' "$1" | tail -n1; }

# ac_bff_login PERSON: a fresh browser (cookie jar) signs in to Account Center.
ac_bff_login() {
  local person=$1 headers kc_jar location page action status
  AC_JAR=$STATE/tmp/ac-$person.jar
  mkdir -p "$STATE/tmp"
  rm -f "$AC_JAR"
  : >"$AC_JAR"
  headers=$(mktemp) kc_jar=$(mktemp)
  ac_curl --output /dev/null --dump-header "$headers" "$AC_URL/api/auth/login"
  location=$(ac_location "$headers")
  [[ $location == "$REALM_URL/"* ]] || { log "ac_bff_login: login did not go to Keycloak ($location)"; return 1; }
  page=$(hcurl --location --cookie "$kc_jar" --cookie-jar "$kc_jar" "$location")
  action=$(login_page_action "$page")
  [[ -n $action ]] || { log "ac_bff_login: no login form"; return 1; }
  status=$(hcurl --output /dev/null --dump-header "$headers" --write-out '%{http_code}' \
    --cookie "$kc_jar" --cookie-jar "$kc_jar" \
    --data-urlencode "username=${P_USER[$person]}" --data-urlencode "password=$HARNESS_PERSON_PASSWORD" \
    --data-urlencode credentialId= "$action")
  location=$(ac_location "$headers")
  rm -f "$kc_jar"
  [[ $status == 302 && $location == "$AC_URL/api/auth/callback"* ]] || { log "ac_bff_login: login answered $status $location"; return 1; }
  status=$(ac_curl --output /dev/null --dump-header "$headers" --write-out '%{http_code}' "$location")
  rm -f "$headers"
  [[ $status == 303 || $status == 302 ]] || { log "ac_bff_login: callback answered $status"; return 1; }
  ac_bff_csrf
}

# ac_bff_csrf: the session's CSRF token, from the sudo methods read.
ac_bff_csrf() {
  http GET "$AC_URL/api/account/sudo/methods" --cookie "$AC_JAR" --cookie-jar "$AC_JAR" -H "Origin: $AC_URL"
  [[ $HTTP_STATUS == 200 ]] || { log "ac_bff_csrf: sudo methods answered $HTTP_STATUS"; return 1; }
  AC_CSRF=$(jq -r .csrfToken <<<"$HTTP_BODY")
  [[ -n $AC_CSRF && $AC_CSRF != null ]]
}

ac_bff_session_ok() { # the account page's data read works: the session is alive
  http GET "$AC_URL/api/account/identity" --cookie "$AC_JAR" -H "Origin: $AC_URL"
  [[ $HTTP_STATUS == 200 ]]
}

ac_bff_sudo_password() {
  http POST "$AC_URL/api/account/sudo/password" --cookie "$AC_JAR" --cookie-jar "$AC_JAR" \
    -H "Origin: $AC_URL" -H "x-csrf-token: $AC_CSRF" -H 'Content-Type: application/json' \
    --data "$(jq -cn --arg p "$HARNESS_PERSON_PASSWORD" '{password: $p}')"
  [[ $HTTP_STATUS == 200 ]] || { log "sudo/password answered $HTTP_STATUS $HTTP_BODY"; return 1; }
}

ac_bff_prepare() {
  http POST "$AC_URL/api/account/deletion/prepare" --cookie "$AC_JAR" --cookie-jar "$AC_JAR" \
    -H "Origin: $AC_URL" -H "x-csrf-token: $AC_CSRF"
  [[ $HTTP_STATUS == 200 && $(jq -r .step <<<"$HTTP_BODY") == confirm ]] || { log "prepare answered $HTTP_STATUS $HTTP_BODY"; return 1; }
}

# ac_bff_submit: the typed confirmation. Sets HTTP_STATUS and AC_SUBMIT_LOCATION.
AC_SUBMIT_LOCATION=''
ac_bff_submit() {
  local headers
  headers=$(mktemp)
  HTTP_STATUS=$(ac_curl --output /dev/null --dump-header "$headers" --write-out '%{http_code}' --max-time 30 \
    -H "Origin: $AC_URL" -H 'Content-Type: application/x-www-form-urlencoded' \
    -H 'Sec-Fetch-Mode: navigate' -H 'Sec-Fetch-Dest: document' -H 'Sec-Fetch-Site: same-origin' -H 'Accept: text/html' \
    --data-urlencode 'confirmation=HESABIMI SİL' --data-urlencode "csrfToken=$AC_CSRF" \
    "$AC_URL/api/account/deletion")
  AC_SUBMIT_LOCATION=$(ac_location "$headers")
  rm -f "$headers"
}

ac_bff_status() { http GET "$AC_URL/api/account/deletion/status" --cookie "$AC_JAR" --cookie-jar "$AC_JAR" -H "Origin: $AC_URL"; }

# ac_bff_delete PERSON: login, Sudo mode, prepare, confirmation. Sets DEL_REQUEST.
ac_bff_delete() {
  local person=$1
  ac_bff_login "$person" || return 1
  ac_bff_sudo_password || return 1
  ac_bff_csrf || return 1
  ac_bff_prepare || return 1
  ac_bff_submit
  log "  Account Center submit answered $HTTP_STATUS -> $AC_SUBMIT_LOCATION"
  DEL_REQUEST=$(request_of "${P_ID[$person]}")
}

# ac_bff_retry: the status page's retry button (receipt cookie and its receipt-bound CSRF token).
ac_bff_retry() {
  local csrf
  ac_bff_status
  csrf=$(jq -r .csrfToken <<<"$HTTP_BODY" 2>/dev/null)
  http POST "$AC_URL/api/account/deletion/status/retry" --cookie "$AC_JAR" --cookie-jar "$AC_JAR" \
    -H "Origin: $AC_URL" -H "x-csrf-token: $csrf"
}
