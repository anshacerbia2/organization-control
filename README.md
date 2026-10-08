# Organization Control

Go control-plane service and the enterprise authority for Organization, Tenant,
Workspace, Membership, and trusted operating context. It realizes **SAD-004 Scnehaux
Organization Control** under **PAD-PLT-002 Organization & Tenancy Platform**.

## What this service owns

- Organization identity, classification, status, and tenancy-relevant relationships.
- Tenant identity, lifecycle, and `tenant_security_version`.
- Workspace identity and lifecycle within one Tenant.
- Membership between a Principal or workload and a Tenant or optional Workspace.
- Operating-context eligibility and context-switch validation.
- Invitation intent and onboarding correlation.
- Tenant suspension, restoration, offboarding, and retirement coordination.
- Projection publication: snapshots, high-water marks, consumer registry, and
  reconciliation.

## What it does not own

Authentication, credentials, sessions, tokens, federation, and protocol trust belong
to the identity kernel. Product permissions, entitlements, and business roles belong to
their owning domains. Membership establishes contextual relationship and
tenancy-administrative authority — nothing more, per ADR-ORG-001 §5.6.

**This service never calls Keycloak.** ADR-ORG-001 §5.4 prohibits Organization from
writing to Keycloak or its database, and the prohibition is structural here: this
process holds no Keycloak credential and has no network route to it. Membership reaches
Keycloak only as

```text
organization-control → canonical event → identity-control → Keycloak Admin API
```

## One runtime role per kind of caller, deliberately

The Organization Database is pooled, so isolation is enforced by PostgreSQL as well as
by the application:

| Role | Scope | Pool |
| :-- | :-- | :-- |
| `organization_rt` | Exactly one Tenant, bound per transaction | Tenant-scoped |
| `organization_provider_rt` | Deliberately cross-Tenant | Provider |
| `organization_consumer_rt` | A registered consumer's own seven routes: reads Memberships and Tenants, writes its own position | Consumer, when consumer authority is configured |
| `organization_resolution_rt` | Closing and waiving dead letters (TDD-005) | Resolution |
| `organization_self_rt` | A person reading their own contexts: three tables' named columns, one Principal | None of its own: `SET LOCAL ROLE` on the tenant connections |

A single role serving both tenant and provider work cannot be constrained: any policy
permissive enough for provider work is permissive enough for a defect in a tenant-scoped
path. Separating them at the role level makes a cross-tenant read attributable at the
connection. The consumer is the same argument for the other deployable: it ran as the
provider role, and its credential could do everything the control plane can.

No runtime role owns a table, holds `SUPERUSER`, holds `BYPASSRLS`, or holds any DDL
privilege. Row-Level Security is enabled and forced on every tenant-scoped table, and
`TDD-organization-control-001` specifies the rest.

## Governance lineage

```text
PAD-PLT-002                  Organization & Tenancy Platform
    ↓
SAD-004                      Scnehaux Organization Control
    ↓
TDD-organization-control-*   Technical designs   (docs/designs)
    ↓
Source code
```

## Repository map

| Repository | Role |
| :-- | :-- |
| `identity-kernel` | Keycloak extensions, realm configuration, login theme, image build |
| `identity-control` | Identity Control Service, holds the Keycloak Admin credential |
| **`organization-control`** | **This service** |
| `foundation-platform` | Shared Go substrate |
| `identity-experience` | Identity administration UI and its BFF |
| `organization-experience` | Organization administration UI and its BFF |

Cross-system state moves only through versioned domain events on the broker. Where an
authoritative answer is required, a consumer calls the published HTTP contract that
every other consumer uses; sharing a foundation grants no privileged interface.

## Layout

| Path | Contents |
| :-- | :-- |
| `cmd/organization-control/` | Deployable entrypoint and composition root |
| `cmd/organization-migrate/` | Schema stages: roles, then platform, RLS, and privileges |
| `cmd/organization-devissuer/` | Local token issuer, `//go:build devissuer` only |
| `Makefile`, `.env.example` | Development entry points and the local environment |
| `internal/config/` | Environment configuration, read once at startup |
| `internal/httpapi/` | Routing, request decoding, and the domain-error-to-problem mapping |
| `internal/access/` | The privileged-access recorder: evidence for cross-Tenant work |
| `internal/db/` | The single scope-binding path and the two pool types |
| `internal/posture/` | The isolation posture read from the catalog: the deploy's post-condition, the start check and readiness |
| `internal/organization/` | Organization registry |
| `internal/tenant/` | Tenant lifecycle and security version |
| `internal/workspace/` | Workspace lifecycle |
| `internal/membership/` | Membership, validity, administrative roles |
| `internal/invitation/` | Invitation intent and onboarding correlation |
| `internal/context/` | Operating-context eligibility and the authoritative fresh check |
| `internal/offboarding/` | Obligation tracking and retirement |
| `internal/projection/` | Snapshots, consumer registry, reconciliation |
| `tools/grantcheck/` | Derives which role runs which statement and has PostgreSQL judge the grants; `make grantcheck` |
| `docs/designs/` | Technical Design Documents |

