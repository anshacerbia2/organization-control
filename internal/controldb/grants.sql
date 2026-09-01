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
              ('audit.privileged_access'),
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
ALTER SCHEMA audit        OWNER TO organization_migrator;
ALTER SCHEMA platform     OWNER TO organization_migrator;

-- CREATE on a schema is a DDL privilege. PostgreSQL grants it to the schema owner only, but
-- PUBLIC retains USAGE on schemas in some configurations, so both are stated rather than
-- assumed.
DO $$
DECLARE
    target TEXT;
BEGIN
    FOREACH target IN ARRAY ARRAY['organization','tenant','workspace','membership',
                                  'invitation','operation','projection','audit','platform']
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

-- The dispatcher holds privileges on three objects and nothing else.
--
-- It is a separate deployable that drains platform.outbox, so it needs no access to a single
-- business table. Running it under organization_provider_rt would work and would make a delivery
-- worker able to mutate every Tenant in the estate -- a second process with the control plane's
-- authority, for a job whose whole scope is "move rows that are already committed".
--
-- Named tables rather than a schema-wide grant, and deliberately so: platform also holds
-- idempotency_key and processed_event, which are an HTTP concern and a consumer's concern. A
-- schema-wide grant would hand this role both, and would silently hand it whatever the substrate
-- adds to the schema next.
GRANT USAGE ON SCHEMA platform TO organization_dispatch_rt;
GRANT SELECT, UPDATE            ON platform.outbox      TO organization_dispatch_rt;
GRANT SELECT, INSERT, UPDATE    ON platform.dead_letter TO organization_dispatch_rt;
GRANT USAGE, SELECT             ON SEQUENCE platform.outbox_sequence TO organization_dispatch_rt;

-- No DELETE on the outbox. A dispatched row is marked published, never removed: retention is the
-- maintenance job's decision, and a worker able to delete is a worker whose bug is unrecoverable
-- because the evidence goes with it.
REVOKE DELETE ON platform.outbox FROM organization_dispatch_rt;

-- And no default privileges: a table added to platform later must be granted deliberately rather
-- than inherited by a role whose scope is three objects.
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA platform
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_dispatch_rt;

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

-- The tenant-scoped role holds nothing in `audit`.
--
-- The evidence there records actions taken across Tenants, so it carries no tenant_id and sits
-- outside the RLS set by construction — that is why it is its own schema rather than a table in
-- `operation`. See schema.hcl and rls.sql. That leaves the grant as the only boundary, exactly as it
-- is for `organization`: a tenant-scoped caller with SELECT here could read which provider operators
-- touched which correlations across the whole estate, and one with INSERT could write evidence
-- attributing an access to somebody else.
--
-- Whole-schema rather than per-table, so a second evidence table added later is covered without
-- anyone remembering to extend a list — the same reasoning rls.sql uses for its policy set.
REVOKE ALL ON ALL TABLES IN SCHEMA audit FROM organization_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA audit
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_rt;

-- The provider role may append evidence and may not change it.
--
-- The loop above grants SELECT, INSERT, UPDATE, and DELETE on every table in every owned schema,
-- which on this schema hands the role being audited the ability to rewrite or erase its own audit
-- trail. That is the one place a uniform grant is wrong: evidence whose writer can amend it is not
-- evidence, and an operator investigating a cross-Tenant access would have no way to tell a missing
-- row from an access that never happened.
--
-- Found by querying has_table_privilege after the first clean deploy rather than by reading this
-- file — the same way identity-control found Atlas's revision table sitting inside a schema the
-- runtime could write.
--
-- SELECT goes too, on least privilege: internal/access only inserts, and nothing in the repository
-- reads this table. A read surface for an investigation would come with its own grant and its own
-- role, rather than being available in advance to the role under investigation.
REVOKE SELECT, UPDATE, DELETE ON ALL TABLES IN SCHEMA audit FROM organization_provider_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA audit
    REVOKE SELECT, UPDATE, DELETE ON TABLES FROM organization_provider_rt;

-- The provider role holds nothing on the outbox.
--
-- A provider operation publishes through the same transactional outbox as any other, but the
-- dispatcher and the retention job run under the migration role. Withholding DELETE from the
-- provider role specifically would be arbitrary; what matters is that neither runtime role can
-- TRUNCATE, which the loop already withholds.
