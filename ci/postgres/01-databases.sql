-- One database per test suite, created on the first start of the CI Postgres
-- (ci/compose.yaml). Every suite connects as the `ci` superuser: the tests create
-- their own schema, and admin_auth's migration round-trip drops tables, which a
-- least-privilege role could not do. The per-service roles are the deploy
-- stack's business (deploy/postgres/init/01-provision.sh), not CI's.
--
-- session_smoke must stay last: the healthcheck in ci/compose.yaml waits for it.
CREATE DATABASE admin_auth_test;
CREATE DATABASE allocator_test;
CREATE DATABASE auth_test;
CREATE DATABASE config_test;
CREATE DATABASE session_test;
CREATE DATABASE session_smoke;