Outbox, dispatcher, event envelope, deduplication, idempotency, problem details, and
telemetry are imported from `foundation-platform` rather than reimplemented here.

## Designs

| TDD | Subject | Status |
| :-- | :-- | :-- |
| `TDD-organization-control-001` | Tenant isolation and Row-Level Security | approved |
| `TDD-organization-control-002` | Membership authority, revocation, and projection publication | approved |

## The revocation contract

Acknowledgement of a revocation means the change is **durable and queued**, never that
it is enforced. Every response carries the accepted timestamp so enforcement delay is
measurable from a recorded origin.

Enforcement is the sum of a propagation term this service starts and a token-lifetime
term the token profile owns. No single owner states the interval; the operational
dashboard presents the sum.

## On the development server

`deploy/dev` runs this service on the shared development server, the way every service there is
deployed (STD-GLB-009 §Development Server Deployment). Its README is the procedure, and its
§Wiring to other services connects this service to the Identity Control API.

## Building the database

Four sources build one database, and each is owned by whoever owns the SQL:

| Source | Owner | What it applies |
| :-- | :-- | :-- |
| `internal/controldb/roles.sql` | this repository | The three cluster roles |
| `schema.hcl` via Atlas | this repository | The eight owned schemas and their tables |
| `foundation-platform/migrations/platform` | the shared module | The `platform` schema |
| `internal/controldb/rls.sql`, `grants.sql` | this repository | Policies, then privileges |

```powershell
$env:ORGANIZATION_MIGRATION_DATABASE_URL = 'postgres://…/organization_control_dev?sslmode=disable'
$env:DATABASE_URL   = $env:ORGANIZATION_MIGRATION_DATABASE_URL
$env:ATLAS_DEV_URL  = 'postgres://…/org_atlas_dev?sslmode=disable'

go run ./cmd/organization-migrate -stage=pre    # cluster roles
atlas migrate apply --env local                 # the owned schemas and their tables
go run ./cmd/organization-migrate -stage=post   # platform schema, RLS, privileges, then two checks
```

The post stage ends with two checks and fails a deploy if either does:

- the isolation posture (`posture.AssertIsolation`, below);
- that no unresolved authority-bearing dead letter lacks every way to close it
  (`controldb.UnclosableDeadLetters`, TDD-005 §The superseded case).

**`roles.sql` creates the three group roles and no login role.** `organization_migrator`,
`organization_rt`, and `organization_provider_rt` are all `NOLOGIN`: they carry privileges and
nobody authenticates as them. The login roles that inherit them are a deployment concern, because
their passwords are, so the pipeline creates them and this repository does not. Locally and in CI
that means:

```sql
CREATE ROLE organization_app LOGIN PASSWORD '…';
CREATE ROLE organization_provider_app LOGIN PASSWORD '…';
GRANT organization_rt          TO organization_app;
GRANT organization_provider_rt TO organization_provider_app;
```

Without them the integration suites skip when `TEST_DATABASE_URL` is unset and fail to authenticate
when it is set — see `.github/workflows/ci.yml` for the exact block CI runs.

**Maintenance is a fourth stage, run on a schedule rather than at deploy.** Run it daily as the
same migration role:

```powershell
go run ./cmd/organization-migrate -stage=maintenance
```

It does six things, each in its own transaction and each timed from the database clock:

- creates the outbox partitions for today and the next seven days;
- drops published partitions older than 30 days;
- removes the payload of dead letters resolved more than 90 days ago, keeping the incident and
  its resolution;
- deletes delivery receipts older than 90 days that no closure cites, and only while no incident
  is open;
- purges Membership batch previews that expired more than a day ago and never executed, with their
  items (`-batch-preview-retention`; TDD-organization-control-002 §Membership Batches). The two
  tables are under `FORCE ROW LEVEL SECURITY`, so the migration role reaches only expired previews,
  through the purge policies `rls.sql` gives it;
- counts unresolved dead letters older than 24 hours.

It exits 3 when that count is not zero, after the rest of its work is done, so the scheduler
raises an alert. An unresolved incident is never disposed. Each boundary has a flag. The defaults
are foundation-platform TDD-001's values.

Until this stage existed, no deployable called any of foundation-platform's retention helpers.
Every outbox row landed in `platform.outbox_default` and stayed there, and resolved payloads and
receipts were kept forever.

**The order differs from identity-control's, and the reason is Atlas rather than preference.**

Atlas refuses to apply against a database it considers unclean, and in database scope any
pre-existing schema counts — including `platform`, and including one named in `exclude`, which
the clean check does not consult. Roles are cluster objects, so creating them leaves the
database clean; that is what makes this order possible at all.

