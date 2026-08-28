-- Row-Level Security: enable, force, and one policy per role per table.
--
-- Applied by `organization-migrate -stage=post`, after Atlas has created the tables, because
-- a policy names a table. It is a separate file from grants.sql so the two failure modes stay
-- separate: a missing grant produces a runtime that cannot read, and a missing policy produces
-- a runtime that reads too much.
--
-- # Why this is not in schema.hcl
--
-- Atlas OSS does not model `ENABLE`/`FORCE ROW LEVEL SECURITY` or `CREATE POLICY`. That has a
-- consequence worth stating: nothing reconciles these the way Atlas reconciles a column, so a
-- policy dropped by hand stays dropped and the schema still matches its declared state. The
-- structural assertions in `rls_integration_test.go` are the only thing that notices, which is
-- why they read `pg_class` and `pg_policy` rather than this file.
--
-- # Why every statement is re-runnable
--
-- This stage runs on every deploy. `ENABLE`/`FORCE` are idempotent; `CREATE POLICY` is not, so
-- each policy is dropped and recreated. Dropping first also means an edited predicate actually
-- takes effect: `CREATE POLICY IF NOT EXISTS` does not exist, and a guarded create would leave
-- the old predicate in place while reporting success.

-- ORDERING GUARD.
--
-- `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` on a missing table raises, so this stage cannot
-- silently do nothing the way a GRANT over an empty schema can. It is asserted anyway, because
-- the error PostgreSQL raises names one relation and this names the stage that is out of order.
DO $$
DECLARE
    missing TEXT;
BEGIN
    SELECT string_agg(expected.name, ', ' ORDER BY expected.name)
      INTO missing
      FROM (VALUES
              ('tenant.tenant'),
              ('tenant.provisioning_request'),
              ('workspace.workspace'),
              ('membership.membership'),
              ('invitation.invitation'),
              ('operation.offboarding'),
              ('operation.offboarding_obligation')
           ) AS expected(name)
     WHERE to_regclass(expected.name) IS NULL;

    IF missing IS NOT NULL THEN
        RAISE EXCEPTION
            'the RLS stage ran before its tables existed; missing: %', missing
            USING HINT = 'run organization-migrate -stage=pre, then atlas migrate apply, then -stage=post';
    END IF;
END
$$;

-- `audit` is absent from the schema list above and below, and that is why the schema exists.
--
-- audit.privileged_access records provider access taken *across* Tenants, so it carries no
-- tenant_id. Placed in `operation`, it would fail the modelling rule asserted next — every table in
-- an RLS schema has a non-nullable tenant_id — and that rule should not grow an exception: a
-- predicate keyed on an invented tenant_id would either attribute a cross-Tenant action to one
-- arbitrary Tenant or hide the row from every Tenant's view. The rule's own HINT prescribes the
-- answer taken here, which is to move the table rather than to carve it out.
--
-- This was found by running the pipeline rather than by reading it. The table was written into
-- `operation` first, and `-stage=post` refused the deploy naming the table — the check working
-- exactly as intended, on the first table that had ever violated it.
--
-- Its boundary is the grant instead: grants.sql revokes everything in `audit` from
-- `organization_rt`, so the tenant-scoped role cannot read or write the evidence at all, and
-- provider traffic reaches it through a role whose access is attributable at the connection level.
--
-- A policy would not have worked in any case. The recorder writes OUTSIDE the transaction it
-- describes — db.withProviderScope records evidence before opening the domain transaction, which is
-- what makes the evidence survive a rollback — so `app.provider_scope` is unset at that moment and a
-- policy keyed on it would refuse every insert.

-- The modelling rule, checked BEFORE any policy is created.
--
-- TDD-organization-control-001: every table carrying RLS has a non-nullable tenant_id, and a
-- tenant-scoped table without that column is a modelling error.
--
-- The order matters, and the first version of this file had it wrong. With the check placed
-- after the policy loop, CREATE POLICY reached the missing column first and the deploy failed
-- with `column "tenant_id" does not exist`. True, and it names a column rather than the rule
-- that was broken. Checked first, the error names the table and says what to do about it.
--
-- Found by creating a table in an RLS schema outside this pipeline and reading which error came
-- back, which is also the case it exists for: a table this pipeline did not create.
DO $$
DECLARE
    offending TEXT;
