---
doc_meta:
  id: TDD-organization-control-003
  title: Organization, Tenant, and Workspace Lifecycle
  owner: Core Platform Team
  version: 1.2.0
  status: approved
  classification: restricted
  review_cycle_days: 90
  created_date: 2026-08-11
  last_reviewed: 2026-08-23
  parent_sad: SAD-004
---

# Organization, Tenant, and Workspace Lifecycle

## Purpose

Specify the three authoritative aggregates this service owns before Membership can
reference them: the Organization registry, the Tenant state machine, and the Workspace
lifecycle bound to one Tenant.

`TDD-organization-control-002` writes a composite foreign key into
`workspace.workspace (tenant_id, workspace_id)` and carries `tenant_security_version` in
every Membership event. Neither the tables nor the two objects those depend on were
designed anywhere. This design supplies them, and it is written before the first migration
rather than during it.

**This design is the sole declaring authority for `tenant.tenant` and
`workspace.workspace`,** including `tenant_security_version` and the
`UNIQUE (tenant_id, workspace_id)` constraint. Version 0.3.0 of
`TDD-organization-control-002` declared both as well; applied as SQL in the order the two
designs state, the second declaration fails and the migration stops. That design now
records the dependency instead of restating the declaration, and this section is where a
reader settles which document to change.

This design is also the authoritative list of which transitions increment
`tenant_security_version` — see §"Security Version Increments". No Membership operation
appears on it.

## Scope

**In scope**

- Organization registry: identity, classification, status, relationships, external
  references.
- Tenant state machine, including the provisioning gate before activation.
- Workspace lifecycle within exactly one Tenant, and the constraint Membership depends
  on.
- Which transitions increment `tenant_security_version` and why.
- Provisioning coordination and the handling of an ambiguous provisioning outcome.

**Out of scope**

- Membership, versions, and revocation — owned by `TDD-organization-control-002`.
- Row-Level Security and the two runtime roles — owned by
  `TDD-organization-control-001`.
- Invitation and offboarding obligations — owned by `TDD-organization-control-004`.
- Physical provisioning of infrastructure, which is external to this system.

## Technical Context

Four concepts, three owned here, and the distinctions matter because collapsing any two
of them is the failure ADR-ORG-001 §5.2 was written to prevent:

| Concept | Is | Is not |
| :-- | :-- | :-- |
| Organization | A party in the ecosystem: provider, customer, partner, publisher | A Tenant, a Subscriber Account, or a BPO Client Account |
| Tenant | A technical isolation and operating boundary | The legal customer, the commercial subscriber, a Product, or a deployment |
| Workspace | A collaboration or operating context inside one Tenant | An HCM department, a BPO workstream, a Product, or an Application |
| Membership | A Principal-to-context relationship | Entitlement, permission, or employment |

An Organization may sponsor several Tenants. A Tenant belongs to exactly one sponsoring
Organization. A Workspace belongs to exactly one Tenant and never moves.

## Component Design

| Component | Package | Responsibility |
| :-- | :-- | :-- |
| `OrganizationService` | `internal/organization` | Registry, classification, relationships, external references |
| `TenantService` | `internal/tenant` | Tenant state machine, security version, desired provisioning profile |
| `WorkspaceService` | `internal/workspace` | Workspace lifecycle inside one Tenant |
| `ProvisioningCoordinator` | `internal/tenant` | Desired-state publication and realized-status correlation |

### Tenant State Machine

```mermaid
stateDiagram-v2
    [*] --> requested
    requested --> provisioning: prerequisites validated
    provisioning --> active: realized status confirmed
    provisioning --> failed: provisioning refused
    failed --> provisioning: retried
    active --> suspended: suspend
    suspended --> active: restore
    active --> offboarding: begin offboarding
    suspended --> offboarding: begin offboarding
    offboarding --> retired: all obligations complete
    retired --> [*]
```

A Tenant is not `active` until provisioning confirms, per SAD-004 §5.1. Activating on
request would mean Memberships could be granted into a Tenant whose isolation boundary
does not exist yet.

`retired` is terminal. There is no transition out of it, because the identifiers of a
retired Tenant have been released to consumers as retired and reviving one would make
a downstream projection wrong in a way no reconciliation would detect.

#### Who issues which transition

