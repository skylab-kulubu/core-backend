#!/usr/bin/env bash
# Creates the throwaway state of one harness run in .state/: a private CA and every TLS
# certificate (edge, account-access Redis server, one client certificate per Redis user), the
# Redis ACL file, and compose.env with generated passwords and keys. Nothing here is a real
# secret; everything is generated fresh and removed by down.sh. Runs inside the driver image.
set -Eeuo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
STATE=${HARNESS_STATE_DIR:-$HERE/.state}
PKI=$STATE/pki

if [[ -f $STATE/compose.env && ${1:-} != --force ]]; then
  printf '[init] state already present in %s\n' "$STATE"
  exit 0
fi

rm -rf "$PKI" "$STATE/realm" "$STATE/compose.env"
mkdir -p "$PKI/redis-server" "$PKI/clients" "$STATE/realm" "$STATE/evidence"

rand_hex() { openssl rand -hex "${1:-24}"; }
rand_b64url() { openssl rand "$1" | base64 -w0 | tr '+/' '-_' | tr -d '='; }

# --- certificates -------------------------------------------------------------------------------
openssl req -x509 -newkey rsa:2048 -nodes -days 30 -sha256 \
  -subj '/CN=SKY LAB erasure harness CA' \
  -keyout "$PKI/ca.key" -out "$PKI/ca.crt" \
  -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' 2>/dev/null

issue() { # issue NAME CN SAN_LIST USAGE
  local name=$1 cn=$2 san=$3 usage=$4
  openssl req -new -newkey rsa:2048 -nodes -subj "/CN=$cn" \
    -keyout "$PKI/$name.key" -out "$PKI/$name.csr" 2>/dev/null
  {
    printf 'basicConstraints=CA:FALSE\n'
    printf 'keyUsage=critical,digitalSignature,keyEncipherment\n'
    printf 'extendedKeyUsage=%s\n' "$usage"
    [[ -z $san ]] || printf 'subjectAltName=%s\n' "$san"
  } >"$PKI/$name.ext"
  openssl x509 -req -in "$PKI/$name.csr" -CA "$PKI/ca.crt" -CAkey "$PKI/ca.key" -CAcreateserial \
    -days 30 -sha256 -extfile "$PKI/$name.ext" -out "$PKI/$name.crt" 2>/dev/null
  rm -f "$PKI/$name.csr" "$PKI/$name.ext"
}

# The edge terminates TLS for the public names the services insist on (the v1 access-gate
# issuer is exactly https://e.yildizskylab.com/realms/e-skylab).
issue edge e.yildizskylab.com 'DNS:e.yildizskylab.com,DNS:my.yildizskylab.com,DNS:api.yildizskylab.com' serverAuth
cat "$PKI/edge.crt" "$PKI/edge.key" >"$PKI/edge.pem"

# mailpit offers STARTTLS: SkyMail's sender insists on it (TLSMandatory).
issue mailpit mailpit 'DNS:mailpit' serverAuth

issue redis-server account-access-redis 'DNS:account-access-redis' serverAuth,clientAuth
cp "$PKI/redis-server.crt" "$PKI/redis-server/account-access-tls.crt"
cp "$PKI/redis-server.key" "$PKI/redis-server/account-access-tls.key"
cp "$PKI/ca.crt" "$PKI/redis-server/account-access-ca.crt"

REDIS_USERS=(core-writer skymail-reader cms-reader forms-reader account-center-reader recovery-operator)
declare -A REDIS_PASSWORD
for user in "${REDIS_USERS[@]}"; do
  issue "clients/$user" "$user" '' clientAuth
  REDIS_PASSWORD[$user]=$(rand_hex 24)
done

# The ACL of deploy/account-access-redis/README.md: the writer gets GET, MGET, SET, PTTL, WAIT,
# PING, HELLO and connection commands; readers GET, MGET and connection commands, read-only on
# the namespace; the recovery operator (harness checks only) also counts and scans.
connection='+@connection -@admin -@dangerous'
{
  printf 'user default off\n'
  printf 'user core-writer on >%s resetkeys ~skylab:account-access:v1:* resetchannels -@all %s +get +mget +set +pttl +wait +ping +hello\n' \
    "${REDIS_PASSWORD[core-writer]}" "$connection"
  for reader in skymail-reader cms-reader forms-reader account-center-reader; do
    printf 'user %s on >%s resetkeys %%R~skylab:account-access:v1:* resetchannels -@all %s +get +mget +ping +hello\n' \
      "$reader" "${REDIS_PASSWORD[$reader]}" "$connection"
  done
  printf 'user recovery-operator on >%s resetkeys %%R~skylab:account-access:v1:* resetchannels -@all %s +get +mget +ping +hello +scan +dbsize +pttl +ttl +exists +info +config|get\n' \
    "${REDIS_PASSWORD[recovery-operator]}" "$connection"
} >"$PKI/redis-server/account-access-users.acl"

# Every file is world-readable on purpose: the services run as different non-root users and
# the material is thrown away with the run.
find "$PKI" -type d -exec chmod 0755 {} +
find "$PKI" -type f -exec chmod 0644 {} +

# --- passwords and keys -------------------------------------------------------------------------
{
  printf 'HARNESS_DIR=%s\n' "$HERE"
  printf 'HARNESS_STATE=%s\n' "$STATE"
  printf 'POSTGRES_PASSWORD=%s\n' "$(rand_hex 16)"
  printf 'KEYCLOAK_DB_PASSWORD=%s\n' "$(rand_hex 16)"
  printf 'CORE_DB_PASSWORD=%s\n' "$(rand_hex 16)"
  printf 'SKYMAIL_DB_PASSWORD=%s\n' "$(rand_hex 16)"
  printf 'CMS_DB_PASSWORD=%s\n' "$(rand_hex 16)"
  printf 'FORMS_DB_PASSWORD=%s\n' "$(rand_hex 16)"
  printf 'ACCOUNT_CENTER_DB_PASSWORD=%s\n' "$(rand_hex 16)"
  printf 'KC_ADMIN_PASSWORD=%s\n' "$(rand_hex 16)"
  printf 'KC_CONFIG_CLIENT_SECRET=%s\n' "$(rand_hex 24)"
  printf 'CORE_CLIENT_SECRET=%s\n' "$(rand_hex 24)"
  printf 'SKYMAIL_SERVICE_CLIENT_SECRET=%s\n' "$(rand_hex 24)"
  printf 'OBS_BROKER_CLIENT_SECRET=%s\n' "$(rand_hex 24)"
  printf 'HARNESS_PERSON_PASSWORD=%s\n' "Hx-$(rand_hex 12)"
  printf 'ACCOUNT_DELETION_RECEIPT_KEY=%s\n' "$(rand_b64url 32)"
  printf 'ACCOUNT_CENTER_SESSION_SECRET=%s\n' "$(rand_b64url 32)"
  printf 'ACCOUNT_CENTER_TOKEN_ENCRYPTION_KEY=%s\n' "$(rand_b64url 32)"
  for user in "${REDIS_USERS[@]}"; do
    printf 'REDIS_%s_PASSWORD=%s\n' "$(tr 'a-z-' 'A-Z_' <<<"$user")" "${REDIS_PASSWORD[$user]}"
  done
  # Filled in by provisioning once Keycloak created the clients.
  printf 'ACCOUNT_ERASURE_CLIENT_SECRET=pending\n'
  printf 'ACCOUNT_CENTER_OIDC_CLIENT_SECRET=pending-until-the-reconciler-created-the-client\n'
} >"$STATE/compose.env"
chmod 0644 "$STATE/compose.env"
printf '[init] new state in %s\n' "$STATE"
