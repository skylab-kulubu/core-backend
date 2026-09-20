# Dedicated account-access Redis

This deployment is a security projection, not a cache. Keep it separate from the general Redis service. The supplied configuration disables plaintext transport, requires mutual TLS, persists every write with AOF `appendfsync always`, and uses `noeviction`. The image is version-and-digest pinned.

Mount these operator-provisioned files through `ACCOUNT_ACCESS_REDIS_SECRETS_DIR`:

- `account-access-tls.crt`
- `account-access-tls.key`
- `account-access-ca.crt`
- `account-access-users.acl`

Generate strong ACL passwords outside the repository. The Core runtime writer may use `GET`, `MGET`, `SET`, `PTTL`, `WAIT`, `PING`, `HELLO` and connection commands only for `skylab:account-access:v1:*`. Reader credentials may use only `GET`, `MGET`, `PING`, `HELLO` and connection commands for that namespace. Neither runtime role receives `DEL`, `UNLINK`, expiry, flush, rename, script, transaction, publish or administrative commands. A separate recovery operator owns contract-key removal, namespace counting and disaster-recovery cutover.

Mount a distinct writer client certificate, private key and CA bundle read-only into the Core container, then point `ACCOUNT_ACCESS_REDIS_TLS_CERT_FILE`, `ACCOUNT_ACCESS_REDIS_TLS_KEY_FILE` and `ACCOUNT_ACCESS_REDIS_CA_CERT_FILE` at those mounted paths. The Redis server certificate/key above must never be mounted into Core, and the Core writer private key must not be shared with reader services.

When replication is configured, set Core's `ACCOUNT_ACCESS_REDIS_REQUIRED_REPLICAS` and bounded `ACCOUNT_ACCESS_REDIS_WAIT_TIMEOUT`. A value of `0` is valid only when the deployment has no replica to acknowledge.