The machine above is declared once and here. The commands that drive it are split, and the
split is not arbitrary:

| Transition | Command owned by |
| :-- | :-- |
| `requested -> provisioning`, `provisioning -> failed`, `failed -> provisioning` | `ProvisioningCoordinator`, from correlated realized status |
| `provisioning -> active` | `TenantService.Activate` |
| `active -> suspended`, `suspended -> active` | `TenantService.Suspend` / `.Restore` |
| `active -> offboarding`, `suspended -> offboarding` | `OffboardingService`, `TDD-organization-control-004` |
| `offboarding -> retired` | `OffboardingService`, `TDD-organization-control-004` |

The last two are transitions here and commands there because each is a stage of a process
that does more than move this row: entering offboarding also creates an
`operation.offboarding` record, raises obligations across domains, and suspends every
Membership in the Tenant, and retirement is refused while any obligation is open or a legal
hold is set. A command in `TenantService` that moved only the Tenant row would look
complete and leave access running, which is why `TenantService` exposes neither and
`internal/tenant` is given no dependency on `internal/membership`.

Keeping the transitions themselves in one table is what makes that split safe: the
refusal rules, the security-version consequences, and the timestamps do not fork between
two services.

### Organization Lifecycle

Deliberately simpler, and deliberately not coupled to Tenant:

```text
active ──► suspended ──► active
   │
   └─────► retired
```

**Retiring an Organization does not retire its Tenants.** A cascade here would take an
irreversible action on isolation boundaries as a side effect of a registry change. An
Organization with Tenants that are not retired cannot itself be retired; the refusal
names the Tenants, and the operator retires them deliberately.

### Workspace Lifecycle

```text
active ──► archived ──► retired
```

A Workspace never moves between Tenants. Its Tenant is fixed at creation and is part of
the key that Membership references, so a move would silently reassign every Membership
scoped to it.

## Data Model

### Organization

```sql
CREATE TABLE organization.organization (
    organization_id     UUID        PRIMARY KEY,
    display_name        TEXT        NOT NULL,
    classification      TEXT        NOT NULL,
    status              TEXT        NOT NULL,
    parent_id           UUID        REFERENCES organization.organization(organization_id),
    version             BIGINT      NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT organization_classification_check
        CHECK (classification IN ('provider', 'customer', 'partner', 'publisher')),
    CONSTRAINT organization_status_check
        CHECK (status IN ('active', 'suspended', 'retired'))
);

CREATE TABLE organization.external_reference (
    organization_id UUID  NOT NULL REFERENCES organization.organization(organization_id),
    authority       TEXT  NOT NULL,
    external_id     TEXT  NOT NULL,
    PRIMARY KEY (organization_id, authority)
);
```

`external_reference` holds identifiers owned elsewhere — a Subscriber Account, a BPO
Client Account, a CRM record. It stores the authority name and the opaque identifier
and nothing else. Copying attributes from those systems would make this registry a
stale mirror of records it does not own, which EAD-003 §6.2 prohibits.

`parent_id` supports internal hierarchy only. It carries no Tenant implication: a child
Organization does not inherit its parent's Tenants.

### Tenant

```sql
CREATE TABLE tenant.tenant (
    tenant_id               UUID        PRIMARY KEY,
    organization_id         UUID        NOT NULL REFERENCES organization.organization(organization_id),
    display_name            TEXT        NOT NULL,
    status                  TEXT        NOT NULL,
    isolation_profile       TEXT        NOT NULL,
    residency_region        TEXT,
    tenant_security_version BIGINT      NOT NULL DEFAULT 1,
    version                 BIGINT      NOT NULL DEFAULT 1,
    activated_at            TIMESTAMPTZ,
    suspended_at            TIMESTAMPTZ,
    offboarding_started_at  TIMESTAMPTZ,
    retired_at              TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tenant_status_check
        CHECK (status IN ('requested','provisioning','active','failed','suspended','offboarding','retired')),
    CONSTRAINT tenant_isolation_check
        CHECK (isolation_profile IN ('pooled', 'bridge', 'silo', 'regional'))
);
```

`isolation_profile` records which of the EAD-005 §5.3 multi-tenant deployment profiles
applies. It is a reference for provisioning and a fact for audit; this service does not
implement isolation, it records which profile was chosen.

