#!/usr/bin/env bash
# Scenario 8 — a deleted person signs in again with YTÜ (ticket 08's gate): p1, erased in
# scenario 1, comes back through the fake OBS identity provider. Keycloak opens a new account
# with a new sub; the old marker does not block it; nothing links it to the old data.

# obs_login PERSON: the browser sign-in through OBS (kc_idp_hint), the fake OBS's login form, the
# broker's first login, and the code exchange. Sets OBS_ACCESS.
OBS_ACCESS=''
obs_login() {
  local person=$1 jar headers verifier challenge page action status location code response
  jar=$(mktemp) headers=$(mktemp)
  verifier="obs-$person-$(openssl rand -hex 24)"
  challenge=$(printf '%s' "$verifier" | openssl dgst -binary -sha256 | openssl base64 -A | tr '+/' '-_' | tr -d '=')
  page=$(hcurl --location --cookie "$jar" --cookie-jar "$jar" \
    "$REALM_URL/protocol/openid-connect/auth?client_id=harness-browser&response_type=code&scope=openid&kc_idp_hint=OBS&redirect_uri=$(jq -rn '"https://harness.invalid/callback" | @uri')&code_challenge=$challenge&code_challenge_method=S256&state=obs-$person")
  action=$(login_page_action "$page")
  [[ $action == https://e.yildizskylab.com/realms/obs-fake/* ]] || { log "obs_login: not on the OBS login form ($action)"; rm -f "$jar" "$headers"; return 1; }
  # The fake OBS authenticates obs-p1 and sends the browser back to the broker; the broker's
  # first login creates the account and redirects to the client. Follow until the client URL.
  status=$(hcurl --output /dev/null --dump-header "$headers" --write-out '%{http_code}' --cookie "$jar" --cookie-jar "$jar" \
    --data-urlencode "username=obs-$person" --data-urlencode "password=$HARNESS_PERSON_PASSWORD" --data-urlencode credentialId= "$action")
  location=$(awk 'tolower($1) == "location:" { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print }' "$headers" | tail -n1)
  local hops=0
  while [[ $location == https://e.yildizskylab.com/* && $hops -lt 8 ]]; do
    hops=$((hops + 1))
    page=$(hcurl --dump-header "$headers" --cookie "$jar" --cookie-jar "$jar" "$location")
    location=$(awk 'tolower($1) == "location:" { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print }' "$headers" | tail -n1)
    if [[ -z $location ]]; then
      # A first-login page (review profile) would stop here; submit it unchanged.
      action=$(login_page_action "$page")
      [[ -n $action ]] || { log "obs_login: stuck on a page without a form"; break; }
      log "  first broker login shows a form; submitting it unchanged"
      hcurl --output /dev/null --dump-header "$headers" --cookie "$jar" --cookie-jar "$jar" \
        --data-urlencode "username=obs-$person" --data-urlencode "email=${P_SCHOOL[$person]}" \
        --data-urlencode "firstName=${P_FIRST[$person]}" --data-urlencode "lastName=${P_LAST[$person]}" "$action"
      location=$(awk 'tolower($1) == "location:" { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print }' "$headers" | tail -n1)
    fi
  done
  rm -f "$jar" "$headers"
  [[ $location == https://harness.invalid/callback* ]] || { log "obs_login: ended at ${location:-nowhere}"; return 1; }
  code=$(sed -n 's/.*[?&]code=\([^&]*\).*/\1/p' <<<"$location")
  response=$(hcurl --fail --data-urlencode grant_type=authorization_code --data-urlencode client_id=harness-browser \
    --data-urlencode "code=$code" --data-urlencode redirect_uri=https://harness.invalid/callback \
    --data-urlencode "code_verifier=$verifier" "$REALM_URL/protocol/openid-connect/token")
  OBS_ACCESS=$(jq -r .access_token <<<"$response")
  [[ -n $OBS_ACCESS && $OBS_ACCESS != null ]]
}

scenario_8() {
  local person=p1 old_sub new_sub
  old_sub=${P_ID[$person]}
  scenario_begin S8 'OBS re-login: a deleted person signs in with YTÜ again, gets a new sub, is not blocked, has no link to the old data'
  check 'p1 was erased (scenario 1 completed)' eq "$(pg super_skylab "SELECT status FROM account_deletion_requests WHERE subject_id = '$old_sub'")" completed
  check 'the old Keycloak user is gone' eq "$(kc_user_status "$old_sub")" 404

  check 'sign-in through the fake OBS IdP' obs_login "$person"
  new_sub=$(jwt_payload "$OBS_ACCESS" | jq -r .sub)
  log "  new sub: $new_sub"
  check 'Keycloak opened a new account with a new sub' test -n "$new_sub" -a "$new_sub" != "$old_sub"
  kc GET "/users/$new_sub/federated-identity"
  check 'the new account is linked to OBS' eq "$(jq -r '.[] | select(.identityProvider == "OBS") | .userId' <<<"$HTTP_BODY")" 0b500001-0000-4000-8000-000000000001
  kc GET "/users/$new_sub"
  check "the new account carries none of the old account's attributes" \
    eq "$(jq -r '[.attributes.personalEmail // [], .attributes.schoolEmail // []] | flatten | length' <<<"$HTTP_BODY")" 0
  check 'the old marker is still there' eq "$(marker_of "$old_sub")" 1
  check 'the new sub has no marker' eq "$(marker_of "$new_sub")" ''

  http GET "$CORE/v1/users/me" -H "Authorization: Bearer $OBS_ACCESS"
  check 'core accepts the new sub (200, not blocked by the old marker)' eq "$HTTP_STATUS" 200
  check 'core created a new, active user row for the new sub' \
    eq "$(pg super_skylab "SELECT account_state FROM users WHERE id = '$new_sub'")" active
  check 'core still keeps the old sub only as the anonymized tombstone with its completed request' \
    eq "$(pg super_skylab "SELECT u.account_state || '/' || r.status FROM users u JOIN account_deletion_requests r ON r.subject_id = u.id WHERE u.id = '$old_sub'")" anonymized/completed
  check 'no deletion request exists for the new sub' eq "$(pg super_skylab "SELECT count(*) FROM account_deletion_requests WHERE subject_id = '$new_sub'")" 0
  check 'no ticket or certificate of the old account points at the new sub' \
    eq "$(pg super_skylab "SELECT (SELECT count(*) FROM tickets WHERE owner_id = '$new_sub') + (SELECT count(*) FROM certificates WHERE owner_id = '$new_sub')")" 0
  check "the old account's records stay detached (owner NULL)" \
    eq "$(pg super_skylab "SELECT count(*) FROM tickets WHERE id = '$(uuid_of "$person-ticket-owned")' AND owner_id IS NULL")" 1
  local db
  for db in skymail skylab_cms forms_db; do
    check "$db holds nothing for the new sub" eq "$(count_lines "$(subject_hits "$db" "$new_sub")")" 0
  done
  printf '%s\n' "$new_sub" >"$STATE/s8.new-sub"
  scenario_end
}
