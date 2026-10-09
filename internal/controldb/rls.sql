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
              ('tenant.tenant_event'),
              ('workspace.workspace'),
              ('membership.membership'),
              ('membership.membership_event'),
              ('invitation.invitation'),
              ('operation.offboarding'),
              ('operation.offboarding_obligation'),
              ('audit.privileged_access')
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

-- membership.membership_event, read by the resolver.
--
-- The loop above gives the two runtime roles their policies on every table here. The resolution
-- role gets one more, on this table only, and for SELECT only: the SUPERSEDED predicate
-- (TDD-organization-control-005) reads which Membership, at which version, a receipted event
-- carries. It runs under the provider binding the resolution scope sets, so the policy keys on the
-- same setting the provider policy does. Without it, FORCE ROW LEVEL SECURITY would show the
-- resolver no rows at all, and every SUPERSEDED closure would be refused for want of evidence that
-- exists.
DROP POLICY IF EXISTS membership_event_resolution_read ON membership.membership_event;
CREATE POLICY membership_event_resolution_read ON membership.membership_event
    FOR SELECT
    TO organization_resolution_rt
    USING (current_setting('app.provider_scope', false)::boolean);

-- tenant.tenant_event, the Tenant half of the same predicate, read the same way.
DROP POLICY IF EXISTS tenant_event_resolution_read ON tenant.tenant_event;
CREATE POLICY tenant_event_resolution_read ON tenant.tenant_event
    FOR SELECT
    TO organization_resolution_rt
    USING (current_setting('app.provider_scope', false)::boolean);

-- membership.membership and tenant.tenant, read by the consumer role.
--
-- The snapshot and the fresh check read across Tenants, so the consumer scope sets the same
-- cross-Tenant binding the provider scope does, and these policies key on it. SELECT only: the
-- consumer writes no business row. An unbound consumer connection sees nothing, the same way an
-- unbound provider one does.
DROP POLICY IF EXISTS membership_consumer_read ON membership.membership;
CREATE POLICY membership_consumer_read ON membership.membership
    FOR SELECT
    TO organization_consumer_rt
    USING (current_setting('app.provider_scope', false)::boolean);

DROP POLICY IF EXISTS tenant_consumer_read ON tenant.tenant;
CREATE POLICY tenant_consumer_read ON tenant.tenant
    FOR SELECT
    TO organization_consumer_rt
    USING (current_setting('app.provider_scope', false)::boolean);


-- membership.membership, tenant.tenant and membership.tenant_admin_grant, read by the self role.
--
-- A person's own contexts (ADR-ORG-005): db.WithSelfRead sets the role and binds app.principal_id to
-- the caller, and these SELECT policies show that Principal's rows and nothing else. A Tenant is
-- visible when the Principal holds an active Membership in it; the subquery runs as the self role,
-- under the Membership policy above it. missing_ok is false, as everywhere but the restrictive pair
-- below, so an unbound self transaction raises rather than reading nothing. The tenant role does not
-- inherit the self role, so none of these reaches a tenant transaction.
DROP POLICY IF EXISTS membership_self_read ON membership.membership;
CREATE POLICY membership_self_read ON membership.membership
    FOR SELECT
    TO organization_self_rt
    USING (principal_id = current_setting('app.principal_id', false)::uuid);

DROP POLICY IF EXISTS tenant_self_read ON tenant.tenant;
CREATE POLICY tenant_self_read ON tenant.tenant
    FOR SELECT
    TO organization_self_rt
    USING (EXISTS (SELECT 1 FROM membership.membership m
                    WHERE m.tenant_id = tenant.tenant_id
                      AND m.principal_id = current_setting('app.principal_id', false)::uuid
                      AND m.status = 'active'));

DROP POLICY IF EXISTS tenant_admin_grant_self_read ON membership.tenant_admin_grant;
CREATE POLICY tenant_admin_grant_self_read ON membership.tenant_admin_grant
    FOR SELECT
    TO organization_self_rt
    USING (principal_id = current_setting('app.principal_id', false)::uuid);

