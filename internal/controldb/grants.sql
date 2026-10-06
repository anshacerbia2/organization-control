-- Privileges for every runtime role across every schema.
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
              ('organization.provider_grant'),
              ('organization.provider_activation'),
              ('organization.provider_grant_event'),
              ('organization.emergency_grant_use'),
              ('tenant.tenant'),
              ('tenant.provisioning_request'),
              ('tenant.tenant_event'),
              ('workspace.workspace'),
              ('membership.membership'),
              ('membership.membership_event'),
              ('membership.tenant_admin_grant'),
              ('invitation.invitation'),
              ('operation.offboarding'),
              ('operation.offboarding_obligation'),
              ('audit.privileged_access'),
              ('projection.consumer'),
              ('platform.outbox'),
              ('platform.outbox_delivery'),
              ('platform.subscription'),
              ('platform.processed_event'),
              ('platform.dead_letter'),
              ('platform.delivery_receipt'),
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

-- ---------------------------------------------------------------------------------------------
-- The owned schemas, deny by default
-- ---------------------------------------------------------------------------------------------
--
-- Every schema starts closed to both runtime roles, and every grant below names a table and a
-- privilege that a statement in this repository needs. tools/grantcheck derives which role runs
-- which statement and has PostgreSQL judge each grant (TDD-organization-control-001 §Grant
-- Derivation); `go run ./tools/grantcheck -matrix` shows the statements behind every line here.
--
-- This replaced a loop that granted SELECT, INSERT, UPDATE and DELETE on every table in every
-- owned schema to both roles, and had new tables inherit the same. The loop's argument was that
-- a table the runtime cannot read surfaces on the first deploy. grantcheck makes that argument
-- unnecessary: a statement needing a grant this file does not give fails CI as MISSING, before
-- the deploy. The loop's cost was the other direction, which nothing reported -- the first run of
-- grantcheck found 52 grants no statement needed, among them DELETE on every business table for
-- both roles, and the tenant-scoped role able to create Tenants and provisioning requests.
--
-- A new table is therefore closed to both roles until this file grants it, the same rule
-- `platform` has always had.
DO $$
DECLARE
    target TEXT;
BEGIN
    FOREACH target IN ARRAY ARRAY['organization','tenant','workspace','membership',
                                  'invitation','operation','projection','audit']
    LOOP
        -- CREATE on a schema is a DDL privilege. PostgreSQL grants it to the schema owner only,
        -- but PUBLIC retains USAGE on schemas in some configurations, so both are stated.
        EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC', target);
        EXECUTE format('REVOKE ALL ON SCHEMA %I FROM organization_rt, organization_provider_rt', target);

        -- Withdraws what the loop granted, on a database that already ran it. Before the grants
        -- below, not after, or it would remove them again.
        EXECUTE format('REVOKE ALL ON ALL TABLES    IN SCHEMA %I FROM organization_rt, organization_provider_rt', target);
        EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA %I FROM organization_rt, organization_provider_rt', target);

        -- And the inheritance: a table added later arrives closed.
        EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA %I REVOKE ALL ON TABLES FROM organization_rt, organization_provider_rt', target);
        EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA %I REVOKE ALL ON SEQUENCES FROM organization_rt, organization_provider_rt', target);
    END LOOP;
END
$$;

-- No DELETE anywhere, for either role. Nothing in this repository deletes a business row: a
-- Membership is revoked, a Tenant retired, an invitation expired, an obligation resolved. A
-- request path able to delete could remove the record of a decision instead of making a new one.
-- No TRUNCATE, REFERENCES or TRIGGER either; none is granted below.

-- tenant.tenant
--   organization_rt          SELECT            invitation and membership read status and
--                                              tenant_security_version inside their transactions
--   organization_provider_rt SELECT, INSERT,   the Tenant lifecycle, organization retirement,
--                            UPDATE            the projection snapshot, the fresh context check
--
-- The tenant-scoped role cannot create or change a Tenant. The loop gave it both.
GRANT USAGE ON SCHEMA tenant TO organization_rt, organization_provider_rt;
GRANT SELECT                 ON tenant.tenant TO organization_rt;
GRANT SELECT, INSERT, UPDATE ON tenant.tenant TO organization_provider_rt;

-- tenant.provisioning_request -- provider only: the provisioning coordinator and offboarding's
-- deprovisioning request.
GRANT SELECT, INSERT, UPDATE ON tenant.provisioning_request TO organization_provider_rt;

