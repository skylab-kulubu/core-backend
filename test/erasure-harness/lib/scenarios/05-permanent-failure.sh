#!/usr/bin/env bash
# Scenario 5 — a permanent failure: core-erasure loses the CMS erase role. The request goes to
# manual_intervention at once with erase_cms_rejected_403, the watchdog gauge rises, the
# account_erasure_attention line is written and the alarm reaches mailpit. The role is given
# back and the retry path (the one Account Center's retry calls) completes the request.

erasure_sa_user() { kc GET "/clients/$(kc_client_uuid core-erasure)/service-account-user"; jq -r .id <<<"$HTTP_BODY"; }
role_body() { # role_body CLIENT_ID ROLE
  kc GET "/clients/$(kc_client_uuid "$1")/roles/$(jq -rn --arg r "$2" '$r | @uri')"
  jq -c '[{id, name}]' <<<"$HTTP_BODY"
}
sa_has_role() { # sa_has_role CLIENT_ID ROLE
  kc GET "/users/$(erasure_sa_user)/role-mappings/clients/$(kc_client_uuid "$1")"
  jq -e --arg r "$2" 'any(.[]; .name == $r)' <<<"$HTTP_BODY" >/dev/null
}

# restart_core: a fresh core process (empty token cache). The watchdog counts at start.
restart_core() {
  dc restart core >/dev/null 2>&1
  wait_until 120 2 service_ready http://core:8080/v1/ready
}

