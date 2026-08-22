-- Privileges for the two runtime roles across every schema.
--
-- Applied by `organization-migrate -stage=post`, after the platform migrations and after Atlas
-- has applied the owned schemas. It runs last because GRANT names objects, and an object that
-- does not exist yet cannot be granted on.
--
-- foundation-platform ships the `platform` schema with no GRANT of its own, deliberately: it
-- does not know what the consuming system's roles are called. Granting on it is therefore this
-- repository's obligation, and forgetting it produces a runtime that cannot reach its own
-- outbox.

-- ORDERING GUARD.
--
-- `GRANT ... ON ALL TABLES IN SCHEMA x` over an empty schema is a no-op, not an error. Run
-- before Atlas has applied schema.hcl, this file reports success and grants nothing, and the
-- failure surfaces later as a runtime that cannot read its own tables. That is what happened
-- the first time identity-control's pipeline ran end to end, so the ordering is asserted here
-- rather than trusted to the caller.
DO $$
DECLARE
    missing TEXT;
BEGIN
    SELECT string_agg(expected.name, ', ' ORDER BY expected.name)
      INTO missing
      FROM (VALUES
              ('organization.organization'),
              ('organization.external_reference'),
              ('tenant.tenant'),
              ('tenant.provisioning_request'),
              ('workspace.workspace'),
              ('membership.membership'),
              ('invitation.invitation'),
              ('operation.offboarding'),
              ('operation.offboarding_obligation'),
              ('projection.consumer'),
              ('platform.outbox'),
              ('platform.processed_event'),
              ('platform.dead_letter'),
              ('platform.idempotency_key')
           ) AS expected(name)
     WHERE to_regclass(expected.name) IS NULL;

    IF missing IS NOT NULL THEN
        RAISE EXCEPTION
            'grants stage ran before its objects existed; missing: %', missing
            USING HINT = 'run organization-migrate -stage=pre, then atlas migrate apply, then this stage';
    END IF;
END
$$;

-- Ownership. Every object belongs to the migration role, which is what leaves the runtime
-- roles unable to alter or drop anything regardless of their DML grants — and, on an
-- RLS-protected table, unable to escape the policy: PostgreSQL exempts an owner from its own
-- table's policies unless FORCE is set, so non-ownership and FORCE are two halves of one
-- control.
ALTER SCHEMA organization OWNER TO organization_migrator;
ALTER SCHEMA tenant       OWNER TO organization_migrator;
ALTER SCHEMA workspace    OWNER TO organization_migrator;
ALTER SCHEMA membership   OWNER TO organization_migrator;
ALTER SCHEMA invitation   OWNER TO organization_migrator;
ALTER SCHEMA operation    OWNER TO organization_migrator;
ALTER SCHEMA projection   OWNER TO organization_migrator;
ALTER SCHEMA platform     OWNER TO organization_migrator;

-- CREATE on a schema is a DDL privilege. PostgreSQL grants it to the schema owner only, but
-- PUBLIC retains USAGE on schemas in some configurations, so both are stated rather than
-- assumed.
DO $$
DECLARE
    target TEXT;
BEGIN
    FOREACH target IN ARRAY ARRAY['organization','tenant','workspace','membership',
                                  'invitation','operation','projection','platform']
    LOOP
        EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC', target);
        EXECUTE format('GRANT USAGE ON SCHEMA %I TO organization_rt, organization_provider_rt', target);

        -- DML only. CREATE, TRUNCATE, and REFERENCES are withheld: TRUNCATE on platform.outbox
        -- would let a runtime discard undelivered security events in one statement, which is
        -- the operation the partition retention job exists to perform under the migration role.
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO organization_rt, organization_provider_rt', target);
        EXECUTE format('GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %I TO organization_rt, organization_provider_rt', target);

        -- A table added by a future migration inherits these. Without it, the next schema
        -- change ships a table the runtime cannot read and the failure appears at request time
        -- rather than at deploy time.
        EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA %I GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO organization_rt, organization_provider_rt', target);
        EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA %I GRANT USAGE, SELECT ON SEQUENCES TO organization_rt, organization_provider_rt', target);
    END LOOP;
END
$$;

-- The partition maintenance helpers are invoked by the migration job, never by a runtime.
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA platform FROM PUBLIC;

-- The tenant-scoped role holds no privilege on `organization`.
--
-- TDD-organization-control-001 classifies that schema outside the RLS set because an
-- Organization sponsors several Tenants, so scoping it to one would be wrong. That leaves it
-- with no row-level control at all, which makes the grant the only boundary: a tenant-scoped
-- caller with SELECT here could read every customer in the estate. Provider traffic reaches it
-- through its own role, where the access is attributable at the connection level.
--
-- Applied after the loop rather than by excluding the schema from it, so a schema added later
-- is granted by default and only this one is special.
REVOKE ALL ON ALL TABLES IN SCHEMA organization FROM organization_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA organization
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_rt;

-- The provider role holds nothing on the outbox.
--
-- A provider operation publishes through the same transactional outbox as any other, but the
-- dispatcher and the retention job run under the migration role. Withholding DELETE from the
-- provider role specifically would be arbitrary; what matters is that neither runtime role can
-- TRUNCATE, which the loop already withholds.