-- tenant.tenant_event -- INSERT, provider only, in the transaction that appends a Tenant event.
-- History, and history is not edited: no role may UPDATE or DELETE a row, for the reason
-- membership.membership_event gives. The resolver reads it through its own role, below.
GRANT INSERT ON tenant.tenant_event TO organization_provider_rt;

-- workspace.workspace -- tenant only. The Workspace service runs under tenant scope, and no
-- provider path reads or writes a Workspace.
GRANT USAGE ON SCHEMA workspace TO organization_rt;
GRANT SELECT, INSERT, UPDATE ON workspace.workspace TO organization_rt;

-- membership.membership
--   organization_rt          SELECT, INSERT,   the Membership service, invitation acceptance,
--                            UPDATE            the offboarding freeze, Workspace retirement checks
--   organization_provider_rt SELECT            the projection snapshot and reconciliation, the
--                                              fresh context check
--
-- Membership authority changes only under tenant scope. The provider role reads it and cannot
-- write it, which the loop allowed.
GRANT USAGE ON SCHEMA membership TO organization_rt, organization_provider_rt;
GRANT SELECT, INSERT, UPDATE ON membership.membership TO organization_rt;
GRANT SELECT                 ON membership.membership TO organization_provider_rt;

-- membership.membership_event -- INSERT, tenant only, in the transaction that bumps the version.
--
-- History, and history is not edited: no role may UPDATE or DELETE a row, and neither runtime role
-- may read it. A runtime able to rewrite a version could make an older event look like the newer
-- one and close a revocation's dead letter as superseded by it. The resolver reads it through its
-- own role, below.
GRANT INSERT ON membership.membership_event TO organization_rt;

-- membership.tenant_admin_grant -- tenant only (ADR-ORG-003). SELECT for the check every tenant
-- request makes, under that Tenant's policy; INSERT for a grant; UPDATE on the three revocation
-- columns alone, which the revocation's lock needs as well. No DELETE, and no UPDATE of who was
-- granted, by whom, when or why. A provider makes both writes, through db.WithProviderInTenant on
-- the tenant pool, and the restrictive policies in rls.sql refuse them from any other transaction.
-- The provider role holds nothing here.
GRANT SELECT, INSERT ON membership.tenant_admin_grant TO organization_rt;
GRANT UPDATE (revoked_at, revoked_by, revoke_reason) ON membership.tenant_admin_grant TO organization_rt;

-- invitation.invitation
--   organization_rt          SELECT, INSERT, UPDATE   issue, accept, revoke
--   organization_provider_rt SELECT, UPDATE           expiry sweep across Tenants
GRANT USAGE ON SCHEMA invitation TO organization_rt, organization_provider_rt;
GRANT SELECT, INSERT, UPDATE ON invitation.invitation TO organization_rt;
GRANT SELECT, UPDATE         ON invitation.invitation TO organization_provider_rt;

-- operation.offboarding, operation.offboarding_obligation -- provider only. Offboarding is a
-- provider operation; its freeze of Memberships runs under tenant scope and touches only
-- membership.membership.
GRANT USAGE ON SCHEMA operation TO organization_provider_rt;
GRANT SELECT, INSERT, UPDATE ON operation.offboarding            TO organization_provider_rt;
GRANT SELECT, INSERT, UPDATE ON operation.offboarding_obligation TO organization_provider_rt;

-- organization.organization -- provider only.
--
-- TDD-organization-control-001 classifies this schema outside the RLS set because an
-- Organization sponsors several Tenants, so scoping it to one would be wrong. That leaves it with
-- no row-level control at all, which makes the grant the only boundary: a tenant-scoped caller
-- with SELECT here could read every customer in the estate.
--
-- organization.external_reference is granted to nobody: no statement in this repository uses it.
GRANT USAGE ON SCHEMA organization TO organization_provider_rt;
GRANT SELECT, INSERT, UPDATE ON organization.organization TO organization_provider_rt;

-- organization.provider_grant -- provider only: SELECT for the authority read every request makes
-- and the grant list, INSERT for the bootstrap and a grant, and UPDATE on the three revocation
-- columns alone. No DELETE, and no UPDATE of who was granted, by whom, when or why: a grant row is
-- the record of who was given cross-Tenant authority, and a revoked one stays as that record
-- (ADR-ORG-001 §5.11). The revocation's FOR UPDATE lock needs the column privilege as well.
GRANT SELECT, INSERT ON organization.provider_grant TO organization_provider_rt;
GRANT UPDATE (revoked_at, revoked_by, revoke_reason) ON organization.provider_grant TO organization_provider_rt;