Database scope is itself forced. identity-control bounds Atlas to one schema with a
`search_path` on both URLs, and this service declares eight: Atlas rejects a multi-schema HCL
source against a schema-scoped dev URL. `atlas.hcl` bounds the scope with `schemas` and
`exclude` instead, and `public` is declared and managed empty — without it, the first generated
plan ended in `DROP SCHEMA "public" CASCADE`.

## Running the service

Every value comes from the environment, per STD-GLB-009. Nothing is defaulted that would let a
misconfigured process start and fail later.

| Variable | Required | Purpose |
| :-- | :-- | :-- |
| `ORGANIZATION_TENANT_DATABASE_URL` | yes | Connects as `organization_app` → `organization_rt` |
| `ORGANIZATION_PROVIDER_DATABASE_URL` | yes | Connects as `organization_provider_app` → `organization_provider_rt` |
| `ORGANIZATION_TOKEN_ISSUER` | yes | Compared for exact equality |
| `ORGANIZATION_TOKEN_AUDIENCE` | yes | This resource's registered identifier, `organization-control-api`: a keyless resource registration, never a client that authenticates (STD-IAM-002 §3.1) |
| `ORGANIZATION_JWKS_URL` | yes | Key source. Never read from a token |
| `ORGANIZATION_CONSUMER_DATABASE_URL` | no | Connects as `organization_consumer_app` → `organization_consumer_rt`. Set, it enables consumer authority |
| `ORGANIZATION_DELIVERY_TARGETS` | no | `consumer=https://acceptance-url` pairs, comma-separated. Each gets a dispatcher in this process once it is a registered consumer (ADR-GLB-018 §5.4) |
| `ORGANIZATION_DISPATCH_DATABASE_URL` | with targets | Connects as `organization_dispatch_app` → `organization_dispatch_rt` |
| `ORGANIZATION_WORKLOAD_CLIENT_ID`, `ORGANIZATION_WORKLOAD_KEY_FILE`, `ORGANIZATION_WORKLOAD_TOKEN_URL` | with targets | This service's workload client, its RSA key (at least 3072 bits), and the kernel token endpoint. Deliveries carry its access token, never a shared secret |
| `ORGANIZATION_DELIVERY_TIMEOUT`, `ORGANIZATION_DELIVERY_RETRY_INTERVAL` | no | `5s` and `1m`: one publication's bound, and how long a target waits before checking its registration again |
| `ORGANIZATION_LISTEN_ADDRESS` | no | Defaults to `:8080` |
| `ORGANIZATION_TOKEN_MAX_SKEW` | no | 30s; capped at 60s by STD-IAM-002 §3.5 |
| `ORGANIZATION_TOKEN_TYPE` | no | `report` (default) accepts a token whose header `typ` is not `at+jwt` and logs it with its `azp`; `enforce` refuses it with 401 (STD-IAM-002 §3.5 step 5, RFC 9068 §4). The dev issuer types its tokens `at+jwt` |
| `ORGANIZATION_PROVISIONING_TIMEOUT` | no | 30m. Age at which a provisioning request becomes `unresolved` |
| `ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL` | no | 15m. Cadence for the unresolved sweep |
| `ORGANIZATION_TENANT_NAME_MAX` | no | 120. Tenant display-name bound |

### Who a caller is

A caller is read from the token's standard claims and this service's own records, never from a role
in the token (ADR-ORG-001 §5.11, TDD-organization-control-001 §Caller Authority). Every token needs a
`principal_id` and a `subject_type`; the `principal_id` is the actor every event and evidence row
names, and `sub` is not read.

| Caller | Token | Record read for each request |
| :-- | :-- | :-- |
| Tenant administrator | `subject_type` `human`, `tenant_id`, `acr` `aal2` or higher, `auth_time` | in that Tenant: an active Membership, the Tenant active, and a tenant administration grant (ADR-ORG-003) |
| Provider | `subject_type` `human`, `acr`, `auth_time`, no `tenant_id` | a provider grant for the `principal_id` |
| Projection consumer | `subject_type` `workload`, `workload_owner`, no `tenant_id` | an active consumer registered with the `principal_id` |

A revoked grant, a retired consumer, a revoked Membership or a suspended Tenant stops at the next
request, not when the token expires. A record read that fails answers 503 and admits nobody.

**A Tenant's administrators** are granted by a provider, with `X-Administrative-Reason`:

```text
GET   /v1/tenants/{tenant_id}/administrators
POST  /v1/tenants/{tenant_id}/administrators                  {"principal_id": "..."}
POST  /v1/tenants/{tenant_id}/administrators/{grant_id}/revoke
```

A grant gives a Principal with no Membership in the Tenant a Tenant-wide one, in the same
transaction. A revocation leaves the Membership in place.

**The first provider grant** is made once, by a command on the deployable:

```text
ORGANIZATION_PROVIDER_DATABASE_URL=… organization-control bootstrap-provider \
    -principal-id <the first Principal's principal_id> -operator "<your name>" -reason "<why>"
```

