#!/bin/sh
# COM-6: Postgres provisioning.
#
# One instance, one database per service, one role per service, and each role
# able to reach only its own database. That last part is the acceptance criterion
# in design/00-common-stack.md:171 ("Each service can only connect to its own
# database"), and it is enforced by revoking CONNECT from PUBLIC rather than by
# trusting every service to use the right DSN.
#
# The Postgres image's entrypoint runs this once, on the first start with an
# empty data directory (/docker-entrypoint-initdb.d is not re-run afterwards).
# The same file is also run against an existing cluster by
# deploy/scripts/provision-upgrade.sh, because the entrypoint never re-runs init
# scripts there. It is therefore idempotent: every role is created only when
# missing and otherwise converged to the password in .env with ALTER ROLE, every
# database is created only when missing (via SELECT ... \gexec), and REVOKE/GRANT
# are idempotent. Running it again changes nothing except a password that .env
# has since changed.
#
# Passwords arrive as environment variables rather than being written here. They
# are generated as hex (deploy/scripts/generate-secrets.sh), so they need no
# escaping in SQL, in a DSN or in a Valkey config file, which is why the quotes
# below are plain.

set -eu

psql_super() {
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres "$@"
}

# provision_role <role> <password>: create the role if it is missing, then
# converge an existing one to the password in .env (covers both a brand-new host
# and an existing host whose password has since changed). The WHERE clauses make
# both SELECTs a no-op when they do not apply; \gexec runs only the rows returned.
provision_role() {
    role="$1"
    password="$2"
    psql_super --set=role="$role" --set=password="$password" <<-'SQL'
	SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'role', :'password')
	WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = :'role')
	\gexec
	SELECT format('ALTER ROLE %I LOGIN PASSWORD %L', :'role', :'password')
	WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = :'role')
	\gexec
	SQL
}

# provision_database <database> <owner>: create the database only when absent.
# CREATE DATABASE cannot be wrapped in the guard as plain SQL; SELECT ... \gexec
# is what makes the existence check and the creation one statement.
provision_database() {
    db="$1"
    owner="$2"
    psql_super --set=db="$db" --set=owner="$owner" <<-'SQL'
	SELECT format('CREATE DATABASE %I OWNER %I', :'db', :'owner')
	WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = :'db')
	\gexec
	SQL
}

# %I / %L through format() rather than string concatenation: the role names are
# literals from this file, but a password that one day stops being hex should
# still be quoted correctly rather than closing the string.
provision_role auth_rw "$AUTH_RW_PASSWORD"
provision_role config_rw "$CONFIG_RW_PASSWORD"
provision_role session_rw "$SESSION_RW_PASSWORD"
provision_role patch_ro "$PATCH_RO_PASSWORD"
provision_role admin_auth_rw "$ADMIN_AUTH_RW_PASSWORD"
# The Allocator owns its game-server pool and allocations, nothing else.
provision_role allocator_rw "$ALLOCATOR_RW_PASSWORD"
# Postgres_exporter is monitoring-only. It owns nothing; pg_monitor
# below gives it the pg_stat_* views, and the CONNECT grant further down is the
# one database its DSN names.
provision_role postgres_exporter "$POSTGRES_EXPORTER_PASSWORD"

# Owners, not just CONNECT holders: each service owns its database, so it can
# migrate its own schema without a separate grant.
provision_database auth auth_rw
provision_database config config_rw
provision_database session session_rw
provision_database admin_auth admin_auth_rw
provision_database allocator allocator_rw

# Nobody else may connect. Without this, every role in the cluster can open every
# database, because CONNECT is granted to PUBLIC by default.
psql_super <<-'SQL'
	REVOKE CONNECT ON DATABASE auth FROM PUBLIC;
	REVOKE CONNECT ON DATABASE config FROM PUBLIC;
	REVOKE CONNECT ON DATABASE session FROM PUBLIC;
	REVOKE CONNECT ON DATABASE admin_auth FROM PUBLIC;
	REVOKE CONNECT ON DATABASE allocator FROM PUBLIC;

	GRANT CONNECT ON DATABASE auth TO auth_rw;
	-- patch_ro reads the config database and never writes it
	-- (design/03-patch-minimal.md:65 says Patch connects as patch_ro, read-only).
	GRANT CONNECT ON DATABASE config TO config_rw, patch_ro;
	GRANT CONNECT ON DATABASE session TO session_rw;
	-- The staff identity service owns admin_auth and nothing else connects to it.
	GRANT CONNECT ON DATABASE admin_auth TO admin_auth_rw;
	GRANT CONNECT ON DATABASE allocator TO allocator_rw;
	-- The exporter connects to `postgres` and to no service database.
	-- The REVOKEs above are what make "only" true; this grant is explicit so the
	-- role's reach is readable here rather than inherited from PUBLIC.
	GRANT CONNECT ON DATABASE postgres TO postgres_exporter;
	SQL

# pg_monitor is a predefined role: membership grants the pg_stat_* views and
# functions an exporter needs, with no table privileges and no superuser.
psql_super -c 'GRANT pg_monitor TO postgres_exporter;'

# In-database grants. The owner needs CREATE explicitly: since Postgres 15 the
# public schema no longer grants it to PUBLIC, and relying on the version-specific
# pg_database_owner behaviour is the kind of thing that breaks on upgrade.
psql_super --dbname auth <<-'SQL'
	GRANT ALL ON SCHEMA public TO auth_rw;
	SQL

psql_super --dbname session <<-'SQL'
	GRANT ALL ON SCHEMA public TO session_rw;
	SQL

psql_super --dbname admin_auth -c 'GRANT ALL ON SCHEMA public TO admin_auth_rw;'
psql_super --dbname allocator -c 'GRANT ALL ON SCHEMA public TO allocator_rw;'

psql_super --dbname config <<-'SQL'
	GRANT ALL ON SCHEMA public TO config_rw;

	-- Read-only, and reading only. SELECT on what exists now, plus default
	-- privileges so tables config_rw creates later are readable without anyone
	-- remembering to grant them. The FOR ROLE is the load-bearing part: default
	-- privileges apply to objects created by that specific role.
	GRANT USAGE ON SCHEMA public TO patch_ro;
	GRANT SELECT ON ALL TABLES IN SCHEMA public TO patch_ro;
	ALTER DEFAULT PRIVILEGES FOR ROLE config_rw IN SCHEMA public
	    GRANT SELECT ON TABLES TO patch_ro;
	SQL

echo "otomo: provisioned roles auth_rw, config_rw, session_rw, patch_ro, admin_auth_rw, allocator_rw, postgres_exporter and databases auth, config, session, admin_auth, allocator"