-- organization.provider_grant.grant_version and organization.provider_grant_event -- provider only:
-- each published transition of a provider:identity-control grant advances the version and records
-- which version its event carries, in the transition's transaction (TDD-organization-control-001
-- §Provider Authority Projection). The history is written once and never rewritten.
GRANT UPDATE (grant_version) ON organization.provider_grant TO organization_provider_rt;
GRANT INSERT ON organization.provider_grant_event TO organization_provider_rt;

-- organization.provider_activation -- provider only: SELECT for the authority read every request
-- makes, INSERT for a request, and UPDATE on the decision and end columns alone (ADR-ORG-002). Who
-- asked, for which grant, for how long and why is never rewritten, and nothing deletes one.
GRANT SELECT, INSERT ON organization.provider_activation TO organization_provider_rt;
GRANT UPDATE (decided_by, decision, decision_reason, decided_at, ends_at, ended_by, end_reason, ended_at)
    ON organization.provider_activation TO organization_provider_rt;

-- organization.emergency_grant_use -- provider only: every request an emergency grant authorizes
-- records the grant's use, and the validation report reads it (ADR-ORG-002 §5.2). INSERT for the
-- first use, UPDATE of the last use and the count alone, SELECT for the report and for the count the
-- update adds to. Nothing deletes one: it is the evidence that the grant was validated.
GRANT SELECT, INSERT ON organization.emergency_grant_use TO organization_provider_rt;
GRANT UPDATE (last_used_at, uses) ON organization.emergency_grant_use TO organization_provider_rt;

-- projection.consumer -- provider only: the consumer registry, progress reports, the snapshot
-- mark, and the fresh check's metering. The resolver reads it through its own role, below.
GRANT USAGE ON SCHEMA projection TO organization_provider_rt;
GRANT SELECT, INSERT, UPDATE ON projection.consumer TO organization_provider_rt;

-- audit.privileged_access -- INSERT, provider only. The recorder writes on the provider
-- connections; the outcome record of a resolution is written by the resolution role, below.
--
-- Nothing reads it and nothing may change it. Evidence whose writer can amend it is not evidence,
-- and a tenant-scoped caller able to INSERT could attribute an access to somebody else. The
-- evidence carries no tenant_id and sits outside the RLS set by construction, so this grant is
-- its only boundary.
GRANT USAGE ON SCHEMA audit TO organization_provider_rt;
GRANT INSERT ON audit.privileged_access TO organization_provider_rt;

-- ---------------------------------------------------------------------------------------------
-- platform, by capability
-- ---------------------------------------------------------------------------------------------
--
-- Deny by default, then name what executes. Every grant below cites the path that needs it:
--
--   role -> execution path -> SQL operation -> table -> privilege
--
-- A privilege with no path is not granted, however harmless it looks. The dispatcher held
-- SELECT and UPDATE on platform.dead_letter for outbox/maintenance.go, which no deployable
-- calls; both runtime roles held full DML on platform.processed_event, which is a consumer's
-- inbox and which no line of code in this repository touches. Neither was a misjudgement about
-- what was needed -- nothing judged at all, because the grant was schema-wide.
--
-- Schema USAGE is granted and table privileges are not implied by it: a role with INSERT on a
-- table it cannot reach through its schema still cannot use it, so both halves appear here.

REVOKE ALL ON SCHEMA platform FROM PUBLIC;
GRANT USAGE ON SCHEMA platform TO organization_rt, organization_provider_rt;

-- Withdraws what earlier versions of this file granted schema-wide, including on a database
-- that has already run them. Stated before the grants below, not after, or it would remove
-- them again.
REVOKE ALL ON ALL TABLES    IN SCHEMA platform FROM organization_rt, organization_provider_rt;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA platform FROM organization_rt, organization_provider_rt;

-- The root cause, not the symptom. Without this, the next table foundation-platform adds is
-- handed to both runtime roles automatically and every per-table revoke above is a race we
-- lose one migration at a time.
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA platform
    REVOKE ALL ON TABLES FROM organization_rt, organization_provider_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA platform
    REVOKE ALL ON SEQUENCES FROM organization_rt, organization_provider_rt;