It refuses when any grant exists, records the operator and the reason in a row no runtime role can
change, and creates no identity: the Principal is the one the Identity Control API's ceremony minted.
Rerun for the same Principal, it reports the grant and writes nothing.

**Every later grant, and every revocation, goes through the API**, as a provider, with
`X-Administrative-Reason`:

```text
GET   /v1/provider-grants                       every grant, active and revoked
POST  /v1/provider-grants                       {"principal_id": "..."}
POST  /v1/provider-grants/{grant_id}/revoke
```

A revoked grant confers nothing from the next request on, and stays listed with who revoked it and
why. The last active grant cannot be revoked: grant another provider first.

**A consumer registers with its workload `principal_id`** (`"principal_id"` in
`POST /v1/projections/consumers`). It stays that consumer's for good: re-registering under another is
refused.

**Four settings are gone, and startup refuses them:** `ORGANIZATION_TENANT_CLAIM`,
`ORGANIZATION_PROVIDER_ROLE`, `ORGANIZATION_CONSUMER_ROLE` and `ORGANIZATION_CONSUMER_CLAIM`. Unset
them. A database holding consumers registered before `principal_id` existed refuses the migration
that adds it; on a development database, clear `projection.consumer` and register again.

**A reconcile interval longer than the provisioning timeout is refused at startup.** A sweep slower
than the timeout leaves a request sitting `requested` well past the age at which its outcome is meant
to be declared unknown, which turns "ambiguous after thirty minutes" into a statement about nothing.
It is the misconfiguration that produces no error anywhere else.

**The two DSNs must differ, and startup refuses them if they are identical.** They are two
credentials for two PostgreSQL login roles with different policies, and the whole isolation posture
rests on ordinary tenant traffic being unable to authenticate as the cross-Tenant role. One DSN
reused for both would compile, pass every test that does not inspect `current_user`, and silently run
the estate's tenant traffic under the role that can read every Tenant.

### Lists, and the version a Membership transition names

What an administrator lists (TDD-organization-control-002 1.9.0, -003 1.6.0, -004 1.6.0):

```text
Tenant administrator, the Tenant from the token:
GET   /v1/workspaces       ?status=active|archived|retired
GET   /v1/memberships      ?status=active|suspended|revoked&workspace_id=<uuid>&principal_id=<uuid>
GET   /v1/memberships/{membership_id}
GET   /v1/invitations      ?state=pending|identity_verified|accepted|expired|revoked

Provider, with X-Administrative-Reason:
GET   /v1/organizations    ?status=active|suspended|retired&classification=provider|customer|partner|publisher
GET   /v1/tenants          ?status=<a Tenant state>&organization_id=<uuid>
```

Every list also takes `after=<the last id of the previous page>` and `limit` (50 when absent, 1 to
100, anything else `400`) and answers `{"<items>": [...], "next": "<id>" | null}`, in creation order
(STD-GLB-001 1.3.0 §Pagination). An unknown filter value is `400`.

A Membership transition names the version it was shown, and a revocation says why:

```text
POST  /v1/memberships/{membership_id}/suspend   {"expected_version": 1}
POST  /v1/memberships/{membership_id}/restore   {"expected_version": 2}
POST  /v1/memberships/{membership_id}/revoke    {"expected_version": 3}   X-Administrative-Reason required
```

A stale version answers `409` and changes nothing. The actor, the correlation and the reason are kept
on the transition's `membership.membership_event` row.

What a provider reads about an offboarding (TDD-organization-control-003 1.7.0, -004 1.7.0):

```text
GET   /v1/offboardings                       ?stage=freeze|obligations|release|retired&tenant_id=<uuid>
GET   /v1/offboardings/{offboarding_id}      the stage-entry instants, the deprovisioning, active_memberships
GET   /v1/offboardings/{offboarding_id}/obligations   {"outstanding": [...], "obligations": [...]}
GET   /v1/tenants/{tenant_id}                adds offboarding_id and active_memberships
```

A Tenant administrator acts on Memberships in bulk, and reads whether a transition is enforced
(TDD-organization-control-002 1.10.0, ADR-ORG-004). An execution whose request ended (a crash, or
the request timeout) is left `executing`; the next execute resumes it once its heartbeat is 30 seconds
old, applying only the items with no outcome, and the same `Idempotency-Key` may be the one that
resumes it (1.13.0):

```text
POST  /v1/membership-batches                 {"action":"revoke","membership_ids":[...]}   preview, 1 to 500; 501 or more is 413
POST  /v1/membership-batches/{batch_id}/execute   {"fail_on_errors": n}   Idempotency-Key; resumes an execution whose request ended
GET   /v1/membership-batches/{batch_id}
GET   /v1/memberships/{membership_id}/enforcement   accepted | propagating | enforced | over_budget
```

What a provider reads about projection health and provisioning (TDD-organization-control-002 1.11.0,
-003 1.8.0):