-- membership.tenant_admin_grant, written by a provider only (ADR-ORG-003 §5.2).
--
-- The loop above gives the tenant role its policy here, as on every table in the schema. The writes
-- run as the tenant role because a provider makes them inside one Tenant, through
-- db.WithProviderInTenant, which binds app.acting_provider to the provider's principal_id. These two
-- policies are RESTRICTIVE, so PostgreSQL ANDs them with the tenant policy: a grant must name the
-- acting provider as granted_by and a revocation as revoked_by. An ordinary tenant transaction never
-- binds the setting, reads it as NULL, and is refused. missing_ok is true here, unlike every other
-- policy, because unset is the expected state for every tenant transaction, and the policy refuses
-- it rather than raising. SELECT is not restricted: the check every tenant request makes reads it.
DROP POLICY IF EXISTS tenant_admin_grant_granted_by_provider ON membership.tenant_admin_grant;
CREATE POLICY tenant_admin_grant_granted_by_provider ON membership.tenant_admin_grant
    AS RESTRICTIVE
    FOR INSERT
    TO organization_rt
    WITH CHECK (granted_by = NULLIF(current_setting('app.acting_provider', true), '')::uuid);

DROP POLICY IF EXISTS tenant_admin_grant_revoked_by_provider ON membership.tenant_admin_grant;
CREATE POLICY tenant_admin_grant_revoked_by_provider ON membership.tenant_admin_grant
    AS RESTRICTIVE
    FOR UPDATE
    TO organization_rt
    USING      (NULLIF(current_setting('app.acting_provider', true), '') IS NOT NULL)
    WITH CHECK (revoked_by = NULLIF(current_setting('app.acting_provider', true), '')::uuid);

-- membership.membership_batch and membership.membership_batch_item, purged by the maintenance stage.
--
-- A Membership batch preview past its expiry can never execute, and the maintenance stage deletes it
-- (TDD-organization-control-002 1.13.0 §Membership Batches). That stage runs as the migration role,
-- which owns both tables, and FORCE ROW LEVEL SECURITY binds an owner as it binds everyone else: with
-- no policy naming it, its DELETE would match no row and report success. These two policies give it
-- exactly the expired previews -- SELECT and DELETE, since a DELETE with a WHERE clause applies the
-- SELECT policies too -- and WITH CHECK (false) refuses it every insert and update.
DROP POLICY IF EXISTS membership_batch_purge ON membership.membership_batch;
CREATE POLICY membership_batch_purge ON membership.membership_batch
    FOR ALL
    TO organization_migrator
    USING      (state = 'previewed' AND expires_at < now())
    WITH CHECK (false);

DROP POLICY IF EXISTS membership_batch_item_purge ON membership.membership_batch_item;
CREATE POLICY membership_batch_item_purge ON membership.membership_batch_item
    FOR ALL
    TO organization_migrator
    USING      (EXISTS (SELECT 1
                          FROM membership.membership_batch b
                         WHERE b.batch_id = membership_batch_item.batch_id
                           AND b.state = 'previewed'
                           AND b.expires_at < now()))
    WITH CHECK (false);

-- audit.tenant_provider_access, a Tenant administrator's read of the provider access to its Tenant.
--
-- ADR-ORG-002 §5.6 has a Tenant administrator read the provider rows that name its Tenant, as Google's
-- Access Transparency customer reads the access Google's personnel made to its data. The record
-- carries no policy (above), so the boundary is this view: organization_rt holds SELECT on it and
-- nothing on the table (grants.sql). A view's relations are "checked against the privileges of the
-- rule owner, not the user invoking the rule" (PostgreSQL 17 §39.5), so the owner reads the table and
-- the predicate decides what the caller sees.
--
--   security_barrier, because without it a view cannot "reliably conceal the data in unseen rows"
--   (§39.5): a function in the caller's query could see a row before the predicate drops it.
--
--   current_setting(..., false), as in every tenant policy: an unbound connection raises rather than
--   reading no Tenant's rows.
--
--   Consumer rows are left out. A consumer is a workload acting on its own records, not provider
--   personnel, and its rows name no Tenant in any case.
--
-- Owned by the role this stage runs as, the one that applied the migrations and so owns the table: the
-- view reads the table with its owner's privileges, and a different owner would hold none on it.
--
-- Here rather than in schema.hcl because Atlas OSS models no views. Dropped and created on every run,
-- like each policy, so an edited predicate takes effect; grants.sql runs after and grants it again.
DROP VIEW IF EXISTS audit.tenant_provider_access;
CREATE VIEW audit.tenant_provider_access WITH (security_barrier) AS
SELECT access_id, actor_id, authority, activation_id, tenant_id, operation, correlation_id,
       reason, occurred_at
  FROM audit.privileged_access
 WHERE tenant_id = current_setting('app.tenant_id', false)::uuid
   AND authority <> 'consumer';