-- platform.outbox -- INSERT
--
-- organization_rt          -> membership, invitation, workspace, organization, tenant (x2),
--                             offboarding transitions -> outbox.Append -> INSERT
-- organization_provider_rt -> the same services under provider scope, and projection.reconcile
--
-- The append happens inside the caller's domain transaction, which is the whole point of the
-- outbox: the event and the state change commit together or neither does.
GRANT INSERT ON platform.outbox TO organization_rt;
GRANT INSERT ON platform.outbox TO organization_provider_rt;

-- platform.outbox_delivery -- INSERT, and platform.subscription -- SELECT on three columns
--
-- organization_rt, organization_provider_rt -> outbox.Append -> appendStatement
--
-- One statement writes the event and the delivery each subscriber is owed (ADR-GLB-018 §5.2). It
-- reads which consumers subscribe, and reads nothing back from the outbox, so neither role gains
-- a read of the outbox it appends to (§5.6). The three columns show which consumers exist and what
-- each receives, which is configuration rather than authority data.
GRANT INSERT ON platform.outbox_delivery TO organization_rt, organization_provider_rt;
GRANT SELECT (consumer, event_types, retired_at) ON platform.subscription TO organization_rt;

-- platform.subscription -- provider only, the rest of it
--
-- organization_provider_rt -> projection.Registry.Register -> outbox.Subscribe -> INSERT, and
--                             UPDATE retired_at on the subscription it replaces
--                          -> projection.Registry.Retire   -> outbox.Unsubscribe -> UPDATE retired_at
--                          -> projection.Registry.Get      -> selectConsumer     -> SELECT event_types
--
-- Column-level UPDATE: a subscription is replaced, never edited, so no role rewrites event_types.
GRANT SELECT, INSERT ON platform.subscription TO organization_provider_rt;
GRANT UPDATE (retired_at) ON platform.subscription TO organization_provider_rt;

-- platform.outbox_delivery -- SELECT and the abandonment's columns, provider only
--
-- organization_provider_rt -> projection.FrontierReader -> frontierStatement -> SELECT
--                          -> projection.SignalsReader  -> laneSignals       -> SELECT
--                          -> projection.Registry.Retire -> outbox.Abandon   -> UPDATE
--
-- The UPDATE is the six columns Abandon writes, so a retirement can close what the retired consumer
-- was owed (ADR-GLB-018 §5.5) and cannot touch anything that identifies the delivery.
GRANT SELECT ON platform.outbox_delivery TO organization_provider_rt;
GRANT UPDATE (published, failure_class, last_error, next_attempt_at, lease_id, leased_until)
    ON platform.outbox_delivery TO organization_provider_rt;

-- platform.outbox -- SELECT, provider only
--
-- organization_provider_rt -> projection.Publisher.Snapshot -> markStatement -> SELECT
--                          -> projection.FrontierReader     -> frontierStatement -> SELECT
--
-- No path was found that reads the outbox under tenant scope, so organization_rt does not get
-- it. If one exists that this derivation missed, the integration suite is where it surfaces.
GRANT SELECT ON platform.outbox TO organization_provider_rt;

-- platform.outbox_sequence -- outbox.Append takes its position from nextval in the same
-- statement, so the sequence travels with the INSERT. USAGE is what nextval needs; SELECT, which
-- only currval and lastval use, is not granted.
GRANT USAGE ON SEQUENCE platform.outbox_sequence
    TO organization_rt, organization_provider_rt;

-- platform.dead_letter -- SELECT, provider only
--
-- organization_provider_rt -> projection.FrontierReader -> unresolved security-debt facts
--
-- organization_rt gets nothing: no request-path code reads incident evidence. It previously
-- held DELETE here, which let the ordinary request path remove a record of an undelivered
-- security event rather than resolve it.
GRANT SELECT ON platform.dead_letter TO organization_provider_rt;

-- platform.idempotency_key
--
-- organization_rt          -> claimWithin inside WithTenantScope -> SELECT, INSERT
--                          -> db.ClaimStore.Complete, on the tenant connections -> UPDATE
-- organization_provider_rt -> claimWithin inside withProviderScope -> SELECT, INSERT
--
-- Deny-by-default is not deny-everything. This is the table that proves it: the claim store
-- runs on the tenant connections by design, so a rule of "no platform access for the request
-- path" would refuse every idempotent request and every Membership mutation in one deploy. The
-- provider role claims and never completes, so it holds no UPDATE.
GRANT SELECT, INSERT, UPDATE ON platform.idempotency_key TO organization_rt;
GRANT SELECT, INSERT         ON platform.idempotency_key TO organization_provider_rt;

