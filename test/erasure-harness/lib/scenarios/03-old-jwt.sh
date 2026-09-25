#!/usr/bin/env bash
# Scenario 3 — old JWTs: tokens for core, SkyMail, CMS and Forms and an Account Center session
# are taken before the deletion. Once the marker is written all five answer 401 (the Account
# Center session is dropped by its own access gate), while the anonymous routes stay 200.
# The deletion is started with core's intake directly, so nothing but the shared marker tells
# Account Center about it.

# old_token_status SERVICE TOKEN: the status of one ordinary authenticated read.
old_token_status() {
  case $1 in
    core) http GET "$CORE/v1/users/me" -H "Authorization: Bearer $2" ;;
    skymail) http GET "http://skymail:3000/v1/mail_tasks" -H "Authorization: Bearer $2" ;;
    cms) http GET "http://cms:5000/cms/collections/me" -H "Authorization: Bearer $2" ;;
    forms) http GET "http://forms:8080/api/me/responses" -H "Authorization: Bearer $2" ;;
  esac
  printf '%s' "$HTTP_STATUS"
}

anonymous_statuses() {
  local out=()
  http GET "$CORE/v1/health"; out+=("core=$HTTP_STATUS")
  http GET "$CORE/v1/events"; out+=("core-events=$HTTP_STATUS")
  http GET "http://skymail:3000/health"; out+=("skymail=$HTTP_STATUS")
  http GET "http://cms:5000/cms/collections/News/"; out+=("cms-news=$HTTP_STATUS")
  http GET "http://forms:8080/api/health"; out+=("forms=$HTTP_STATUS")
  http GET "$AC_URL/api/health"; out+=("account-center=$HTTP_STATUS")
  printf '%s ' "${out[@]}"
}

scenario_3() {
  local person=p7 subject service before after anonymous
  declare -A token
  subject=${P_ID[$person]}
  scenario_begin S3 'old JWTs: core, SkyMail, CMS and Forms tokens and the Account Center session all 401 once the marker is written; anonymous routes stay 200'

  for service in core skymail cms forms; do
    token[$service]=$(user_token "$person" "$service")
    before=$(old_token_status "$service" "${token[$service]}")
    check "$service accepts the person's token before the deletion (200)" eq "$before" 200
  done
  check 'Account Center session works before the deletion' ac_bff_login "$person"
  check 'Account Center identity read answers 200 before' ac_bff_session_ok
  local ac_jar=$AC_JAR
  anonymous=$(anonymous_statuses)
  log "  anonymous before: $anonymous"

  check 'core intake accepted (a second Account Center session proves sudo)' start_deletion "$person"
  check 'marker written' eq "$(marker_of "$subject")" 1
  # Right after the marker: the same tokens, still unexpired.
  for service in core skymail cms forms; do
    after=$(old_token_status "$service" "${token[$service]}")
    check "$service refuses the old token once the marker exists (401)" eq "$after" 401
  done
  AC_JAR=$ac_jar
  check 'the earlier Account Center session is refused (its access gate reads the marker)' not ac_bff_session_ok
  log "  Account Center answered $HTTP_STATUS"
  check 'request completes' wait_until 120 2 request_is "$DEL_REQUEST" completed
  for service in core skymail cms forms; do
    after=$(old_token_status "$service" "${token[$service]}")
    check "$service still refuses the old token after the erasure (401)" eq "$after" 401
  done
  anonymous=$(anonymous_statuses)
  log "  anonymous after: $anonymous"
  check 'anonymous routes still answer 2xx' bash -c '! grep -Eq "=[^2]" <<<"$1"' _ "$anonymous"
  check 'core decided "blocked" for the old token (gate decision event)' \
    bash -c 'docker logs "$1" 2>&1 | grep -q "\"blocked\""' _ "$(dc ps -q core)"
  scenario_end
}