```text
GET   /v1/projections/consumers              ?state=active|retired   each item adds state, retired_at, stale
GET   /v1/projections/consumers/{id}         with the list, last_reconciled_at, _mark, _findings and reconciliation_age_seconds
GET   /v1/tenants/{tenant_id}                adds provisioning: the latest request, unresolved included
```

A person lists where they may work, and a provider cancels an offboarding begun by mistake
(TDD-organization-control-002 1.12.0, -004 1.8.0, ADR-ORG-005, ADR-ORG-006):

```text
GET   /v1/principals/{principal_id}/contexts      your own, with any human token; anyone's, as a provider with a reason
POST  /v1/offboardings/{offboarding_id}/cancel    {"expected_version": n}   in freeze or obligations only
```

### Locally: `.env` and the Makefile

Nothing above needs to be typed. `.env.example` carries a working local set; the `Makefile` loads
`.env` and exports it to the child process.

```text
make env      copy .env.example to .env, once, without overwriting an existing one
make issuer   terminal 1: the dev token issuer on 127.0.0.1:8098
make run      terminal 2: the service on 127.0.0.1:8099
make token    save a provider token to .token
make api P=/v1/tenants/<id>
make api M=POST P=/v1/organizations B=body.json KEY=org-1
make gates    everything CI runs: fmt vet build arch tidy test
```

The same commands work from `cmd.exe`, PowerShell, and a POSIX shell, which is the reason this
exists. The environment used to live here as a PowerShell block; cmder is `cmd.exe`, where
`$env:NAME = 'value'` is not an assignment, and the line fails with *"The filename, directory name, or
volume label syntax is incorrect"* — a message about paths for what is a shell mismatch, so the
reader goes looking at the DSN.

**`.env` is loaded by `make`, never by the binary.** The service still reads only the process
environment, so a deployment is configured exactly as the table above describes and no code path
looks for a file. `.env` is gitignored; `.env.example` is committed.

**`-include`, not `include`.** `fmt`, `vet`, `build`, `arch`, and `test-unit` must work in a fresh
clone with no `.env`, and in CI, where the environment comes from the workflow.

The optional variables are listed in `.env.example`, commented out, so the knobs are discoverable
without being set.

### Three muxes, not one with an exemption list

| Mux | Authentication | Scope resolution | Holds |
| :-- | :-- | :-- | :-- |
| `Probes` | none, ever | none | `GET /healthz`, `GET /readyz` |
| `Anonymous` | none | none | `POST /v1/invitations/lookup` |
| `API` | required | required | everything else |

An exemption list is edited by whoever adds a route, and the failure mode of forgetting is an
unauthenticated mutation. Here a route is unauthenticated only if its author writes it into
`Anonymous`. identity-control learned the other half of this in an outage: one mux meant the
authentication middleware also wrapped `/readyz`, every probe answered 401, and no replica entered
service.

The anonymous route reads nothing. SAD-004 §5.5 requires an invitation lookup to answer identically
for an absent, expired, revoked, accepted, and valid token, and the only construction where that
holds for the status code, the body, *and* the response time is one that looks nothing up.

### Against the kernel

The dev issuer drives this service alone. Delivering provider grants to the Identity Control API,
and serving its workload on the consumer routes, needs kernel tokens on both sides:
identity-control's `docs/run.md` §8 wires the two, in an order where each step's token still works.

### Driving the service by hand

Every authenticated route needs a token from the issuer named in `ORGANIZATION_JWKS_URL`, so without
one the only things reachable by hand are the two probes and the anonymous invitation lookup.
`cmd/organization-devissuer` closes that: it publishes a key set and mints tokens the real verifier
accepts, so the service under test runs completely unmodified — same binary, same chain, same
verifier.

```text
make issuer                    # terminal 1, leave it open
make run                       # terminal 2
make token                     # terminal 3: saves a provider token to .token
make api P=/v1/tenants/<id>
make api M=POST P=/v1/organizations B=body.json KEY=org-1
make token ROLE=tenant         # a Tenant-scoped token, refused 403 on a provider route
make token ROLE=stranger       # a person holding no provider grant, refused 403
```

A provider token names the dev provider, whose grant `scripts/ci-fixture.sql` seeds. On a database
without the fixture, run `organization-control bootstrap-provider` for a Principal and ask the issuer
for `role=provider&principal_id=<it>`.

`make api` sends `X-Administrative-Reason` because every provider-scoped call writes a row to
`audit.privileged_access` and the service answers 400 rather than recording an unexplained one.
`KEY=<value>` adds the `Idempotency-Key` every command requires: a value of its own per command, and
the same value to repeat one. The body is a **file**, not an argument: quoting JSON on
a `cmd` line means escaping every double quote, and one missed backslash produces a 400 that looks
like the service rejecting a valid request.

The issuer binds its port before printing anything. A second copy fails on the bind, and printing the
instructions first meant a wall of text followed by an error, which reads as the service refusing
something rather than as *this is already running*.

