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

scope_role_mapping() { # scope_role_mapping ADD|REMOVE: the CMS erase role in scope account-erase-cms
  local scope method
  scope=$(kc GET /client-scopes; jq -r '.[] | select(.name == "account-erase-cms") | .id' <<<"$HTTP_BODY")
  [[ $1 == ADD ]] && method=POST || method=DELETE
  kc "$method" "/client-scopes/$scope/scope-mappings/clients/$(kc_client_uuid skycms)" --data "$(role_body skycms cms:account:erase)"
  [[ $HTTP_STATUS == 204 ]]
}

gauge_at_least() { # gauge_at_least N: the manual intervention gauge on /v1/metrics
  [[ $(metric skylab_account_erasure_manual_intervention_requests) -ge $1 ]] 2>/dev/null
}

attention_line() { # attention_line REQUEST: core's account_erasure_attention line for it
  dc logs --no-color --no-log-prefix core 2>/dev/null | grep "account_erasure_attention request_id=$1" | tail -n1 || true
}

alarm_mails() { curl -s "http://mailpit:8025/api/v1/search?query=subject%3A%22SKY%20LAB%20hesap%20silme%22" | jq -r '.messages_count // 0'; }

# check_alarm_path REQUEST CODE: the gauge counts it (after a restart, the watchdog counts at
# start), core writes one attention line, and the alarm reaches /ADMIN in mailpit.
check_alarm_path() {
  local request=$1 code=$2 before attention mails_before message
  before=$(metric skylab_account_erasure_manual_intervention_requests)
  restart_core
  check "watchdog gauge counts the request (was ${before:-absent})" wait_until 60 2 gauge_at_least 1
  attention=$(attention_line "$request")
  log "  ${attention:-no attention line}"
  check "core logged account_erasure_attention reason=manual_intervention code=$code, without the subject" \
    bash -c '[[ "$1" == *"reason=manual_intervention"* && "$1" == *"code=$2"* && "$1" != *"$3"* ]]' _ "$attention" "$code" "$4"
  mails_before=$(alarm_mails)
  check 'alarm read the gauges over loopback and mailed /ADMIN over SMTP' erasure_alarm
  check 'alarm mail reached mailpit' wait_until 20 1 bash -c '[[ $(curl -s "http://mailpit:8025/api/v1/search?query=subject%3A%22SKY%20LAB%20hesap%20silme%22" | jq -r ".messages_count // 0") -gt $1 ]]' _ "$mails_before"
  message=$(curl -s "http://mailpit:8025/api/v1/message/$(curl -s "http://mailpit:8025/api/v1/search?query=subject%3A%22SKY%20LAB%20hesap%20silme%22" | jq -r '.messages[0].ID')")
  check 'alarm mail went to the /ADMIN member only' eq "$(jq -r '[.To[].Address] | join(",")' <<<"$message")" erasure-alarm-admin@harness.invalid
  check 'alarm mail names no person and no subject' not grep -qiF -e "$3" -e "$4" <<<"$message"
}

# retry_until_completed REQUEST: Account Center's retry button, then the request completes.
retry_until_completed() {
  ac_bff_retry
  log "  Account Center retry answered $HTTP_STATUS $(jq -c 'del(.csrfToken)' <<<"$HTTP_BODY" 2>/dev/null)"
  check "Account Center's retry answered 200 with the request back in the queue" \
    grep -Eqx '200/(pending|processing|completed)' <<<"$HTTP_STATUS/$(jq -r .status <<<"$HTTP_BODY" 2>/dev/null)"
  check 'request completes after the retry' wait_until 90 2 request_is "$1" completed
  ac_bff_status
  check "Account Center's status page shows the completed request ($(jq -r '.status + " updatedAt=" + .updatedAt' <<<"$HTTP_BODY" 2>/dev/null))" \
    eq "$HTTP_STATUS/$(jq -r .status <<<"$HTTP_BODY" 2>/dev/null)" 200/completed
  check 'all nine steps are checkpointed' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$1'")" 9
  check 'the retry did not call SkyMail or Forms again (one receipt, one Forms PUT)' \
    eq "$(pg skymail "SELECT count(*) FROM account_erasure_receipts WHERE request_id = '$1'")/$(curl -s "http://forms:9090/harness/stats/$1" | jq -r .puts)" 1/1
}

scenario_5() {
  local subject gauge_before
  scenario_begin S5 'permanent failure: a CMS erase role taken away, manual_intervention, gauge, attention log, alarm mail, Account Center retry completes'

  # 5a — the erase scope loses its role mapping: the token still names skycms but carries no
  # role, CMS answers 403, the request goes to manual intervention at once.
  subject=${P_ID[p9]}
  check 'role mapping removed from scope account-erase-cms' scope_role_mapping REMOVE
  restart_core  # core caches each service token until 30 s before it expires
  check 'p9: Account Center login, Sudo mode, prepare, confirmation' ac_bff_delete p9
  check 'p9: request goes to manual_intervention at once' wait_until 60 2 request_is "$DEL_REQUEST" manual_intervention
  check 'p9: the stable code is erase_cms_rejected_403' eq "$(request_field "$DEL_REQUEST" last_error_code)" erase_cms_rejected_403
  check 'p9: one attempt spent' eq "$(request_field "$DEL_REQUEST" attempt_count)" 1
  check 'p9: SkyMail and Forms were checkpointed in the same pass' steps_done "$DEL_REQUEST" erase_skymail erase_forms
  check 'p9: nothing after the service steps ran' \
    eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST' AND step IN ('erase_cms','anonymize_core','delete_identity')")" 0
  ac_bff_status
  check 'p9: Account Center status shows manual_intervention with partial=true' \
    eq "$HTTP_STATUS/$(jq -r '.status + "/" + (.partial|tostring)' <<<"$HTTP_BODY")" 200/manual_intervention/true
  check_alarm_path "$DEL_REQUEST" erase_cms_rejected_403 "${P_SCHOOL[p9]}" "$subject"
  check 'role mapping given back to scope account-erase-cms' scope_role_mapping ADD
  restart_core
  retry_until_completed "$DEL_REQUEST"
  check 'p9: Keycloak user deleted (404)' eq "$(kc_user_status "$subject")" 404

  # 5b — the service account loses the role. Keycloak then leaves the whole optional scope out
  # of the token (a scope with role mappings applies only to holders of one of its roles): no
  # skycms audience, no role. CMS answers 401, which core treats as a refused token (ordinary
  # retry, attempts spent), not as a permanent 403.
  subject=${P_ID[p3]}
  local role
  role=$(role_body skycms cms:account:erase)
  kc DELETE "/users/$(erasure_sa_user)/role-mappings/clients/$(kc_client_uuid skycms)" --data "$role"
  check 'cms:account:erase removed from service-account-core-erasure' eq "$HTTP_STATUS" 204
  check 'the service account no longer holds the CMS role' not sa_has_role skycms cms:account:erase
  restart_core
  check 'p3: Account Center login, Sudo mode, prepare, confirmation' ac_bff_delete p3
  check 'p3: the first pass fails on CMS' wait_until 60 2 request_where "$DEL_REQUEST" "last_error_code <> ''"
  note "p3: with the role taken from the service account the code is '$(request_field "$DEL_REQUEST" last_error_code)' (CMS 401: the token lost the scope's audience), not erase_cms_rejected_403; the request spends the ordinary budget before manual intervention"
  log "  waiting for the ordinary retry budget (8 attempts, 30 s apart)"
  check 'p3: request reaches manual_intervention once the budget is spent' wait_until 420 5 request_is "$DEL_REQUEST" manual_intervention
  log "  p3 request: $(pg super_skylab "SELECT status, attempt_count, last_error_code FROM account_deletion_requests WHERE id = '$DEL_REQUEST'" | tr '\t' ' ')"
  check 'p3: all 8 attempts were spent' eq "$(request_field "$DEL_REQUEST" attempt_count)" 8
  check_alarm_path "$DEL_REQUEST" "$(request_field "$DEL_REQUEST" last_error_code)" "${P_SCHOOL[p3]}" "$subject"
  kc POST "/users/$(erasure_sa_user)/role-mappings/clients/$(kc_client_uuid skycms)" --data "$role"
  check 'cms:account:erase given back to the service account' eq "$HTTP_STATUS" 204
  retry_until_completed "$DEL_REQUEST"
  check 'p3: Keycloak user deleted (404)' eq "$(kc_user_status "$subject")" 404

  restart_core
  check 'manual intervention gauge back to 0' wait_until 60 2 bash -c '[[ $(curl -s http://core:8080/v1/metrics | awk '"'"'$1 == "skylab_account_erasure_manual_intervention_requests" { print $2 }'"'"') == 0 ]]'
  scenario_end
}
