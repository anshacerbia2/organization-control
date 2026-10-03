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

-- The runtime login roles, the same file the development server's migration runs.
\ir login-roles.sql

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

-- The dev issuer's provider (cmd/organization-devissuer, role=provider). A provider is a Principal
-- holding a provider grant this service records (ADR-ORG-001 §5.11), so the fixture records one for
-- the Principal that issuer names, the way `organization-control bootstrap-provider` would. Seeded
-- here rather than by running the command for the reason the Tenants above are: the grant is setup,
-- and the bootstrap has its own integration test in internal/authority.
--
-- An emergency grant, as the bootstrap's is (ADR-ORG-002 §5.2): standing, so the dev issuer's provider
-- token is a provider without an activation. Every request it makes is reported as emergency use.
INSERT INTO organization.provider_grant (grant_id, principal_id, scope, bootstrap_operator, reason, kind)
VALUES ('44444444-4444-4444-8444-44444444444a', '55555555-5555-4555-8555-55555555555a',
        'provider:organization-control', 'scripts/ci-fixture.sql',
        'development and CI: the dev issuer''s provider', 'emergency')
ON CONFLICT DO NOTHING;