COMMENT ON VIEW audit.tenant_provider_access IS
    'The provider access that named the bound Tenant, for its Tenant administrator. ADR-ORG-002 §5.6.';

-- operation.lifecycle_signals, the offboarding and provisioning gauges (TDD-organization-control-004
-- and -003 §Operational Notes).
--
-- The signals are read on every metric collection, on the raw provider connections, which bind no
-- scope: a provider scope would file a privileged-access record per collection, and the review of
-- that record (ADR-ORG-002 §5.6) would then be a review of a timer. The rows are under Row-Level
-- Security all the same, so the read goes through this view, which returns one row of counts and ages
-- and names no Tenant, no offboarding and no person.
--
-- The view reads its tables with its owner's privileges: "If any of the underlying base relations
-- has row-level security enabled, then by default, the row-level security policies of the view
-- owner are applied" (PostgreSQL 17, CREATE VIEW, Notes). The owner is the migration role, which
-- FORCE ROW LEVEL SECURITY binds like everyone else, so the three SELECT policies below give it
-- exactly the rows the counts need: an offboarding still in progress, an open obligation past its
-- due date, a provisioning request in flight or ambiguous. They are permissive, and the migration
-- role has no other SELECT policy on these tables, so they are the whole of what it reads here. Its
-- one write is the provisioning sweep's, through operation.provisioning_sweep below, which ages a
-- request from `requested` to `unresolved` and does nothing else.
--
-- security_barrier for the reason the Tenant's provider-access view carries it: a function in the
-- caller's query is not evaluated against a row before the view's own predicates.
DROP POLICY IF EXISTS offboarding_signals_read ON operation.offboarding;
CREATE POLICY offboarding_signals_read ON operation.offboarding
    FOR SELECT
    TO organization_migrator
    USING (stage IN ('freeze', 'obligations', 'release'));

DROP POLICY IF EXISTS offboarding_obligation_signals_read ON operation.offboarding_obligation;
CREATE POLICY offboarding_obligation_signals_read ON operation.offboarding_obligation
    FOR SELECT
    TO organization_migrator
    USING (state = 'open' AND due_at < now());

DROP POLICY IF EXISTS provisioning_request_signals_read ON tenant.provisioning_request;
CREATE POLICY provisioning_request_signals_read ON tenant.provisioning_request
    FOR SELECT
    TO organization_migrator
    USING (state IN ('requested', 'unresolved'));

DROP VIEW IF EXISTS operation.lifecycle_signals;
CREATE VIEW operation.lifecycle_signals WITH (security_barrier) AS
WITH observed AS (
    SELECT clock_timestamp() AS at
),
overdue AS (
    SELECT count(*) AS n, min(due_at) AS oldest
      FROM operation.offboarding_obligation
     WHERE state = 'open' AND due_at < now()
),
running AS (
    SELECT count(*) AS n, min(started_at) AS oldest
      FROM operation.offboarding
     WHERE stage IN ('freeze', 'obligations', 'release')
),
requests AS (
    SELECT coalesce(desired_profile->>'operation', 'provision') AS operation,
           state,
           count(*) AS n,
           min(CASE WHEN state = 'unresolved' THEN coalesce(resolved_at, requested_at)
                    ELSE requested_at END) AS oldest
      FROM tenant.provisioning_request
     WHERE state IN ('requested', 'unresolved')
     GROUP BY 1, 2
)
SELECT overdue.n AS obligations_overdue,
       coalesce(extract(epoch FROM observed.at - overdue.oldest), 0)::double precision
           AS oldest_overdue_obligation_age,
       running.n AS offboardings_in_progress,
       coalesce(extract(epoch FROM observed.at - running.oldest), 0)::double precision
           AS oldest_offboarding_age,
       coalesce((SELECT n FROM requests WHERE operation = 'provision' AND state = 'requested'), 0)
           AS provision_requested,
       coalesce(extract(epoch FROM observed.at - (SELECT oldest FROM requests
           WHERE operation = 'provision' AND state = 'requested')), 0)::double precision
           AS provision_requested_oldest_age,
       coalesce((SELECT n FROM requests WHERE operation = 'provision' AND state = 'unresolved'), 0)
           AS provision_unresolved,
       coalesce(extract(epoch FROM observed.at - (SELECT oldest FROM requests
           WHERE operation = 'provision' AND state = 'unresolved')), 0)::double precision
           AS provision_unresolved_oldest_age,
       coalesce((SELECT n FROM requests WHERE operation = 'deprovision' AND state = 'requested'), 0)
           AS deprovision_requested,
       coalesce(extract(epoch FROM observed.at - (SELECT oldest FROM requests
           WHERE operation = 'deprovision' AND state = 'requested')), 0)::double precision
           AS deprovision_requested_oldest_age,
       coalesce((SELECT n FROM requests WHERE operation = 'deprovision' AND state = 'unresolved'), 0)
           AS deprovision_unresolved,
       coalesce(extract(epoch FROM observed.at - (SELECT oldest FROM requests
           WHERE operation = 'deprovision' AND state = 'unresolved')), 0)::double precision
           AS deprovision_unresolved_oldest_age
  FROM observed, overdue, running;
