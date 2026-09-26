#!/usr/bin/env bash
# Brings the harness stack up in dependency order and provisions Keycloak the way an operator
# does: the realm import (clients, the fake OBS IdP, the test persons), bootstrap-reconciler.sh,
# reconcile-account-center.sh and create-erasure-client.sh from the candidate Keycloak image.
# Sourced by main.sh.

render_realms() {
  local users obs_users
  users=$(jq -Rn --arg password "$HARNESS_PERSON_PASSWORD" '
    [inputs | select(length > 0 and (startswith("#") | not)) | split("\t")
     | {key: .[0], id: .[1], username: .[2], firstName: .[3], lastName: .[4], school: .[5], personal: .[6]}
     | {id, username, enabled: true, emailVerified: true, firstName, lastName, email: .school,
        attributes: {schoolEmail: [.school], personalEmail: [.personal]},
        credentials: [{type: "password", value: $password, temporary: false}],
        clientRoles: {skymail: ["skymail:access", "skymail:mails:read"], "harness-cms": ["cms:access"]},
        federatedIdentities: (if .key == "p1"
          then [{identityProvider: "OBS", userId: "0b500001-0000-4000-8000-000000000001", userName: "obs-p1"}]
          else [] end)}]' <"$HARNESS_DIR/lib/persons.tsv")
  # The fake OBS knows p1 under a stable id: the same YTÜ person who signs in again after the
  # erasure (scenario 8).
  obs_users=$(jq -Rn --arg password "$HARNESS_PERSON_PASSWORD" '
    [inputs | select(startswith("p1\t")) | split("\t")
     | {id: "0b500001-0000-4000-8000-000000000001", username: "obs-p1", enabled: true, emailVerified: true,
        firstName: .[3], lastName: .[4], email: .[5],
        credentials: [{type: "password", value: $password, temporary: false}]}]' <"$HARNESS_DIR/lib/persons.tsv")
  mkdir -p "$STATE/realm"
  jq --argjson users "$users" --arg core "$CORE_CLIENT_SECRET" --arg skymail "$SKYMAIL_SERVICE_CLIENT_SECRET" \
    --arg obs "$OBS_BROKER_CLIENT_SECRET" '
    .users = $users
    | .clients |= map(if .secret == "__CORE_CLIENT_SECRET__" then .secret = $core
                      elif .secret == "__SKYMAIL_SERVICE_CLIENT_SECRET__" then .secret = $skymail else . end)
    | .identityProviders |= map(.config.clientSecret = $obs)
    | .users += [
        {username: "service-account-core", enabled: true, serviceAccountClientId: "core",
         clientRoles: {"realm-management": ["manage-users", "view-users", "query-users", "query-groups", "view-clients", "query-clients"]}},
        {username: "service-account-skymail-backend", enabled: true, serviceAccountClientId: "skymail-backend",
         clientRoles: {"realm-management": ["view-users", "view-clients", "query-groups", "query-users"]}},
        {username: "harness-admin", enabled: true, emailVerified: true, firstName: "Harness", lastName: "Admin",
         email: "erasure-alarm-admin@harness.invalid", groups: ["/ADMIN"]}
      ]' "$HARNESS_DIR/realm/e-skylab.json" >"$STATE/realm/e-skylab-realm.json"
  jq --argjson users "$obs_users" --arg obs "$OBS_BROKER_CLIENT_SECRET" \
    '.users = $users | .clients |= map(.secret = $obs)' \
    "$HARNESS_DIR/realm/obs-fake.json" >"$STATE/realm/obs-fake-realm.json"
  chmod 0644 "$STATE"/realm/*.json
}

# set_env NAME VALUE: rewrites one line of compose.env (the running shell sees it too).
set_env() {
  local tmp
  tmp=$(mktemp)
  grep -v "^$1=" "$STATE/compose.env" >"$tmp" || true
  printf '%s=%s\n' "$1" "$2" >>"$tmp"
  cat "$tmp" >"$STATE/compose.env"
  rm -f "$tmp"
  export "$1=$2"
}

keycloak_ready() {
  hcurl --fail --output /dev/null "$REALM_URL/.well-known/openid-configuration" 2>/dev/null
}

keycloak_tool() { # keycloak_tool SCRIPT [args]: an operator script from the candidate image
  local script=$1
  shift
  dc run --rm --no-deps \
    -e KEYCLOAK_ADMIN_URL=http://keycloak:8080 \
    -e KEYCLOAK_ADMIN_REALM=master \
    -e KEYCLOAK_REALM=e-skylab \
    -e KEYCLOAK_BOOTSTRAP_ADMIN_USERNAME=admin \
    -e "KEYCLOAK_BOOTSTRAP_ADMIN_PASSWORD=$KC_ADMIN_PASSWORD" \
    -e KEYCLOAK_CONFIG_CLIENT_ID=account-center-config \
    -e "KEYCLOAK_CONFIG_CLIENT_SECRET=$KC_CONFIG_CLIENT_SECRET" \
    -e ACCOUNT_CENTER_BASE_URL=https://my.yildizskylab.com \
    -e KEYCLOAK_ERASURE_ADMIN_USERNAME=admin \
    -e "KEYCLOAK_ERASURE_ADMIN_PASSWORD=$KC_ADMIN_PASSWORD" \
    -e SKY_HARNESS=1 \
    --entrypoint "/opt/keycloak/config/$script" keycloak-config "$@"
}

provision_infrastructure() {
  log "starting postgres, the account-access Redis, the CMS Redis, mailpit and the edge"
  dc up -d --wait postgres
  dc up -d account-access-redis cms-redis mailpit edge
  render_realms
  log "starting Keycloak (realm import)"
  dc up -d keycloak
  wait_until 240 3 keycloak_ready || { log "Keycloak did not become ready"; dc logs --tail 80 keycloak >&2; return 1; }
  log "Keycloak is ready"
}

provision_keycloak() {
  local out
  log "bootstrap-reconciler.sh"
  keycloak_tool bootstrap-reconciler.sh >"$EVIDENCE/keycloak-bootstrap.log" 2>&1 \
    || { cat "$EVIDENCE/keycloak-bootstrap.log" >&2; return 1; }
  log "reconcile-account-center.sh"
  keycloak_tool reconcile-account-center.sh >"$EVIDENCE/keycloak-reconcile.log" 2>&1 \
    || { cat "$EVIDENCE/keycloak-reconcile.log" >&2; return 1; }
  log "create-erasure-client.sh (dry run, then --apply)"
  keycloak_tool create-erasure-client.sh --admin-user admin >"$EVIDENCE/erasure-client-dry-run.log" 2>&1 \
    || { cat "$EVIDENCE/erasure-client-dry-run.log" >&2; return 1; }
  keycloak_tool create-erasure-client.sh --admin-user admin --apply >"$EVIDENCE/erasure-client-apply.log" 2>&1 \
    || { cat "$EVIDENCE/erasure-client-apply.log" >&2; return 1; }
  out=$(kc_client_secret core-erasure)
  [[ -n $out && $out != null ]] || { log "core-erasure has no secret"; return 1; }
  set_env ACCOUNT_ERASURE_CLIENT_SECRET "$out"
  out=$(kc_client_secret account-center)
  [[ -n $out && $out != null ]] || { log "account-center has no secret"; return 1; }
  set_env ACCOUNT_CENTER_OIDC_CLIENT_SECRET "$out"
  log "Keycloak provisioned: core-erasure and account-center exist"
}

service_ready() { # service_ready URL
  hcurl --fail --output /dev/null "$1" 2>/dev/null
}

provision_services() {
  log "starting core, SkyMail, CMS, the Forms stub and the hold proxy"
  dc up -d core skymail cms forms hold-proxy
  wait_until 180 3 service_ready http://core:8080/v1/ready || { dc logs --tail 60 core >&2; return 1; }
  wait_until 180 3 service_ready http://skymail:3000/ready || { dc logs --tail 60 skymail >&2; return 1; }
  wait_until 180 3 service_ready http://cms:5000/health/ready || { dc logs --tail 60 cms >&2; return 1; }
  wait_until 60 2 service_ready http://forms:8080/api/health || { dc logs --tail 60 forms >&2; return 1; }
  log "services ready"
}

provision_account_center() {
  log "Account Center: migrations, then the server"
  dc --profile account-center run --rm --no-deps --entrypoint node account-center scripts/migrate.mjs \
    >"$EVIDENCE/account-center-migrate.log" 2>&1 || { cat "$EVIDENCE/account-center-migrate.log" >&2; return 1; }
  dc --profile account-center up -d account-center
  wait_until 180 3 service_ready https://my.yildizskylab.com/api/ready || { dc logs --tail 60 account-center >&2; return 1; }
  log "Account Center ready"
}
