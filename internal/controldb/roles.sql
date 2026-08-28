-- Cluster roles and the schemas they live in.
--
-- Applied by `organization-migrate -stage=pre`, before anything else exists. It creates no
-- table and grants nothing on one: privileges are `grants.sql`, which runs after Atlas has
-- applied the schema, because GRANT names objects and an object that does not exist yet
-- cannot be granted on.
--
-- Three roles, and the split is the design rather than tidiness.
-- TDD-organization-control-001 separates tenant-scoped from provider-scoped traffic at the
-- role level: a single role serving both cannot be constrained, because any policy permissive
-- enough for provider work is permissive enough for a defect in a tenant-scoped path. At the
-- role level the separation is visible in `pg_stat_activity` and in audit output, so incident
-- review can distinguish a provider operation from a tenant-scoped defect without
-- reconstructing application state.

DO $$
BEGIN
    -- organization_migrator owns every object. That ownership is what makes the DML grants
    -- in grants.sql sufficient: an owner can ALTER and DROP its own tables regardless of
    -- which privileges were granted to it, so the runtime roles must own nothing.
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'organization_migrator') THEN
        CREATE ROLE organization_migrator NOLOGIN NOSUPERUSER NOCREATEDB NOBYPASSRLS;
    END IF;

    -- The tenant-scoped runtime. Carries ordinary administrative traffic, bound to exactly
    -- one Tenant per transaction.
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'organization_rt') THEN
        CREATE ROLE organization_rt NOLOGIN NOSUPERUSER NOCREATEDB NOBYPASSRLS;
    END IF;

    -- The provider-scoped runtime. Deliberately cross-Tenant, and still not BYPASSRLS: its
    -- policy reads `app.provider_scope`, so an unbound provider connection fails closed the
    -- same way an unbound tenant connection does. A role holding BYPASSRLS would be
    -- indistinguishable in the catalog from one that never needed it.
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'organization_provider_rt') THEN
        CREATE ROLE organization_provider_rt NOLOGIN NOSUPERUSER NOCREATEDB NOBYPASSRLS;
    END IF;
END
$$;

-- Re-asserted every run rather than set once at creation. A role that predates this file, or
-- one a restored dump brought in with different attributes, is corrected here rather than
-- discovered later by the privilege test. ADR-GLB-002 §5.2 names SUPERUSER and BYPASSRLS
-- specifically: a role holding either makes every policy in this database inert while the
-- catalog still reports RLS as enabled.
ALTER ROLE organization_migrator    NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;
ALTER ROLE organization_rt          NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;
ALTER ROLE organization_provider_rt NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;

-- The schemas are NOT created here. Atlas creates them, and that differs from
-- identity-control on purpose.
--
-- identity-control scopes Atlas to one schema through `search_path`, and a schema-scoped plan
-- may not modify the schema it is scoped to, so `identity` had to exist first. This service
-- declares eight schemas, and Atlas rejects a multi-schema HCL source against a schema-scoped
-- dev URL, so it necessarily works in database scope — where it can own the schema objects.
--
-- Creating them here as well would break the pipeline rather than merely duplicate it: Atlas
-- refuses to apply against a database it considers unclean, and an empty schema it did not
-- create is exactly that. The first run failed with `connected database is not clean: found
-- schema "invitation"`.