COMMENT ON VIEW operation.lifecycle_signals IS
    'Counts and ages of offboarding and provisioning in progress, naming nothing. TDD-organization-control-004 §Operational Notes.';

-- operation.provisioning_sweep and operation.invitation_expiry, the two scheduled sweeps
-- (TDD-organization-control-003 §Scheduled Sweeps, TDD-organization-control-004 §Expiry Sweep).
--
-- The sweeps run in the serving process on a schedule, on the raw provider connections, and bind no
-- scope, for the reason the gauges above bind none: a provider scope would file a privileged-access
-- record per run, and the review of that record would be a review of a timer. So they write through
-- these views, which the migration role owns. An UPDATE on an automatically updatable view is
-- converted into one on its table, and the view owner's policies apply (PostgreSQL 17, CREATE VIEW).
--
-- Each UPDATE policy names the one state the row leaves in USING and the one it enters in WITH
-- CHECK, so a defect in either sweep can make no other transition. The SELECT policies are needed as
-- well: an UPDATE that reads the row is checked against the owner's SELECT policies on the existing
-- and the new row (PostgreSQL 17, CREATE POLICY). The provisioning sweep reuses the gauges' SELECT
-- policy, which already admits `requested` and `unresolved`; the expiry's admits `expired` because
-- that is the new row.
--
-- The routes POST /v1/provisioning/sweep-unresolved and POST /v1/invitations/expire-lapsed run the
-- same statements through the same views under a provider scope.
DROP POLICY IF EXISTS provisioning_request_sweep ON tenant.provisioning_request;
CREATE POLICY provisioning_request_sweep ON tenant.provisioning_request
    FOR UPDATE
    TO organization_migrator
    USING (state = 'requested')
    WITH CHECK (state = 'unresolved' AND resolved_at IS NOT NULL);

DROP VIEW IF EXISTS operation.provisioning_sweep;
CREATE VIEW operation.provisioning_sweep WITH (security_barrier) AS
SELECT request_id, requested_at, state, resolved_at, detail
  FROM tenant.provisioning_request
 WHERE state = 'requested';
COMMENT ON VIEW operation.provisioning_sweep IS
    'Provisioning requests still requested, which the scheduled sweep ages to unresolved. TDD-organization-control-003 §Scheduled Sweeps.';

DROP POLICY IF EXISTS invitation_expiry_read ON invitation.invitation;
CREATE POLICY invitation_expiry_read ON invitation.invitation
    FOR SELECT
    TO organization_migrator
    USING (expires_at <= now() AND state IN ('pending', 'identity_verified', 'expired'));

DROP POLICY IF EXISTS invitation_expiry ON invitation.invitation;
CREATE POLICY invitation_expiry ON invitation.invitation
    FOR UPDATE
    TO organization_migrator
    USING (expires_at <= now() AND state IN ('pending', 'identity_verified'))
    WITH CHECK (state = 'expired');

-- No target identifier or hash: the event the sweep publishes carries neither, so the view does not.
DROP VIEW IF EXISTS operation.invitation_expiry;
CREATE VIEW operation.invitation_expiry WITH (security_barrier) AS
SELECT invitation_id, tenant_id, workspace_id, subject_type, state, correlation_id, principal_id,
       expires_at
  FROM invitation.invitation
 WHERE state IN ('pending', 'identity_verified')
   AND expires_at <= now();
COMMENT ON VIEW operation.invitation_expiry IS
    'Invitations past their expiry and not yet expired, which the scheduled sweep expires. TDD-organization-control-004 §Expiry Sweep.';