-- platform.processed_event -- nothing, for either runtime role.
--
-- It is a consumer's deduplication inbox. inbox.Guard runs in foundation-reference, against
-- foundation-reference's own database. Referenced in this repository only by the ordering
-- guard at the top of this file, and by the comment below.

-- platform.delivery_receipt -- nothing, for either runtime role.
--
-- It is the root of trust for dead-letter resolution: a row saying this event reached this
-- consumer, and the only evidence in that contract not derived from the consumer's own report
-- about itself. A request path able to INSERT here could forge the proof that closes a security
-- debt, so forging the evidence and forging the resolution are the same act.
--
-- It arrived with the v0.2.5 bump, and it arrived closed. That is the default-privilege revoke
-- above doing the one thing it exists for: before it, this table would have been handed full DML
-- to both runtime roles by inheritance, with nothing failing and nothing logging.
--
-- The dispatcher's INSERT is granted with its other privileges below.

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

-- SELECT on the outbox, for the envelope, and SELECT and UPDATE on the deliveries, for the claim,
-- the lease and the outcome. Since foundation-platform v0.3.0 publication state is a delivery's
-- (ADR-GLB-018 §5.2), so the dispatcher no longer writes the outbox at all, and the UPDATE it held
-- there is withdrawn.
GRANT SELECT                    ON platform.outbox          TO organization_dispatch_rt;
REVOKE UPDATE                   ON platform.outbox          FROM organization_dispatch_rt;
GRANT SELECT, UPDATE            ON platform.outbox_delivery TO organization_dispatch_rt;

-- INSERT and SELECT on dead_letter. UPDATE is withdrawn; SELECT is required, and not for the
-- reason it looks like.
--
-- deadLetterStatement inserts into dead_letter and SELECTs from the outbox and the delivery, so the
-- obvious reading is that INSERT alone suffices. It does not. The statement ends
-- `ON CONFLICT ON CONSTRAINT dead_letter_delivery DO NOTHING` (it ended `ON CONFLICT (event_id)`
-- before v0.3.0), and a conflict target makes PostgreSQL demand SELECT on the table being inserted
-- into. Measured rather than reasoned, as the identical statement with
-- and without the clause, under this exact role:
--
--   INSERT ... VALUES (...)                                    -> INSERT 0 1
--   INSERT ... VALUES (...) ON CONFLICT (event_id) DO NOTHING  -> permission denied
--
-- Both this file and the review that prompted it had concluded INSERT was enough. The database
-- is the only party that was going to settle it, which is why the capability test in
-- dispatch_role_integration_test.go executes the statement instead of asserting the catalog.
--
-- UPDATE stays withdrawn: it was held for outbox/maintenance.go -- CountStaleUnresolvedDeadLetters
-- and DisposeResolvedDeadLetters -- which no deployable calls. Privilege granted for code that
-- does not run is privilege nothing can justify later.
GRANT INSERT, SELECT            ON platform.dead_letter TO organization_dispatch_rt;
REVOKE UPDATE                   ON platform.dead_letter FROM organization_dispatch_rt;

-- INSERT and SELECT on delivery_receipt. Same pair as dead_letter, and for the same reason.
--
-- recordReceiptStatement writes one row per delivery in the same transaction that marks the
-- outbox row published, so a receipt cannot exist for a delivery that rolled back. It ends
-- `ON CONFLICT (event_id, consumer) DO NOTHING`, and a conflict target makes PostgreSQL require
-- SELECT on the table being inserted into.
--
-- Measured for this table rather than carried over from the one above, which is the distinction
-- that matters: foundation-platform v0.2.5 shipped a preflight asserting INSERT alone here,
-- because the measurement already in hand for dead_letter was not repeated for the table beside
-- it. That check would have passed against a database where the next delivery failed.
--
--   INSERT ... ON CONFLICT (event_id, consumer) DO NOTHING
--     INSERT           -> permission denied
--     INSERT, SELECT   -> INSERT 0 1
--
-- Never UPDATE or DELETE. A delivery worker able to edit the evidence of its own deliveries is
-- one whose bug rewrites the record that would have shown it.
GRANT INSERT, SELECT            ON platform.delivery_receipt TO organization_dispatch_rt;

