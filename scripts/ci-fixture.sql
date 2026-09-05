-- The runtime login roles and the two-Tenant fixture the isolation suite asserts against.
--
-- One file, used by CI and by `make test-ci`. It lived as two heredocs inside the workflow,
-- which meant a local reproduction of a CI failure had to be retyped from the YAML and could
-- drift from it silently -- and a fixture that differs from CI's is a local run that proves
-- something about a database CI never had.
--
-- Passwords are psql variables rather than literals, because these roles belong to the cluster
-- and not to one database: a local run that hardcoded CI's values would rewrite the passwords
-- the development .env depends on. Pass them in:
--
--   psql "$DSN" -v runtime_password=runtime -v provider_password=provider -f scripts/ci-fixture.sql
--
-- Group roles are NOLOGIN by design, so a login role that inherits one is how a deployable
-- authenticates. TDD-organization-control-001 requires the isolation tests to connect as the
-- role that carries production traffic, which cannot be the role the migration authenticates as.

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

-- Two Tenants, because cross-tenant denial cannot be proven with one. Seeded on the
-- administrative connection deliberately: the fixture is not the thing under test, and seeding
-- through a bound runtime role would make the suite assert its own setup.
INSERT INTO organization.organization (organization_id, display_name, classification, status)
VALUES ('00000000-0000-4000-8000-00000000000a', 'Org A', 'customer', 'active'),
       ('00000000-0000-4000-8000-00000000000b', 'Org B', 'customer', 'active')
ON CONFLICT DO NOTHING;

INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
VALUES ('11111111-1111-4111-8111-11111111111a', '00000000-0000-4000-8000-00000000000a', 'Tenant A', 'active', 'pooled'),
       ('11111111-1111-4111-8111-11111111111b', '00000000-0000-4000-8000-00000000000b', 'Tenant B', 'active', 'pooled')
ON CONFLICT DO NOTHING;

INSERT INTO membership.membership (membership_id, principal_id, tenant_id, subject_type, status, valid_from, provenance)
VALUES ('22222222-2222-4222-8222-22222222222a', '33333333-3333-4333-8333-33333333333a', '11111111-1111-4111-8111-11111111111a', 'human', 'active', now(), 'migration'),
       ('22222222-2222-4222-8222-22222222222b', '33333333-3333-4333-8333-33333333333b', '11111111-1111-4111-8111-11111111111b', 'human', 'active', now(), 'migration')
ON CONFLICT DO NOTHING;
