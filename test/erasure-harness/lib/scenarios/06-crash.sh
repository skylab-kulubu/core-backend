#!/usr/bin/env bash
# Scenario 6 — core dies during a service call. SkyMail's answer is held by the proxy after
# SkyMail committed its erasure; core is killed (SIGKILL) before it sees the answer. After the
# 5-minute lease expires the restarted core sends the same command again and SkyMail answers
# from its receipt: the erasure ran once.

hold_proxy() { # hold_proxy SECONDS: hold only the answers of the erase route
  curl --silent --fail -X POST "http://hold-proxy:9091/harness/hold?seconds=$1&prefix=/internal/v1/account-erasures/" >/dev/null
}
skymail_receipt_exists() { [[ $(pg skymail "SELECT count(*) FROM account_erasure_receipts WHERE request_id = '$1'") == 1 ]]; }

# core_with_urls SKYMAIL CMS FORMS: recreate core with other service URLs (same env otherwise).
core_with_urls() {
  save_logs core
  HARNESS_SKYMAIL_URL=$1 HARNESS_CMS_URL=$2 HARNESS_FORMS_URL=$3 dc up -d --no-deps core >/dev/null 2>&1
  wait_until 120 2 service_ready http://core:8080/v1/ready
}

scenario_6() {
  local person=p4 subject core_container first_counts puts_skymail completed_lines
  subject=${P_ID[$person]}
  scenario_begin S6 'crash: core killed during the SkyMail call, lease expires, command resent, SkyMail answers from its receipt'

  check 'core now reaches the services through the hold proxy' core_with_urls http://hold-proxy:9001 http://hold-proxy:9002 http://hold-proxy:9003
  hold_proxy 20
  check 'Account Center: login, Sudo mode, prepare, confirmation' ac_bff_delete "$person"
  check 'SkyMail committed its erasure while the proxy holds the answer' wait_until 30 1 skymail_receipt_exists "$DEL_REQUEST"
  core_container=$(dc ps -q core)
  docker kill --signal KILL "$core_container" >/dev/null
  check 'core was killed during the call' eq "$(docker inspect -f '{{.State.Running}}' "$core_container")" false
  hold_proxy 0
  first_counts=$(pg skymail "SELECT counts::text FROM account_erasure_receipts WHERE request_id = '$DEL_REQUEST'")
  check 'no service step was checkpointed by the killed core' \
    eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST' AND step LIKE 'erase_%'")" 0
  check 'the request is still leased (processing) by the dead worker' \
    request_where "$DEL_REQUEST" "status = 'processing' AND lease_until > now()"
  log "  lease until $(request_field "$DEL_REQUEST" lease_until); restarting core"
  dc start core >/dev/null 2>&1
  wait_until 120 2 service_ready http://core:8080/v1/ready
  check 'the restarted core does not take the request before the lease expires' \
    request_where "$DEL_REQUEST" "status = 'processing' AND lease_until > now()"
  log "  waiting for the lease to expire and the request to complete (up to 7 minutes)"
  check 'request completes after the lease expired' wait_until 450 5 request_is "$DEL_REQUEST" completed
  check 'all nine steps are checkpointed' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST'")" 9
  puts_skymail=$(dc logs --no-color --no-log-prefix hold-proxy 2>/dev/null | grep -c "PUT /internal/v1/account-erasures/$DEL_REQUEST 200" || true)
  check "SkyMail got the command twice through the proxy ($puts_skymail)" test "$puts_skymail" -ge 2
  completed_lines=$(dc logs --no-color --no-log-prefix skymail 2>/dev/null | grep -F "$DEL_REQUEST" | grep -c 'account erasure completed' || true)
  check "SkyMail erased once: one 'account erasure completed' line ($completed_lines)" eq "$completed_lines" 1
  check 'SkyMail keeps one receipt with the first counts' \
    eq "$(pg skymail "SELECT count(*) || ' ' || max(counts::text) FROM account_erasure_receipts WHERE request_id = '$DEL_REQUEST'")" "1 $first_counts"
  check "core's checkpoint holds SkyMail's first counts" \
    eq "$(pg super_skylab "SELECT counts::jsonb::text FROM account_deletion_steps WHERE request_id = '$DEL_REQUEST' AND step = 'erase_skymail'")" \
    "$(pg skymail "SELECT counts::jsonb::text FROM account_erasure_receipts WHERE request_id = '$DEL_REQUEST'")"
  check 'Forms ran its erasure once' eq "$(curl -s "http://forms:9090/harness/stats/$DEL_REQUEST" | jq -r .erase_runs)" 1
  check 'Keycloak user deleted (404)' eq "$(kc_user_status "$subject")" 404
  check 'core back on the direct service URLs' core_with_urls http://skymail:3000 http://cms:5000 http://forms:8080
  scenario_end
}