`organization_id` is a plain foreign key rather than a composite, because a Tenant
belongs to one Organization and that binding is fixed at creation.

The four timestamp columns record the *current* position, not a history. Each transition
stamps its own — `activated_at`, `suspended_at`, `offboarding_started_at`, `retired_at` —
and **restore sets `suspended_at` back to NULL**. Left populated it would make a restored
Tenant indistinguishable from a suspended one to every report and alert that filters on
the column, and that is the reading someone will take, because a non-null timestamp named
`suspended_at` says the Tenant is suspended. The history of past suspensions belongs to the
event stream, which is the record that is supposed to be append-only; a mutable column is
the wrong place to accumulate one.

Each stamp carries the accepted instant of the transition rather than `now()` evaluated
independently, so the lifecycle fact in the row and the `time` in the published envelope
are the same instant. `updated_at` keeps `now()`: it is database housekeeping and answers a
different question.

`version` increments on every transition, including the ones that publish nothing.
`tenant_security_version` increments only where the table in
§"Security Version Increments" says so. The two are separate because they answer separate
questions — `version` orders two events about this row and backs the optimistic check,
`tenant_security_version` decides whether a token a consumer is holding is stale. Carrying
only the second would leave a restore-then-suspend pair with the same value on one of the
two events and no ordering between them.

### Workspace

```sql
CREATE TABLE workspace.workspace (
    workspace_id   UUID        PRIMARY KEY,
    tenant_id      UUID        NOT NULL REFERENCES tenant.tenant(tenant_id),
    display_name   TEXT        NOT NULL,
    workspace_type TEXT        NOT NULL,
    status         TEXT        NOT NULL,
    version        BIGINT      NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT workspace_status_check
        CHECK (status IN ('active', 'archived', 'retired'))
);

-- Required as the target of the composite foreign key in
-- TDD-organization-control-002. Without it that constraint cannot be created and
-- the same-Tenant invariant on Membership is unenforced.
ALTER TABLE workspace.workspace
    ADD CONSTRAINT workspace_tenant_scope_unique UNIQUE (tenant_id, workspace_id);
```

The `UNIQUE (tenant_id, workspace_id)` looks redundant beside a primary key on
`workspace_id` alone, and it is not. PostgreSQL requires a composite foreign key to
reference a uniquely constrained column set, so this constraint is what allows
`membership.membership` to enforce that a referenced Workspace belongs to the
Membership's Tenant. Dropping it as apparent redundancy silently removes that
invariant, which is why the migration test asserts its presence.

### Provisioning Correlation

```sql
CREATE TABLE tenant.provisioning_request (
    request_id      UUID        PRIMARY KEY,
    tenant_id       UUID        NOT NULL REFERENCES tenant.tenant(tenant_id),
    desired_profile JSONB       NOT NULL,
    state           TEXT        NOT NULL,
    correlation_id  UUID        NOT NULL,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ,
    detail          TEXT,
    CONSTRAINT provisioning_state_check
        CHECK (state IN ('requested', 'realized', 'failed', 'unresolved'))
);
```

`unresolved` is a first-class state, not an error code. SAD-004 §7.5 requires an
ambiguous provisioning outcome to remain pending or failed and never to be inferred as
success. A timeout after the request left is `unresolved`, and reconciliation resolves
it later. Treating a timeout as failure and retrying would provision twice.

## API / Interface

```text
POST   /v1/organizations
GET    /v1/organizations/{organization_id}
POST   /v1/organizations/{organization_id}:suspend
POST   /v1/organizations/{organization_id}:restore
POST   /v1/organizations/{organization_id}:retire

POST   /v1/tenants
GET    /v1/tenants/{tenant_id}
POST   /v1/tenants/{tenant_id}:activate
POST   /v1/tenants/{tenant_id}:suspend
POST   /v1/tenants/{tenant_id}:restore

POST   /v1/tenants/{tenant_id}/workspaces
GET    /v1/tenants/{tenant_id}/workspaces/{workspace_id}
POST   /v1/tenants/{tenant_id}/workspaces/{workspace_id}:archive
POST   /v1/tenants/{tenant_id}/workspaces/{workspace_id}:retire
```

