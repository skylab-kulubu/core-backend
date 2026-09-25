#!/bin/bash
# One Postgres for the whole harness: one role and one database per service, as in production
# (core-backend deploy/postgres/init plus Account Center).
set -euo pipefail
create() { # create ROLE DATABASE PASSWORD
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<EOSQL
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', '$1', '$3')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$1')\gexec
SELECT format('CREATE DATABASE %I OWNER %I', '$2', '$1')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$2')\gexec
EOSQL
}
create keycloak keycloak "$KEYCLOAK_DB_PASSWORD"
create super_skylab super_skylab "$CORE_DB_PASSWORD"
create skymail skymail "$SKYMAIL_DB_PASSWORD"
create skylab_cms skylab_cms "$CMS_DB_PASSWORD"
create forms forms_db "$FORMS_DB_PASSWORD"
create account_center account_center "$ACCOUNT_CENTER_DB_PASSWORD"
