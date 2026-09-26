#!/usr/bin/env bash
# Scenario 2 — a lost answer (audit gate 7), through Account Center: the person confirms, core
# accepts the intake, but its answer is held past Account Center's 5-second timeout, so Account
# Center sends the person to its recovery page. The saga closes the Keycloak session the sudo
# token is bound to; the recovery page then replays the intake with the same idempotency key and
# the same sealed bearer and sudo token, and core answers the accepted state (202/200), not 401.

introspect_active() { # introspect_active TOKEN: "true"/"false" as the realm tells core
  hcurl --user "core:$CORE_CLIENT_SECRET" --data-urlencode "token=$1" \
    "$REALM_URL/protocol/openid-connect/token/introspect" | jq -r .active
}

intake_calls_at_least() {
  (($(dc logs --no-color --no-log-prefix hold-proxy 2>/dev/null | grep -c 'POST /v1/account-deletion-requests/self' || true) >= $1))
}

ac_bff_prepared() { ac_bff_login "$1" && ac_bff_sudo_password && ac_bff_csrf && ac_bff_prepare; }

scenario_2() {
  local person=p6 subject intakes_before
  subject=${P_ID[$person]}
  scenario_begin S2 'lost answer: accepted, answer dropped, saga closes the session, same key replays 202/200 (not 401)'

  check 'Account Center login, Sudo mode and prepare (bearer and sudo token sealed in the intent)' ac_bff_prepared "$person"

  # The answer to core's intake is held past Account Center's 5-second timeout.
  curl --silent --fail -X POST "http://hold-proxy:9091/harness/hold?seconds=12&prefix=/v1/account-deletion-requests/self" >/dev/null
  intakes_before=$(dc logs --no-color --no-log-prefix hold-proxy 2>/dev/null | grep -c 'POST /v1/account-deletion-requests/self' || true)
  ac_bff_submit
  curl --silent --fail -X POST "http://hold-proxy:9091/harness/hold?seconds=0" >/dev/null
  log "  Account Center submit answered $HTTP_STATUS -> $AC_SUBMIT_LOCATION"
  check 'Account Center lost the answer and sent the person to recovery' eq "$HTTP_STATUS $AC_SUBMIT_LOCATION" "303 $AC_URL/account-deletion?recovery=1"
  DEL_REQUEST=$(request_of "$subject")
  check 'core had accepted: durable request with the global block confirmed' \
    request_where "$DEL_REQUEST" 'platform_blocked_at IS NOT NULL'
  check 'the saga logged the person out' wait_until 60 2 step_done "$DEL_REQUEST" logout_sessions
  check 'the request completes' wait_until 120 2 request_is "$DEL_REQUEST" completed

  # The recovery page: Account Center replays the intake with the sealed credentials.
  ac_bff_status
  log "  recovery status answered $HTTP_STATUS $(jq -c 'del(.csrfToken)' <<<"$HTTP_BODY" 2>/dev/null)"
  check 'the recovery replay reads the accepted state (200 completed), not 401/409' eq "$HTTP_STATUS/$(jq -r .status <<<"$HTTP_BODY")" 200/completed
  check 'core saw the intake twice: the lost one and the replay' wait_until 30 2 intake_calls_at_least $((intakes_before + 2))
  check 'exactly one request for the person' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_requests WHERE subject_id = '$subject'")" 1
  ac_bff_status
  check 'a second status read still answers completed' eq "$HTTP_STATUS/$(jq -r .status <<<"$HTTP_BODY")" 200/completed
  scenario_end
}