Every mutation requires an `Idempotency-Key`, the optimistic `version` of the record the
caller was shown, an authenticated actor, and a reason where the operation is
provider-scoped. A version mismatch returns `409` rather than retrying, because the
caller acted on a view that has since changed.

Errors are RFC 7807 problem documents from `foundation-platform`.

### Every Tenant transition is provider-scoped

`TenantService` binds to the provider pool, not the tenant-scoped one, and this is forced
rather than preferred. A Tenant does not activate, suspend, or restore itself: the decision
belongs to the provider. More concretely, `organization_rt` holds no `SELECT` on
`organization.organization` at all — `TDD-organization-control-001` revokes it, because a
tenant-scoped caller with that grant could read every customer in the estate — so the
activation precondition on the sponsoring Organization is not evaluable on a tenant-scoped
connection. The provider binding is the only one under which the checks this design
requires can run.

That binding brings its obligations with it rather than as a separate discipline.
`db.WithProviderScope` refuses a blank reason and refuses to proceed when the
privileged-access record cannot be written, so every Tenant transition carries an actor, a
correlation identifier, and a reason as recorded evidence — PAD-PLT-002 §3.3 invariant 22 —
and the evidence is written *before* the transaction runs. Evidence written on the way out
is missing for exactly the cases an investigation asks about, because a transaction that
panics or is killed mid-flight never reaches its own epilogue.

### The optimistic check lives in the service, not only at the edge

The `version` requirement above is stated for the HTTP surface, and it is enforced one
layer lower: the service refuses a command whose expected version does not match the
locked row. A check that only the edge performs is a check the next caller of the same
method does not perform, and the case it protects — two operators acting on one Tenant
from two stale views — is precisely the case where the second write silently wins.

The order of the two refusals is deliberate. The state machine is consulted first and the
version second, because a caller acting on a stale view usually has both wrong, and
"restore is not permitted from active" tells an operator what happened where "version 4 is
not version 5" tells them only that something did.

### Published Events

```text
com.scnehaux.organization.organization.registry.created
com.scnehaux.organization.organization.registry.suspended
com.scnehaux.organization.organization.registry.retired
com.scnehaux.organization.tenant.lifecycle.requested
com.scnehaux.organization.tenant.lifecycle.activated
com.scnehaux.organization.tenant.lifecycle.retired
com.scnehaux.organization.tenant.security.suspended     (priority)
com.scnehaux.organization.tenant.security.restored      (priority)
com.scnehaux.organization.workspace.lifecycle.created
com.scnehaux.organization.workspace.lifecycle.archived
com.scnehaux.organization.workspace.lifecycle.retired
```

Suspension and restoration are priority events because both invalidate cached context:
suspension removes it, restoration makes a cached denial wrong.

`tenant.security.suspended` is the security consequence event, not a one-to-one mirror
of the lifecycle state name. It is emitted both for `active -> suspended` and when an
`active` or already-suspended Tenant enters `offboarding`; in both cases every existing
Tenant context must stop. The event carries the incremented `tenant_security_version`.

Two transitions therefore share one event type, deliberately. A consumer that must tell
them apart reads `tenant_status` out of the payload — which is present for exactly this
reason, and is why "one event type per action" is an invariant for Membership and not for
Tenant.

**The three provisioning transitions publish nothing.** `requested -> provisioning`,
`provisioning -> failed`, and `failed -> provisioning` are silent, and the silence is part
of the specification rather than an omission from the list above: no context exists to
invalidate inside a Tenant that has never been active, and no consumer holds a projection
of a Tenant that has never existed to it. An event here would carry a state change nobody
outside this service can act on. The implementation declares the silent set explicitly and
asserts that every action is either published or declared silent, so a transition added
later cannot become quiet by default.

**Retirement takes the standard lane and increments `tenant_security_version`.** The
pairing looks inconsistent and is not. The only way into `retired` is from `offboarding`,
which already published `tenant.security.suspended` on the priority lane and already froze
context, so by the time a Tenant retires there is no access left to withdraw — the urgency
was discharged one transition earlier. The increment is still correct: the version is
monotonic and every context is now permanently invalid.

What is enforced instead of a version-to-lane rule is that the lane agrees with the
event's own classification. The fifth segment of the type carries the class, so a type
containing `security` and a standard-lane append cannot coexist: an event that tells a
consumer it is urgent while sitting behind a lifecycle backlog is a lie the consumer has
no way to detect.