**The build tag is the safety property.** `//go:build devissuer` keeps it out of `go build ./...`,
out of CI, and out of any image. It signs tokens for whoever asks, which is what makes it useful and
why it must never run anywhere real — so it is absent from the standard build rather than disabled by
a flag somebody could leave on.

**Two things it discovered, both of which apply to the real realm.**

`verify` permits exactly one algorithm, **PS256**, and verifies with `SignPSS` at
`PSSSaltLengthEqualsHash`. An RS256 token is well formed, correctly claimed, and refused.

`verify` rejects any RSA modulus below **3072 bits**, and it does so while parsing the key set: a
2048-bit key is discarded, the set then carries no usable key, and every verification fails as
`kid unknown and the key set could not be reloaded`. That message is about key distribution and the
cause is key size. **A Keycloak realm signing with 2048-bit keys will have every token refused by
this service, and the refusal will not say why** — worth settling before the Keycloak
proof-of-concept rather than during it.

Both are now asserted by `internal/httpapi/token_endtoend_test.go`, which covers the one
authentication path the rest of the package does not: key material fetched from a JWKS endpoint over
HTTP. `authentication_test.go` signs real tokens against a real verifier but supplies the key with
`verify.StaticKeys`, so `verify.NewJWKS` and the document it parses were never exercised here.

### Tenant intake and provisioning correlation

A Tenant is created in `requested` and reaches `active` only through the provisioning system. Six
routes cover the path:

| Route | Driven by | Effect |
| :-- | :-- | :-- |
| `POST /v1/tenants` | operator | Tenant in `requested`, a provisioning request, and `tenant.lifecycle.requested` — one transaction |
| `POST /v1/tenants/{id}/provisioning` | operator | `requested → provisioning`, or a retry from `failed` with a new request row |
| `POST /v1/provisioning/realized` | provisioning system | Marks the request `realized`. Does **not** activate |
| `POST /v1/provisioning/failed` | provisioning system | Marks the request `failed` and moves the Tenant to `failed` |
| `POST /v1/provisioning/sweep-unresolved` | scheduler | Ages unanswered requests to `unresolved`. Never retries |
| `POST /v1/tenants/{id}/activate` | operator | `provisioning → active`, once realized and the sponsor is active |

Four properties are worth naming because each is a decision rather than a detail.

**The creation event is the desired-state publication.** TDD-organization-control-003 ends intake with
"publish desired state" and names exactly one event at creation, so they are the same event:
`tenant.lifecycle.requested` carries the isolation profile, the residency region, and the correlation
identifier the realized status comes back on. A separate internal channel would have given the estate
two records of one intention.

**A realized status does not activate.** Activation also checks the sponsoring Organization, which is
a decision about the customer relationship rather than about infrastructure — and the provisioning
system has no view of it. It stays a deliberate act.

**A timeout produces `unresolved`, never `failed`, and never a retry.** SAD-004 §7.5 requires an
ambiguous outcome to remain pending or failed and never to be inferred as success. The target may have
built the boundary or may not; treating that as a refusal and retrying is how a Tenant gets
provisioned twice. The sweep ages deprovisioning requests too, which is what finally makes
`internal/offboarding`'s ambiguity gate reachable — nothing produced the state before.

**The two callback routes are authenticated like everything else.** A route exempted so an external
system could reach it more easily would let anyone who learned a correlation identifier declare a
Tenant's boundary built, and activation reads exactly that statement before letting Memberships in.
The design's API list names none of these four provisioning routes; that is an omission rather than a
prohibition, since it mandates realized-status correlation, gives this service no inbound transport
but HTTP, and `POST /v1/offboardings/{id}/deprovisioning` already reports the other direction's
outcome the same way.

### `Idempotency-Key`

Supply the header on a mutation and a retry of the identical request returns the first response
instead of executing again. The response carries `Idempotent-Replay: true` so a client can tell which
happened.

**The claim is made inside the service's own transaction, not by the middleware.**
`idempotency.Claim` takes a `db.Tx` because the claim has to commit with the effect it guards, so
`internal/db` makes it inside the scoped transaction every service already opens — the middleware only
attaches it to the context. A middleware claiming in a transaction of its own would commit
separately, and a key held by a mutation that then rolled back refuses every retry of a request that
never happened, reported as "already in progress". That sends whoever is debugging to look for a
concurrent request that does not exist. `TestAFailedMutationReleasesItsKey` fails if the claim is
moved out of that transaction — it was written by moving it out and watching it fail.

Threading it through the scope binding rather than through the services was the alternative to a
`Within` variant on some thirty service methods across eight packages. The services never see a
claim, which is what stops one of them being written without honouring it.

**The response is recorded with the effect.** A command handler supplies a renderer
(`httpapi.answer`), and the service hands the value it returns to `db.Respond` as the last step of its
transaction. `db.Respond` completes the claim in the transaction that made it, so the claim, the effect
and the response commit together, and the handler writes the bytes it recorded. A process dying after
the commit and before the reply leaves a key that a retry replays (TDD-organization-control-003 §The
Response Is Recorded with the Effect).