BEGIN
    SELECT string_agg(format('%s.%s', n.nspname, c.relname), ', ' ORDER BY n.nspname, c.relname)
      INTO offending
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname IN ('tenant', 'workspace', 'membership', 'invitation', 'operation')
       AND c.relkind = 'r'
       AND NOT EXISTS (
             SELECT 1
               FROM pg_attribute a
              WHERE a.attrelid = c.oid
                AND a.attname  = 'tenant_id'
                AND a.attnotnull
                AND a.attnum > 0
                AND NOT a.attisdropped);

    IF offending IS NOT NULL THEN
        RAISE EXCEPTION
            'table(s) in an RLS schema carry no non-nullable tenant_id: %', offending
            USING HINT = 'add the column, or move the table to a schema outside the RLS set';
    END IF;
END
$$;

-- Applied identically to every tenant-scoped table. A loop rather than seven copied blocks:
-- the predicate is the control, and seven hand-written copies is seven chances for one of them
-- to drift into `USING` without `WITH CHECK`.
--
-- Three choices in the policy body carry weight, per ADR-GLB-002 §5.2:
--
--   FORCE ROW LEVEL SECURITY, because PostgreSQL does not apply policies to a table's owner
--   without it. Omitted, the control is inert against exactly the connection most likely to be
--   misused during an incident.
--
--   WITH CHECK mirroring USING, so a bound caller cannot write a row belonging to another
--   Tenant. A policy carrying only USING restricts reads and leaves INSERT and UPDATE open,
--   which is quiet: reads look correctly isolated while writes are not.
--
--   current_setting(..., false) — missing_ok = false. An unset binding raises instead of
--   returning NULL, and a NULL predicate is false, so an unbound connection would return zero
--   rows. A query returning nothing looks like an empty result; a query raising looks like the
--   defect it is.
DO $$
DECLARE
    target RECORD;
BEGIN
    FOR target IN
        SELECT n.nspname AS schema_name, c.relname AS table_name
        FROM   pg_class c
        JOIN   pg_namespace n ON n.oid = c.relnamespace
        WHERE  n.nspname IN ('tenant', 'workspace', 'membership', 'invitation', 'operation')
          AND  c.relkind = 'r'
        ORDER BY n.nspname, c.relname
    LOOP
        -- Every table in these schemas must be protected, so the set is discovered from the
        -- catalog rather than listed. A table added by a later migration is covered on the
        -- next deploy instead of waiting for someone to extend a list.
        EXECUTE format('ALTER TABLE %I.%I ENABLE ROW LEVEL SECURITY', target.schema_name, target.table_name);
        EXECUTE format('ALTER TABLE %I.%I FORCE  ROW LEVEL SECURITY', target.schema_name, target.table_name);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I.%I',
                       target.table_name || '_tenant_scope', target.schema_name, target.table_name);
        EXECUTE format($fmt$
            CREATE POLICY %I ON %I.%I
                FOR ALL
                TO organization_rt
                USING      (tenant_id = current_setting('app.tenant_id', false)::uuid)
                WITH CHECK (tenant_id = current_setting('app.tenant_id', false)::uuid)
        $fmt$, target.table_name || '_tenant_scope', target.schema_name, target.table_name);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I.%I',
                       target.table_name || '_provider_scope', target.schema_name, target.table_name);
        EXECUTE format($fmt$
            CREATE POLICY %I ON %I.%I
                FOR ALL
                TO organization_provider_rt
                USING      (current_setting('app.provider_scope', false)::boolean)
                WITH CHECK (current_setting('app.provider_scope', false)::boolean)
        $fmt$, target.table_name || '_provider_scope', target.schema_name, target.table_name);
    END LOOP;
END
$$;

