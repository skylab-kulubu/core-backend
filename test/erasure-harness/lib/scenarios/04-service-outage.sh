#!/usr/bin/env bash
# Scenario 4 — a service is down (audit gate 5): with SkyMail stopped the request takes the
# deferred retry without spending an attempt, anonymize_core and delete_identity do not run, and
# the request completes once SkyMail is back.

scenario_4() {
  local person=p2 subject deferred_at next_at attempts
  subject=${P_ID[$person]}
  scenario_begin S4 'service outage: SkyMail down, deferred retry, nothing after the service steps, completes when SkyMail is back'

  dc stop skymail >/dev/null 2>&1
  check 'SkyMail is stopped' eq "$(dc ps --status running --services 2>/dev/null | grep -cx skymail)" 0
  check 'intake accepted with SkyMail down' start_deletion "$person"
  check 'the first pass defers on erase_skymail' \
    wait_until 60 2 request_where "$DEL_REQUEST" "last_error_code = 'erase_skymail_failed'"
  deferred_at=$(pg super_skylab "SELECT extract(epoch FROM next_attempt_at)::bigint FROM account_deletion_requests WHERE id = '$DEL_REQUEST'")
  log "  request after the first pass: $(pg super_skylab "SELECT status, attempt_count, last_error_code, next_attempt_at FROM account_deletion_requests WHERE id = '$DEL_REQUEST'" | tr '\t' ' ')"
  log "  steps: $(steps_of "$DEL_REQUEST")"
  attempts=$(request_field "$DEL_REQUEST" attempt_count)
  check 'deferred retry spends no attempt (attempt_count stays 0)' eq "$attempts" 0
  next_at=$(pg super_skylab "SELECT extract(epoch FROM next_attempt_at)::bigint - extract(epoch FROM now())::bigint FROM account_deletion_requests WHERE id = '$DEL_REQUEST'")
  check "next attempt is deferred by about 5 minutes (in ${next_at}s)" test "$next_at" -gt 200 -a "$next_at" -le 305
  check 'disable_identity and logout_sessions are checkpointed' steps_done "$DEL_REQUEST" disable_identity logout_sessions
  check 'CMS and Forms were still tried in the same pass and checkpointed' steps_done "$DEL_REQUEST" erase_cms erase_forms
  check 'erase_skymail, anonymize_core and delete_identity did not run' \
    eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST' AND step IN ('erase_skymail','anonymize_core','erase_profile_media','erase_staged_uploads','delete_identity')")" 0
  check 'core still holds the person (not anonymized) while SkyMail is down' \
    eq "$(pg super_skylab "SELECT account_state FROM users WHERE id = '$subject'")" deletion_pending
  check 'Keycloak user still exists, disabled' \
    eq "$(kc GET "/users/$subject"; jq -r '.enabled' <<<"$HTTP_BODY")" false
  core_status "$DEL_RECEIPT"
  local coarse
  coarse=$(jq -r '.status + "/" + (.partial|tostring)' <<<"$HTTP_BODY")
  check "receipt status reports pending or processing with partial=true ($coarse)" grep -Eqx '(pending|processing)/true' <<<"$coarse"
  if [[ $(jq -r .updatedAt <<<"$HTTP_BODY") > $(date -u +%FT%TZ) ]]; then
    note "status updatedAt is in the future ($(jq -r .updatedAt <<<"$HTTP_BODY")): core writes the next attempt time into updated_at when it defers"
  fi

  # A second deferral while SkyMail is still down keeps the budget untouched: wait for it.
  log "  SkyMail stays down; waiting for the deferred attempt (about 5 minutes)"
  check 'the deferred attempt ran again and deferred again, still with no attempt spent' \
    wait_until 420 5 request_where "$DEL_REQUEST" "status = 'pending' AND attempt_count = 0 AND next_attempt_at > to_timestamp($deferred_at + 60)"

  dc start skymail >/dev/null 2>&1
  check 'SkyMail is back' wait_until 120 3 service_ready http://skymail:3000/ready
  log "  waiting for the next deferred attempt (up to 5 minutes)"
  check 'request completes once SkyMail is back' wait_until 420 5 request_is "$DEL_REQUEST" completed
  check 'all nine steps are checkpointed' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST'")" 9
  check 'SkyMail receipt written' eq "$(pg skymail "SELECT count(*) FROM account_erasure_receipts WHERE request_id = '$DEL_REQUEST'")" 1
  # Every claim counts one attempt and a deferral refunds it: after two deferred passes and the
  # completing one, only the completing pass is left on the counter.
  check 'only the completing pass is counted (attempt_count 1 after three passes)' eq "$(request_field "$DEL_REQUEST" attempt_count)" 1
  check 'Keycloak user deleted (404)' eq "$(kc_user_status "$subject")" 404
  local hits
  hits=$(pii_hits skymail "$person")
  check "skymail: no row names p2 ($(count_lines "$hits") hits)" test -z "$hits"
  scenario_end
}