| Situation | Answer |
| :-- | :-- |
| No header, or a blank one, on a command | 400 `validation-failed`, naming the header, before anything is read |
| No header on a route that does not require one | Passes through untouched |
| Identical retry of a completed request | The stored response, `Idempotent-Replay: true` |
| Same key, different request | 409 `idempotency-key-conflict` |
| Same key, first use not yet completed | 409 `request-in-progress` |
| Header on a `GET` or `HEAD` | 400 — a key spent on a read would answer the caller's later mutation |
| Body over 1 MiB | 400 — refused rather than silently unclaimed |

**Which routes require it.** Every command does: a `POST` a person or an operator sends to change
authoritative state. The routes that honour a key without requiring one are reads carried in a body,
machine reports that carry their own identity, sweeps, and the dead-letter acts, each with its reason
in `internal/httpapi/commands.go` and TDD-organization-control-003 §The `Idempotency-Key` Is Required
on Commands. The key is checked after the caller's authority, so a caller the route does not admit is
answered 403.

A window remains for the few commands whose effects commit in more than one transaction: a batch
execution, the offboarding freeze batch and the offboarding cancellation. Their response is recorded
after the handler writes, as every response was before. A process dying in between leaves a key
claimed and uncompleted, and later retries are refused rather than replayed; the mutation still
happened exactly once. A batch execution answers it itself: a retry with its key adopts the
uncompleted claim and is answered from the batch (TDD-organization-control-002 §Resuming an
execution). `TestEveryCommandRecordsItsResponseWithItsEffect` keeps a new command from joining that
list without a reason.

**A replay returns the same response, not the same bytes.**
`platform.idempotency_key.response_body` is `jsonb`, so PostgreSQL sorts object keys and drops
insignificant whitespace. Anything hashing or signing a response body has to canonicalise first.

## Metrics and alerts

With `OTEL_EXPORTER_OTLP_ENDPOINT` set, the service exports over OTLP/HTTP to the OpenTelemetry
Collector, as STD-GLB-003 requires. Unset, it exports nothing and says so at startup.

The enforcement gauges (`internal/telemetry`) are read from the database on each collection, on
the provider connections, by `projection.SignalsReader`:

| Series (Prometheus name) | What it is |
| :-- | :-- |
| `organization_outbox_unpublished{consumer,lane}` | deliveries waiting to be published to each active consumer, per lane (`priority`, `standard`) |
| `organization_outbox_oldest_unpublished_age_seconds{consumer,lane}` | that consumer's dispatcher lag |
| `organization_security_debt_dead_letters{consumer}` | unresolved authority-bearing dead letters the consumer's frontier counts: its own and any naming no consumer; above zero, its projection-backed checks refuse |
| `organization_security_debt_oldest_age_seconds{consumer}` | how long the oldest has been open |
| `organization_dead_letters_unresolved_unwaived` | unresolved dead letters with no live waiver |
| `organization_dead_letters_oldest_unresolved_unwaived_age_seconds` | the oldest of them |
| `organization_projection_consumer_report_age_seconds{consumer}` | seconds since the consumer last reported progress, or since it registered |
| `organization_projection_consumer_max_accepted_age_seconds{consumer}` | its declared budget |
| `organization_projection_consumer_verify_ratio{consumer}` | its last measured fresh-check ratio |

`observability/alerts/organization-control.rules.yml` holds the alert rules, with each threshold's source
noted beside it (SAD-004 §9.3.2 and the TDDs). CI checks the rules and runs their unit tests with a
pinned `promtool`, and a mutation that moves one threshold must fail those tests.
`internal/telemetry`'s test fails if a rule reads a series no instrument exports. Locally:

```text
promtool check rules observability/alerts/organization-control.rules.yml
promtool test rules observability/alerts/organization-control.test.yml
```

## Runbooks

`docs/runbooks/` answers the production gate's five procedures and the alerts in
`observability/alerts`: revocation not enforced within budget, projection drift repair,
provider-access review, stuck offboarding, and dead-letter resolution. Each lists the gaps it found.
A change to a route, metric or alert updates the runbook that names it in the same change.

## Row-Level Security is not in `schema.hcl`, and that is a vendor limitation rather than a design choice

Atlas OSS models neither `ENABLE`/`FORCE ROW LEVEL SECURITY` nor `CREATE POLICY`. Verified
against v1.3.2: `schema inspect` emits no trace of either and prints *"Skipping … advanced
objects. Upgrade to Pro."* So the policies live in `internal/controldb/rls.sql` and are applied
after Atlas.

**This is a workaround. Three consequences follow, and two of them are closed.**

*A runtime role cannot remove a policy.* Neither holds any DDL privilege and neither owns a
table, so the application cannot weaken its own isolation.

