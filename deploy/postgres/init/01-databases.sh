#!/bin/bash
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<EOSQL
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'keycloak', '${KEYCLOAK_DB_PASSWORD}')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'keycloak')\gexec
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'super_skylab', '${SUPER_SKYLAB_DB_PASSWORD}')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'super_skylab')\gexec
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'skylab_cms', '${SKYLAB_CMS_DB_PASSWORD}')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'skylab_cms')\gexec
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'forms', '${FORMS_DB_PASSWORD}')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'forms')\gexec
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'skymail', '${SKYMAIL_DB_PASSWORD}')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'skymail')\gexec

SELECT format('CREATE DATABASE %I OWNER %I', 'keycloak', 'keycloak')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'keycloak')\gexec
SELECT format('CREATE DATABASE %I OWNER %I', 'super_skylab', 'super_skylab')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'super_skylab')\gexec
SELECT format('CREATE DATABASE %I OWNER %I', 'skylab_cms', 'skylab_cms')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'skylab_cms')\gexec
SELECT format('CREATE DATABASE %I OWNER %I', 'forms_db', 'forms')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'forms_db')\gexec
SELECT format('CREATE DATABASE %I OWNER %I', 'skymail', 'skymail')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'skymail')\gexec
EOSQL