# erasure_alarm: the alarm's path in the harness. Like skylab-erasure-alarm it reads the four
# gauges from inside core's container over loopback (never through the edge) and, when
# overdue or manual intervention is above zero, mails the verified members of /ADMIN directly
# over SMTP, not through SkyMail. The real script needs the OpenBao rotator and is rehearsed by
# ops/wizards/erasure/erasure-alarm-rehearsal.sh; this stand-in proves gauge -> mail here.
erasure_alarm() {
  local metrics manual overdue open recipients members subject body
  metrics=$(docker exec "$(dc ps -q core)" wget -q -T 15 -O - http://127.0.0.1:8080/v1/metrics)
  manual=$(awk '$1 == "skylab_account_erasure_manual_intervention_requests" { print $2 }' <<<"$metrics")
  overdue=$(awk '$1 == "skylab_account_erasure_overdue_requests" { print $2 }' <<<"$metrics")
  open=$(awk '$1 == "skylab_account_erasure_open_requests" { print $2 }' <<<"$metrics")
  [[ -n $manual && -n $overdue ]] || { log "  alarm: gauges absent"; return 1; }
  ((manual > 0 || overdue > 0)) || { log "  alarm: nothing to report"; return 0; }
  kc GET "/groups?search=ADMIN&exact=true"
  members=$(jq -r '.[0].id' <<<"$HTTP_BODY")
  kc GET "/groups/$members/members?briefRepresentation=false"
  recipients=$(jq -r '.[] | select(.enabled and .emailVerified and (.email // "") != "") | .email' <<<"$HTTP_BODY")
  [[ -n $recipients ]] || { log "  alarm: no /ADMIN recipient"; return 1; }
  subject="SKY LAB hesap silme: $overdue gecikmiş, $manual elle müdahale bekleyen istek"
  body=$(printf 'Core gauge: açık=%s gecikmiş=%s elle-müdahale=%s\nKişi verisi yok.\n' "$open" "$overdue" "$manual")
  local rcpt args=()
  while IFS= read -r rcpt; do args+=(--mail-rcpt "$rcpt"); done <<<"$recipients"
  printf 'From: erasure-alarm@harness.invalid\r\nTo: %s\r\nSubject: %s\r\nX-Skylab-Rotator: erasure-alarm\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n' \
    "$(paste -sd, - <<<"$recipients")" "$subject" "$body" \
    | curl --silent --show-error --url smtp://mailpit:1025 --mail-from erasure-alarm@harness.invalid "${args[@]}" --upload-file -
}

mailpit_count() { # mailpit_count QUERY
  curl --silent "http://mailpit:8025/api/v1/search?query=$(jq -rn --arg q "$1" '$q | @uri')" | jq -r '.messages_count // .total // 0'
}

scenario_5() {
  local person=p3 subject role gauge_before gauge_after attention
  subject=${P_ID[$person]}
  scenario_begin S5 'permanent failure: CMS role removed, manual_intervention, gauge, attention log, alarm mail, retry completes'

  role=$(role_body skycms cms:account:erase)
  kc DELETE "/users/$(erasure_sa_user)/role-mappings/clients/$(kc_client_uuid skycms)" --data "$role"
  check 'cms:account:erase removed from service-account-core-erasure' eq "$HTTP_STATUS" 204
  check 'the service account no longer holds the CMS role' not sa_has_role skycms cms:account:erase
  # core caches each service token until 30 s before it expires; a fresh process takes a token
  # that reflects the role change at once (in production the cached token ages out in <= 4.5 min).
  restart_core
  gauge_before=$(metric skylab_account_erasure_manual_intervention_requests)

  check 'Account Center: login, Sudo mode, prepare, confirmation' ac_bff_delete "$person"
  check 'request goes to manual_intervention' wait_until 60 2 request_is "$DEL_REQUEST" manual_intervention
  check 'the stable code is erase_cms_rejected_403' eq "$(request_field "$DEL_REQUEST" last_error_code)" erase_cms_rejected_403
  log "  steps: $(steps_of "$DEL_REQUEST")"
  check 'SkyMail and Forms were checkpointed in the same pass' steps_done "$DEL_REQUEST" erase_skymail erase_forms
  check 'anonymize_core and delete_identity did not run' \
    eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST' AND step IN ('erase_cms','anonymize_core','delete_identity')")" 0
  ac_bff_status
  check 'Account Center status shows manual_intervention (200) with partial=true' \
    eq "$HTTP_STATUS/$(jq -r '.status + "/" + (.partial|tostring)' <<<"$HTTP_BODY")" 200/manual_intervention/true

  # The watchdog counts every five minutes and at start; a restart makes it count now.
  restart_core
  check 'watchdog gauge counts the request' wait_until 60 2 bash -c '[[ $(curl -s http://core:8080/v1/metrics | awk '"'"'$1 == "skylab_account_erasure_manual_intervention_requests" { print $2 }'"'"') -ge 1 ]]'
  gauge_after=$(metric skylab_account_erasure_manual_intervention_requests)
  check "manual intervention gauge rose ($gauge_before -> $gauge_after)" test "${gauge_after:-0}" -gt "${gauge_before:-0}"
  attention=$(dc logs --no-color --no-log-prefix core 2>/dev/null | grep "account_erasure_attention request_id=$DEL_REQUEST" | tail -n1 || true)
  log "  $attention"
  check 'core logged account_erasure_attention reason=manual_intervention for the request, without the subject' \
    bash -c '[[ "$1" == *"reason=manual_intervention"* && "$1" == *"code=erase_cms_rejected_403"* && "$1" != *"$2"* ]]' _ "$attention" "$subject"

  local mails_before
  mails_before=$(mailpit_count 'subject:"SKY LAB hesap silme"')
  check 'alarm read the gauges over loopback and mailed /ADMIN' erasure_alarm
  check 'alarm mail reached mailpit' wait_until 20 1 bash -c '[[ $(curl -s "http://mailpit:8025/api/v1/search?query=subject%3A%22SKY%20LAB%20hesap%20silme%22" | jq -r .messages_count) -gt '"$mails_before"' ]]'
  check 'alarm mail went to the /ADMIN member and names no person' \
    bash -c 'm=$(curl -s "http://mailpit:8025/api/v1/search?query=subject%3A%22SKY%20LAB%20hesap%20silme%22" | jq -r ".messages[0].ID"); t=$(curl -s "http://mailpit:8025/api/v1/message/$m"); jq -e ".To[0].Address == \"erasure-alarm-admin@harness.invalid\"" <<<"$t" >/dev/null && ! grep -qiF -e "$1" -e "$2" <<<"$t"' _ "${P_SCHOOL[$person]}" "$subject"

  # Operator fix: give the role back, then the retry button on Account Center's status page.
  kc POST "/users/$(erasure_sa_user)/role-mappings/clients/$(kc_client_uuid skycms)" --data "$role"
  check 'cms:account:erase given back' eq "$HTTP_STATUS" 204
  ac_bff_retry
  check "Account Center's retry answered 200 with the request pending or processing" \
    grep -Eqx '200/(pending|processing|completed)' <<<"$HTTP_STATUS/$(jq -r .status <<<"$HTTP_BODY")"
  if wait_until 30 2 request_is "$DEL_REQUEST" completed; then
    check 'request completes after the retry' true
  else
    # A token core fetched before the fix may still be cached (it lives until 30 s before exp).
    note "retry right after the fix did not complete: $(request_status "$DEL_REQUEST") $(request_field "$DEL_REQUEST" last_error_code) — core still held the CMS token it took before the role came back"
    restart_core
    ac_bff_retry
    check 'request completes after a retry with a fresh token' wait_until 60 2 request_is "$DEL_REQUEST" completed
  fi
  check 'all nine steps are checkpointed' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST'")" 9
  check 'retry kept SkyMail and Forms checkpoints (called once each)' \
    eq "$(pg skymail "SELECT count(*) FROM account_erasure_receipts WHERE request_id = '$DEL_REQUEST'")/$(curl -s "http://forms:9090/harness/stats/$DEL_REQUEST" | jq -r .puts)" 1/1
  check 'Keycloak user deleted (404)' eq "$(kc_user_status "$subject")" 404
  restart_core
  check 'manual intervention gauge back to its earlier value' \
    wait_until 60 2 bash -c '[[ $(curl -s http://core:8080/v1/metrics | awk '"'"'$1 == "skylab_account_erasure_manual_intervention_requests" { print $2 }'"'"') == '"${gauge_before:-0}"' ]]'
  scenario_end
}