GRANT USAGE, SELECT             ON SEQUENCE platform.outbox_sequence TO organization_dispatch_rt;

-- No DELETE on the outbox. A dispatched row is marked published, never removed: retention is the
-- maintenance job's decision, and a worker able to delete is a worker whose bug is unrecoverable
-- because the evidence goes with it.
REVOKE DELETE ON platform.outbox, platform.outbox_delivery FROM organization_dispatch_rt;

-- Whether its own consumer name is registered and active, read once at startup.
--
-- DISPATCH_CONSUMER_NAME keys every receipt this worker writes, and the resolver reads receipts by
-- the registered consumer_id. A one-character mismatch produces receipts no resolution will ever
-- find, and nothing failed until an incident could not be closed. The dispatcher now refuses to
-- start when its name is not an active consumer (ROADMAP.md item 13).
--
-- Two columns, not the row. The registration's terms and the fresh-check meter are nothing a
-- delivery worker needs to read, and it writes nothing here.
--
--   SELECT EXISTS (SELECT 1 FROM projection.consumer WHERE consumer_id = $1 AND retired_at IS NULL)
GRANT USAGE ON SCHEMA projection TO organization_dispatch_rt;
GRANT SELECT (consumer_id, retired_at) ON projection.consumer TO organization_dispatch_rt;

-- And no default privileges: a table added to platform or projection later must be granted
-- deliberately rather than inherited by a role whose scope is a handful of objects.
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA platform
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_dispatch_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA projection
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_dispatch_rt;

-- ---------------------------------------------------------------------------------------------
-- The resolution role
-- ---------------------------------------------------------------------------------------------
--
-- Closing a dead letter is what makes the frontier stop reporting a security debt, which is what
-- lets every consumer serve again. It is the estate's most consequential operator act, and it is
-- the only one with its own credential.
--
-- Separate from the provider role on purpose. The provider role replays an abandoned delivery and
-- holds no UPDATE here, so it cannot close what it just put back on the wire. One credential
-- performing both would make replay and "declare it delivered" the same act, and the evidence
-- between them decorative.

GRANT USAGE ON SCHEMA platform, projection, audit, membership, tenant, organization TO organization_resolution_rt;

-- Column-level UPDATE, not table-level.
--
-- The resolver writes four columns and must not be able to touch the rest of the row. Table-level
-- UPDATE would let it rewrite failure_class, envelope, payload or dead_lettered_at -- the record
-- of what went wrong -- while closing the incident that records it. An operator able to edit the
-- account of a failure and close it in one statement leaves nothing an investigation can read.
--
-- SELECT on the whole row, because the predicate has to read the incident before it decides.
GRANT SELECT ON platform.dead_letter TO organization_resolution_rt;
GRANT UPDATE (resolved_at, resolution_type, resolved_by, resolution_reference)
    ON platform.dead_letter TO organization_resolution_rt;

-- The waiver, on its own four columns. A waiver is an operational exception, never a closure, and
-- it is kept in columns the frontier's debt query does not read, so this grant cannot make an
-- incident look delivered. projection.Resolver.Waive refuses the cases that would hide a live
-- outage; see TDD-organization-control-005 §WAIVED.
GRANT UPDATE (waived_at, waived_until, waived_by, waiver_reason)
    ON platform.dead_letter TO organization_resolution_rt;

-- The evidence, read-only. A resolver that could write receipts could manufacture the proof it
-- then consumes, which is the whole predicate defeated in one grant.
GRANT SELECT ON platform.delivery_receipt TO organization_resolution_rt;

-- Whose receipt counts. The active projection consumer is derived from this table server-side
-- rather than accepted from the request: the operator chooses the action, the server chooses the
-- subject the evidence must be about.
GRANT SELECT ON projection.consumer TO organization_resolution_rt;

-- The idempotency claim. claimWithin runs inside every scoped transaction, the resolution one
-- included, so a /resolve carrying an Idempotency-Key claims it under this role. idempotency.Claim
-- is INSERT ... ON CONFLICT DO NOTHING, whose conflict target requires SELECT, followed by a
-- SELECT of the stored row. No UPDATE: completion runs on the tenant connections, not here. This
-- grant was missing, and a keyed /resolve failed with permission denied; no test sent the header.
GRANT SELECT, INSERT ON platform.idempotency_key TO organization_resolution_rt;