## Algorithms / Logic

### Security Version Increments

`tenant_security_version` is the cheap staleness test consumers use without a remote
call, so it increments on every transition that invalidates cached context — in either
direction:

| Transition | Increments | Why |
| :-- | :-- | :-- |
| `active → suspended` | Yes | Every cached context in the Tenant is now invalid |
| `suspended → active` | Yes | Every cached denial is now wrong |
| `active → offboarding` | Yes | Context is frozen |
| `suspended → offboarding` | Yes | A new terminal process boundary must invalidate every cached version |
| `* → retired` | Yes | Every context is permanently invalid |
| `requested → provisioning → active` | No | No context existed to invalidate |
| Display name change | No | Carries no security consequence |

Incrementing on restore is the one that is easy to omit and expensive to omit. A
consumer that cached "suspended" and never sees a version change keeps denying a Tenant
that has been restored, and the symptom presents as a support ticket rather than as a
projection failure.

### Tenant Activation

```text
BEGIN
    load tenant FOR UPDATE
    reject if status is not 'provisioning'
    reject if the caller's expected version is not the stored version
    reject if the most recent provisioning request is not 'realized'
    reject if the sponsoring Organization is not 'active'
    set status = 'active', activated_at = accepted_at
    version = version + 1
    outbox.Append(tenant.lifecycle.activated)
COMMIT
```

The Organization check is at activation rather than at creation. A Tenant may be
requested while its Organization is still being onboarded; it may not become active
under a suspended or retired sponsor.

Both preconditions are evaluated inside the transaction that performs the update, after
the row is locked. Evaluated before it they would be checks against a state that can change
before the write lands — for the sponsor check, a Tenant going active under an Organization
suspended a moment earlier.

**The provisioning check reads the most recent attempt, not any attempt.** A failed attempt
followed by a successful retry must activate, and a realized attempt followed by a later
failure must not. `EXISTS (... AND state = 'realized')` satisfies the first case and gets
the second one wrong in the permissive direction, which is the direction that activates a
Tenant whose boundary was torn down. The ordering key is `requested_at` with the request
identifier as a tiebreaker, so two attempts recorded in the same instant still order
deterministically.

A Tenant with no provisioning request at all is refused for the same reason and with the
same error as one whose request is unrealized. From the caller's side "provisioning has not
confirmed" is true either way, and the distinction between "never requested" and "requested
and pending" belongs in the operator's view of the Tenant rather than in the refusal.

### Organization Retirement Refusal

```text
retire(organization):
    count tenants where organization_id = this and status != 'retired'
    if count > 0:
        refuse with 409, naming the tenants
    set status = 'retired'
```

The refusal names the Tenants rather than reporting a count. An operator who has to
find them will retire the wrong one.

### Provisioning Correlation

```text
on tenant creation:
    persist tenant as 'requested'
    persist provisioning_request as 'requested' with a correlation identifier
    publish desired state

on realized status:
    match by correlation identifier
    set provisioning_request to 'realized'
    advance tenant to 'provisioning' complete, awaiting explicit activation

on timeout with no status:
    set provisioning_request to 'unresolved'
    do not retry automatically
    reconciliation queries the provisioning system and resolves it
```

An unresolved request is never retried automatically. Retrying an operation whose
outcome is unknown is how a Tenant gets provisioned twice, and EAD-004 §6.6 requires
critical mutations to define duplicate protection at the business boundary rather than
at the transport.

## Configuration

| Variable | Default | Purpose |
| :-- | :-- | :-- |
| `ORGANIZATION_PROVISIONING_TIMEOUT` | `30m` | Age at which a provisioning request becomes `unresolved` |
| `ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL` | `15m` | Cadence for resolving unresolved requests |
| `ORGANIZATION_TENANT_NAME_MAX` | `120` | Display name bound |

## Testing Strategy

### State Machines

- Every transition outside each diagram is refused.
- A Tenant cannot become `active` from `requested` without passing `provisioning`.
- A Tenant cannot become `active` under a suspended or retired Organization.
- `retired` is terminal for Tenant, Organization, and Workspace.

### Constraints

- `workspace.workspace` carries `UNIQUE (tenant_id, workspace_id)`; dropping it fails
  the migration test.