*Drift heals on deploy.* `rls.sql` recreates every policy on every run and discovers its table
set from the catalog, so a dropped policy returns and a newly added table is protected without
anyone remembering to extend a list. That is better than a declarative list, which someone has
to maintain.

*Nothing reconciles a policy the way Atlas reconciles a column.* This one stays open, and it is
narrower than it first looks: reconciliation happens at deploy time, and `rls.sql` already
converges at deploy time. What a declarative tool would add is a **diff** — the ability to review
a policy change as a schema change and to detect drift without applying anything.

Closing that half requires Atlas Pro, which also supplies `atlas migrate lint` — the destructive
gate `ADR-GLB-004` mandates and that CI currently stands in for with a text-level grep. One
purchase clears both, and both are recorded as debt in [ROADMAP.md](ROADMAP.md) rather than
presented as equivalent to the mandated mechanism.

### The safety gap is closed without any vendor

The dangerous half was never reconciliation. It was that **between deploys, in production,
nothing checked** — CI asserts the posture of a throwaway database, which says nothing about the
one serving traffic.

`posture.AssertIsolation` (`internal/posture`) reads `pg_class`, `pg_policy`, and `pg_roles` and
reports every way the posture is not intact. `-stage=post` calls it as a post-condition, so a deploy
that applied SQL without achieving the posture fails instead of reporting green. The service calls it
at startup, as the tenant login role, and refuses to start on a problem; `GET /readyz` calls it on
every probe, so a replica whose database lost a policy leaves the load balancer. Serving
tenant-scoped traffic with isolation disabled is worse than not serving, and `EAD-006 §8` requires a
security-control failure to fail closed (TDD-organization-control-001 §Verifying the Posture at
Runtime).

Six weakenings are tested and each is detected: `FORCE` removed, RLS disabled, a policy dropped,
a table added to a tenant-scoped schema without `tenant_id`, and a runtime role granted
`BYPASSRLS` or `LOGIN`. Every one of them leaves the schema matching its declared state, which is
exactly why a schema tool would not have caught them either.

## Proving isolation

`TDD-organization-control-001` is explicit about what counts:

> Isolation tests MUST prove cross-tenant denial using the actual application runtime role; a
> test executed on an administrative or owning connection is not isolation evidence.

Every isolation assertion connects as a login role that inherits `organization_rt` or
`organization_provider_rt`. On an owning connection they would all pass for the wrong reason,
because PostgreSQL exempts an owner from its own table's policies unless `FORCE` is set.

```powershell
$env:TEST_DATABASE_URL      = 'postgres://postgres:…@127.0.0.1:5432/organization_control_dev?sslmode=disable'
$env:TEST_RUNTIME_PASSWORD  = '…'   # organization_app, inherits organization_rt
$env:TEST_PROVIDER_PASSWORD = '…'   # organization_provider_app, inherits organization_provider_rt

go test ./... -count=1 -p 1
```

**`-p 1` is required, not preferred.** Every integration suite runs against one database, and
`internal/controldb` deliberately weakens the isolation posture to prove its assertions catch
weakening: it and `internal/posture` drop `membership_tenant_scope`, remove `FORCE`, and grant
`organization_rt` `BYPASSRLS`, restoring each afterwards. Those changes are table-wide and cluster-wide, so any
other package's RLS assertion that runs in that window observes a weakened database and fails
for a reason unrelated to the code it tests. Without the flag `go test ./...` passes roughly
three runs in four, which is the worst possible failure rate — often enough to be dismissed as
flakiness and rare enough to survive review. An advisory lock does not close it, because
`ALTER ROLE` races with every role-dependent assertion in the estate regardless of what locks
the two suites hold.

CI creates both login roles, seeds two Tenants, and sets `REQUIRE_INTEGRATION=1` so a service
container that never came up fails the build rather than leaving every cross-tenant assertion
unrun and the build green.

### Coverage floor

The floor is 65%, measured over shipped packages (`internal/`), with the integration suites
running as the real roles. CI fails below it and keeps `coverage.out` for a week.

It exists to put a number on what a green integration run covers, not to prove anything is
complete. The suite refutes a grant, a policy or a rule only on the paths it executes, so the
share of statements it executes is the bound on that claim. `tools/grantcheck` covers the
grant half on every derived path regardless of coverage, which is why the floor is not the
only thing standing between a missing grant and production.

The mechanism mirrors foundation-platform's: atomic mode and no `-coverpkg`, so a statement
counts only when its own package's tests run it. `tools/` is verified by its own tests, and
`cmd/` is the composition root, exercised by the deploy e2e and the system proof.

The number is set below the measured 68.3% at introduction, not at a round target. It is a
floor, not a target: coverage raised by testing getters buys nothing. The lowest package is
`internal/httpapi` at 40%. Its handlers are thin over services tested directly, and its
refusals before the database are tested at the transport boundary. Raise the floor when a
change raises the figure for a reason worth keeping.