-- The outcome record, written inside the resolution transaction so it exists if and only if the
-- closure does. The attempt record, written before that transaction opens so a failed attempt is
-- still attributable, goes through the provider pool's recorder and does not use this grant.
GRANT INSERT ON audit.privileged_access TO organization_resolution_rt;

-- Which Membership, at which version, a receipted event carries: the domain half of the SUPERSEDED
-- predicate. Read-only, and on this one table of the schema; rls.sql gives it the matching policy.
GRANT SELECT ON membership.membership_event TO organization_resolution_rt;

-- And which Tenant, at which security version: the Tenant half of the same predicate.
GRANT SELECT ON tenant.tenant_event TO organization_resolution_rt;

-- And which provider grant, at which grant_version: the provider grant third of it.
GRANT SELECT ON organization.provider_grant_event TO organization_resolution_rt;

-- Nothing inherited, for the same reason as the dispatcher: a table added later must be granted
-- deliberately rather than arrive in the hands of a role whose scope is four objects.
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA platform
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_resolution_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA projection
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_resolution_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA audit
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_resolution_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA membership
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_resolution_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA tenant
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_resolution_rt;

-- ---------------------------------------------------------------------------------------------
-- The consumer role
-- ---------------------------------------------------------------------------------------------
--
-- A registered projection consumer acting on its own records, through eight routes: its registry
-- row, progress, bootstrap, the Organization and provider authority snapshots, the frontier, and the
-- two context checks. It ran as the
-- provider role before this, and so could read and write every table the control plane can.
-- Every grant below is one tools/grantcheck derives from those routes.

GRANT USAGE ON SCHEMA membership, tenant, projection, platform, organization TO organization_consumer_rt;

-- The snapshot's rows and the fresh check's answer. Read-only; rls.sql gives both the matching
-- policy. No other business table: not an invitation, an offboarding, an Organization or a
-- Workspace.
GRANT SELECT ON membership.membership TO organization_consumer_rt;
GRANT SELECT ON tenant.tenant TO organization_consumer_rt;

-- The provider authority snapshot's rows, for a consumer subscribed to the provider grant events:
-- what a grant confers, by column, and nothing of why it was made, by whom, or the requests behind
-- an activation (TDD-organization-control-001 §Provider Authority Projection).
GRANT SELECT (grant_id, principal_id, scope, kind, revoked_at, grant_version)
    ON organization.provider_grant TO organization_consumer_rt;
GRANT SELECT (activation_id, grant_id, decision, ended_at, ends_at)
    ON organization.provider_activation TO organization_consumer_rt;

-- Its own registry row: read, and four columns written. The snapshot mark (bootstrap), the
-- reported position (progress), and the fresh-check meter. Column-level so it cannot change its
-- declared terms -- max_accepted_age, stale_behavior -- or un-retire itself. Which row is its own is
-- decided by the transport layer, which admits a consumer only for itself.
GRANT SELECT ON projection.consumer TO organization_consumer_rt;
GRANT UPDATE (snapshot_mark, last_reported_mark, last_reported_at, verify_calls_since_report)
    ON projection.consumer TO organization_consumer_rt;

-- The snapshot's high-water mark and the frontier: outbox positions and unresolved dead letters.
-- Read-only. A consumer that could write either could forge the facts its own freshness is judged by.
GRANT SELECT ON platform.outbox TO organization_consumer_rt;
GRANT SELECT ON platform.dead_letter TO organization_consumer_rt;

-- Its own owed deliveries, for its frontier, and its subscription's types, for its registry row.
-- The frontier filters to the consumer's own deliveries; reading the table is what that filter
-- runs on, and it writes neither.
GRANT SELECT ON platform.outbox_delivery TO organization_consumer_rt;
GRANT SELECT (consumer, event_types, retired_at) ON platform.subscription TO organization_consumer_rt;

-- The idempotency claim, for the reason the resolution role holds it: claimWithin runs inside
-- every recorded scope, so a keyed request claims its key under this role.
GRANT SELECT, INSERT ON platform.idempotency_key TO organization_consumer_rt;

-- Nothing inherited: a table added later must be granted deliberately.
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA platform
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_consumer_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA projection
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_consumer_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA membership
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_consumer_rt;
ALTER DEFAULT PRIVILEGES FOR ROLE organization_migrator IN SCHEMA tenant
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM organization_consumer_rt;

