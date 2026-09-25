#!/usr/bin/env bash
# Scenario 7 — the nightly rotation window (ADR-0050): Keycloak's core-erasure secret changes
# first, core still holds the old one for a few seconds, and the token endpoint answers
# 401 invalid_client. The service steps defer (no attempt spent) and the request completes once
# core has the new secret, the way the rotator hands it over (new value in core's environment,
# core restarted).

token_status_with() { # token_status_with SECRET: HTTP status and error of a core-erasure token request
  local out
  out=$(hcurl --write-out $'\n%{http_code}' --data-urlencode grant_type=client_credentials \
    --data-urlencode client_id=core-erasure --data-urlencode "client_secret=$1" \
    --data-urlencode 'scope=openid account-erase-skymail' "$REALM_URL/protocol/openid-connect/token")
  printf '%s %s' "${out##*$'\n'}" "$(jq -r '.error // "ok"' <<<"${out%$'\n'*}" 2>/dev/null)"
}

scenario_7() {
  local person=p5 subject client old new window_start window_end
  subject=${P_ID[$person]}
  scenario_begin S7 'rotation window: token endpoint 401 invalid_client for a few seconds, request still completes'

  # A fresh core holds no cached service token, so the pass must ask the token endpoint.
  check 'core restarted with an empty token cache' restart_core
  client=$(kc_client_uuid core-erasure)
  old=$ACCOUNT_ERASURE_CLIENT_SECRET
  new=$(openssl rand -hex 24)
  kc PUT "/clients/$client" --data "$(jq -cn --arg s "$new" '{secret: $s}')"
  check 'Keycloak rotated the core-erasure secret (the rotator'"'"'s first half)' eq "$HTTP_STATUS" 204
  window_start=$(date +%s)
  check 'the old secret now gets 401 invalid_client' eq "$(token_status_with "$old")" '401 invalid_client'

  check 'Account Center: login, Sudo mode, prepare, confirmation inside the window' ac_bff_delete "$person"
  check 'the service steps defer on the token endpoint' \
    wait_until 60 2 request_where "$DEL_REQUEST" "last_error_code IN ('erase_skymail_failed','erase_cms_failed','erase_forms_failed') AND status = 'pending'"
  log "  request: $(pg super_skylab "SELECT status, attempt_count, last_error_code, next_attempt_at FROM account_deletion_requests WHERE id = '$DEL_REQUEST'" | tr '\t' ' ')"
  check 'no attempt spent' eq "$(request_field "$DEL_REQUEST" attempt_count)" 0
  check 'no service step and nothing after them ran' \
    eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST' AND step NOT IN ('disable_identity','logout_sessions')")" 0
  check 'core logged the token endpoint failure without a secret' \
    bash -c 'l=$(docker logs "$1" 2>&1 | grep -F "$2" | grep -i "token" | tail -n1); [[ -n $l && $l != *"$3"* && $l != *"$4"* ]]' _ "$(dc ps -q core)" "$DEL_REQUEST" "$old" "$new"

  # The rotator's second half: core's environment gets the new value and core restarts.
  set_env ACCOUNT_ERASURE_CLIENT_SECRET "$new"
  dc up -d --no-deps core >/dev/null 2>&1
  wait_until 120 2 service_ready http://core:8080/v1/ready
  window_end=$(date +%s)
  note "rotation window (Keycloak new secret -> core restarted with it): $((window_end - window_start)) s"
  check 'the new secret gets a token' eq "$(token_status_with "$new")" '200 ok'
  log "  waiting for the deferred attempt (up to 5 minutes)"
  check 'request completes after the window' wait_until 420 5 request_is "$DEL_REQUEST" completed
  check 'all nine steps are checkpointed' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST'")" 9
  check 'only the completing pass is counted (attempt_count 1)' eq "$(request_field "$DEL_REQUEST" attempt_count)" 1
  check 'Keycloak user deleted (404)' eq "$(kc_user_status "$subject")" 404
  scenario_end
}
