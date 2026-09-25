#!/usr/bin/env bash
# Scenario 2 — a lost answer (audit gate 7): core accepts the intake, the answer never reaches
# the caller, the saga closes the Keycloak session the sudo token is bound to, and the retry
# with the same idempotency key and the same sealed bearer and sudo token gets the accepted
# state back (202/200), not 401.
#
# The caller here is the harness speaking Account Center's exact intake request (the bearer
# and sudo token Account Center seals in its intent). Account Center's own recovery page is
# covered by scenario 2b when its BFF can be driven.

introspect_active() { # introspect_active TOKEN: "true"/"false" as the realm tells core
  hcurl --user "core:$CORE_CLIENT_SECRET" --data-urlencode "token=$1" \
    "$REALM_URL/protocol/openid-connect/token/introspect" | jq -r .active
}

scenario_2() {
  local person=p6 subject access sudo key replay_receipt
  subject=${P_ID[$person]}
  scenario_begin S2 'lost answer: accepted, answer dropped, saga closes the session, same key replays 202/200 (not 401)'

  check 'Account Center session and sudo proof' ac_login "$person"
  access=$AC_ACCESS
  sudo=$(sudo_token "$access")
  key=$(new_idempotency_key)
  check 'the sudo token is active before the intake' eq "$(introspect_active "$sudo")" true

  # The intake goes out; its answer is dropped (the receipt is never read).
  core_self_delete "$access" "$sudo" "$key"
  HTTP_BODY='' HTTP_STATUS=''
  check 'core accepted: durable request with the global block confirmed' \
    bash -c '[[ $(PGPASSWORD=$1 psql -h postgres -U postgres -d super_skylab -XAt -c "SELECT platform_blocked_at IS NOT NULL FROM account_deletion_requests WHERE subject_id = '"'"'$2'"'"'") == t ]]' _ "$POSTGRES_PASSWORD" "$subject"
  DEL_REQUEST=$(request_of "$subject")
  check 'the saga logged the person out' wait_until 60 2 step_done "$DEL_REQUEST" logout_sessions
  check 'the sudo token is now inactive at the realm (its session is gone)' eq "$(introspect_active "$sudo")" false

  core_self_delete "$access" "$sudo" "$key"
  log "  replay answered $HTTP_STATUS $(jq -c '{status, partial, platformBlocked}' <<<"$HTTP_BODY" 2>/dev/null)"
  check 'replay with the same key, bearer and sudo token answers 202 or 200' grep -Eqx '200|202' <<<"$HTTP_STATUS"
  replay_receipt=$(jq -r .receipt <<<"$HTTP_BODY" 2>/dev/null)
  core_status "$replay_receipt"
  check 'the replayed receipt reads the same request' eq "$HTTP_STATUS" 200
  core_self_delete "$access" "$sudo" "$(new_idempotency_key)"
  check 'a new key needs a live proof: 401 with the dead sudo token' eq "$HTTP_STATUS" 401
  check 'the request completes' wait_until 120 2 request_is "$DEL_REQUEST" completed
  core_self_delete "$access" "$sudo" "$key"
  check 'replay after completion answers 200 completed' eq "$HTTP_STATUS/$(jq -r .status <<<"$HTTP_BODY" 2>/dev/null)" 200/completed
  check 'exactly one request for the person' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_requests WHERE subject_id = '$subject'")" 1
  scenario_end
}
