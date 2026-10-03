-- The five runtime login roles, each inheriting one NOLOGIN group role from roles.sql.
--
-- Shared by scripts/ci-fixture.sql (CI and `make test-ci`) and deploy/dev/migrate.sh (the development
-- server), so the roles a deployable authenticates as are created one way everywhere. Passwords are
-- psql variables, never literals, because these roles belong to the cluster:
--
--   psql -v runtime_password=… -v provider_password=… -v dispatch_password=… \
--        -v resolution_password=… -v consumer_password=… -f scripts/login-roles.sql
--
-- Group roles are NOLOGIN by design, so a login role that inherits one is how a deployable
-- authenticates. TDD-organization-control-001 requires traffic to use the role that carries it, which
-- cannot be the role the migration authenticates as.

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'organization_app') THEN
    CREATE ROLE organization_app LOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'organization_provider_app') THEN
    CREATE ROLE organization_provider_app LOGIN;
  END IF;
END
$$;

-- NOBYPASSRLS is the one that matters: a runtime role holding BYPASSRLS makes every policy
-- inert while every structural assertion about those policies still passes.
ALTER ROLE organization_app
  WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD :'runtime_password';
ALTER ROLE organization_provider_app
  WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD :'provider_password';

GRANT organization_rt          TO organization_app;
GRANT organization_provider_rt TO organization_provider_app;

-- The delivery worker's login role. Separate from both runtimes because it is a separate process
-- with a separate credential: rotating it must not touch the API's, and a compromised delivery
-- worker must not be able to read a Tenant.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'organization_dispatch_app') THEN
    CREATE ROLE organization_dispatch_app LOGIN;
  END IF;
END
$$;
ALTER ROLE organization_dispatch_app
  WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD :'dispatch_password';
GRANT organization_dispatch_rt TO organization_dispatch_app;

-- The resolver's login role. Separate again, and for a sharper reason than the dispatcher's:
-- this is the credential that closes security incidents. A deployment that reused the provider
-- credential for it would put replay and closure behind one secret, which is the separation the
-- fourth role exists to create.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'organization_resolution_app') THEN
    CREATE ROLE organization_resolution_app LOGIN;
  END IF;
END
$$;
ALTER ROLE organization_resolution_app
  WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD :'resolution_password';
GRANT organization_resolution_rt TO organization_resolution_app;

-- A registered consumer's login role. Its own credential because the consumer is another
-- deployable: its secret leaking must yield its seven routes' reads and its own registry row, not the
-- control plane.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'organization_consumer_app') THEN
    CREATE ROLE organization_consumer_app LOGIN;
  END IF;
END
$$;
ALTER ROLE organization_consumer_app
  WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD :'consumer_password';
GRANT organization_consumer_rt TO organization_consumer_app;