- A Membership referencing a Workspace of another Tenant is rejected by the composite
  foreign key from `TDD-organization-control-002`.
- A Workspace cannot be reassigned to another Tenant.
- An Organization with a non-retired Tenant cannot be retired, and the refusal names
  the Tenants.

### Security Version

- Suspension increments `tenant_security_version`.
- **Restoration increments it.**
- Offboarding and retirement increment it.
- Entering offboarding emits `tenant.security.suspended` with the incremented version,
  including when the prior lifecycle state was already `suspended`.
- Provisioning transitions and display-name changes do not.
- The version never decreases.
- A Membership transition does not increment it. The Membership event reads it and carries
  it; `TDD-organization-control-002` §"Revocation" states why.
- A rolled-back transition does not increment it. Asserted by injecting a failure between
  the status change and the outbox append: the status, `version`,
  `tenant_security_version`, and the outbox are all unchanged afterwards, and the same
  transition then succeeds once the injection is removed.

### Lifecycle Timestamps and Lanes

- `restore` sets `suspended_at` back to NULL.
- Each stamped timestamp equals the `time` in the event the same transition published.
- The three provisioning transitions publish nothing, and an action that is neither
  published nor declared silent fails the test rather than defaulting to quiet.
- An event type whose class segment is `security` is appended to the priority lane, and one
  whose class is `lifecycle` is not — asserted for every action rather than per event.

### Provisioning

- A timeout with no status produces `unresolved`, not `failed`.
- An unresolved request is not retried automatically.
- A realized status arriving after the timeout resolves the request by correlation.
- A duplicate realized status produces one effect.

### Concurrency

- A stale `version` on any mutation returns `409`.
- Two concurrent activations of the same Tenant produce one activation.

## Security Notes

This design creates the isolation boundaries the rest of the platform enforces. A
Tenant that becomes active before its boundary exists would accept Memberships into
nothing, so the provisioning gate is a security control and not a workflow nicety.

Cascading retirement is deliberately absent. An irreversible action taken as a side
effect of a registry change is the shape of an accidental mass outage, and the refusal
that replaces it costs an operator one extra deliberate step.

`external_reference` stores an authority name and an opaque identifier. Copying
attributes from Subscriber Account, Client Account, or CRM records would place data
this domain does not own inside its authority, which EAD-003 §6.1 prohibits.

## Performance Notes

All three aggregates are low-volume and low-churn relative to Membership. Reads are
served from indexes on the natural access paths: Tenants by Organization, Workspaces by
Tenant.

The provisioning reconciler queries only unresolved requests, so its cost is
proportional to the ambiguity in flight rather than to Tenant count.

## Operational Notes

| Signal | Warning | Critical |
| :-- | :-- | :-- |
| Provisioning requests in `unresolved` | any occurrence | older than two reconcile intervals |
| Tenants stuck in `provisioning` | 1 hour | 4 hours |
| Organization retirement refused | any occurrence | — |
| `tenant_security_version` unchanged across a suspend or restore | — | any occurrence |

The last signal is a correctness assertion expressed as an alert. A suspension that
does not move the version leaves every consumer holding a projection it has no way to
know is stale.

Runbooks required before production: stuck provisioning, unresolved provisioning
resolution, and Tenant activation refused.

## Traceability

| Relationship | Target |
| :-- | :-- |
| Parent system | SAD-004 — Scnehaux Organization Control |
| Realizes capability | PAD-PLT-002 — Organization & Tenancy Platform |
| Governed by | ADR-ORG-001 §5.2 — Organization, Subscriber Account, Client Account, Tenant, and Workspace remain distinct |
| Conforms to | EAD-005 §5.3 — pooled, bridge, silo, and regional isolation profiles |
| Enterprise constraint | EAD-003 — one authority per fact; an external reference does not copy the record |
| Enterprise constraint | EAD-004 §6.6 — critical mutations define duplicate protection at the business boundary |
| Depends on | `TDD-foundation-platform-001` — outbox, envelope, idempotency |
| Consumed by | `TDD-organization-control-002` — Membership references `tenant.tenant` and `workspace.workspace` |
| Consumed by | `TDD-organization-control-004` — offboarding drives the Tenant terminal transitions |
