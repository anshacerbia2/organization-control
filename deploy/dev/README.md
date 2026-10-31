# organization-control on the development server

Organization Control beside identity-kernel's and identity-control's stacks, deployed the way every
service on the server is (STD-GLB-009 §Development Server Deployment). The sections follow that
standard's skeleton, in its order.

## What runs

The Compose project is `scnehaux-organization-control-dev`.

| Service | What it is |
| :-- | :-- |
| `postgres` | This service's own Control Database, in the named volume `postgres` |
| `migrate` | One-shot: the cluster roles, the owned schemas, the platform schema, RLS, privileges, the five runtime login roles, and a check of the privilege shape |
| `organization-control` | The API, with its delivery dispatcher (ADR-GLB-018 §5.4) and the two scheduled sweeps (TDD-003 §Scheduled Sweeps) in process, on `127.0.0.1:8083` (`ORGANIZATION_CONTROL_PORT`) |
| `bootstrap-provider` | One-off task: the first provider grant |
| `maintenance` | One-off task: the daily maintenance stage |
| `restore-bump` | One-off task: the restore bump, run by `restore.sh` after a restore and before the service serves (ADR-ORG-003 §5.5) |

| Network | What it is |
| :-- | :-- |
| `internal` | This stack's own, between the database, the job and the service. `compose.override.example.yaml` pins it to the subnet reserved for it |
| `scnehaux-identity-api` | identity-kernel's, joined for Keycloak: the key set and workload tokens |
| `scnehaux-identity-control-api` | identity-control's, joined both ways: this service delivers to identity-control on it, and identity-control reads this service's snapshot and frontier on it |

Two departures from the skeleton, kept on purpose:

- **No `scnehaux-organization-control-api` network.** No other stack joins this one: identity-control
  reaches it over identity-control's own network, which this stack joins, and Organization Experience
  runs on a laptop and calls the published port. A network nobody joins would be one more name to
  keep. If a stack ever needs to join this one, it is created under that name then.
- **The internal network keeps Compose's default name**, `scnehaux-organization-control-dev_internal`,
  and the volume `scnehaux-organization-control-dev_postgres`. Renaming the project, a network or the
  volume would leave the server's existing database volume orphaned beside a new, empty one.

## Before you start

- identity-kernel's stack is running. Its `scnehaux-identity-api` network carries Keycloak.
- identity-control's stack is running, on a version that creates the `scnehaux-identity-control-api`
  network, and has moved to `identity-control-api` (its README §Moving a server). This stack joins
  both networks, so it starts after them.
- You can get a provider token from identity-control (its README §Calling the API): the ceremony's
  Principal, `bootstrap-operator`. identity-control's ceremony takes two named people, an operator
  and a witness, who are never the same (`STD-IAM-001 §3.10`): `bootstrap.sh <operator> <witness>
  <reason>`. `deploy-dev` passes `ci@scnehaux.local` and `ci-witness@scnehaux.local`.
- Docker with Compose v2, and your user in the `docker` group.
- PowerShell 7 (`pwsh`) for the wiring scripts in §Wiring to other services.
- On a network that SSL-inspects `proxy.golang.org`, as the development server's does, set
  `GOPROXY=direct` in `.env` before the first build. Every Go build in `compose.yaml` takes it as a
  build argument.

## First start

```sh
cd organization-control/deploy/dev
cp .env.example .env
# fill in KEYCLOAK_ISSUER (the value identity-control's .env holds) and the six passwords:
#   openssl rand -hex 32
# and GOPROXY=direct where the default proxy is intercepted
cp compose.override.example.yaml compose.override.yaml   # on the development server: the subnet pin
docker compose up -d --build
curl -fsS http://127.0.0.1:8083/readyz       # ready once the migration job has succeeded
```

Set `BUILD_REVISION=$(git rev-parse HEAD)` for the build to record the commit as the image's `org.opencontainers.image.revision` label (`unknown` without it); `deploy-dev` sets it and checks it.

The service starts with no provider and no dispatcher. §One-off tasks and §Wiring to other services
make it useful.

`compose.override.yaml` is read by Compose automatically and is git-ignored. A network's subnet cannot
change in place: on a stack started before the pin, `docker compose down` (never `-v`) and then
`docker compose up -d` recreates the network and keeps the database.

## Updating

```sh
cd organization-control/deploy/dev
git pull
docker compose up -d --build
```

The migrate job applies whatever is new before the service restarts. The one-off tasks run the migrate
image, so this rebuilds them too.

## One-off tasks

| Task | Run | When |
| :-- | :-- | :-- |
| `bootstrap-provider` | `docker compose run --rm bootstrap-provider -principal-id <id> -operator "<you>" -reason "<why>"` | once per Control Database: it refuses while any grant exists |
| `maintenance` | `docker compose run --rm maintenance` | daily, from cron; it exits 3 on a security incident open past 24 hours |
| `restore-bump` | run by `restore.sh` (§Backups); never by hand on a database that was not just restored | once per restore, before the service serves |
| `scripts/dev-provisioning.ps1` | `pwsh ./scripts/dev-provisioning.ps1 -IdentityRepo <identity-control checkout> -State <wiring state file> -TenantId <id> -Realized` (or `-Failed -Reason "<why>"`) | for each Tenant waiting on provisioning: it stands in for the provisioning system (§Provisioning a Tenant on the development server) |
| `scripts/dev-obligation.ps1` | `pwsh ./scripts/dev-obligation.ps1 -IdentityRepo <identity-control checkout> -State <wiring state file> -OffboardingId <id> -ObligationId <id> -Domain <its domain> -Completed` (or `-Waived -Reason "<why>"`, `-Failed -Reason "<why>"`) | for each obligation an offboarding raised: it stands in for the domain that owns it (§Offboarding a Tenant on the development server) |
| `scripts/dev-deprovisioning.ps1` | `pwsh ./scripts/dev-deprovisioning.ps1 -IdentityRepo <identity-control checkout> -State <wiring state file> -OffboardingId <id> -Realized` (or `-Failed -Reason "<why>"`) | for each released offboarding: it stands in for the provisioning system's deprovisioning report (§Offboarding a Tenant on the development server) |

`maintenance` creates the outbox partitions ahead, applies the retention, purges expired Membership
batch previews, purges idempotency keys completed more than 7 days ago, purges audit records older
than 7 years (the privileged-access record, its reviews and emergency grant uses), and counts stale
incidents (the repository README §Building the database). On the
development server, in the crontab of the user that runs the stacks (`crontab -e`), with the checkout
where that server keeps it:

```text
15 3 * * * cd /home/development/apps/organization-control/deploy/dev && docker compose run --rm maintenance >> "$HOME/organization-control-maintenance.log" 2>&1
```

Plain `docker compose` from `deploy/dev`, so it reads this stack's `.env` and override like every
other command here. A run that exits 3 is not a failure of the run: its work is done, and the log
count says how many incidents to resolve (TDD-organization-control-005; `POST /v1/dead-letters/.../resolve`).

**Check that it is installed and runs.** After adding the line, and whenever the server is rebuilt, as
the user that runs the stacks:

1. The line is in your crontab:

   ```sh
   crontab -l | grep -F 'docker compose run --rm maintenance'
   ```

   No output means it is not installed: add it with `crontab -e`. `crontab -l` prints the crontab cron
   reads; cron "searches its spool area (/var/spool/cron/crontabs) for crontab files" and picks up a
   change on its own (Debian, *cron(8)*), so nothing is restarted after an edit.
2. The cron daemon runs: `systemctl status cron` reads `active (running)`. On Debian and Ubuntu the
   `cron` package installs the unit `/lib/systemd/system/cron.service`.
3. It ran: after 03:15 the log has a new run.

   ```sh
   ls -l --time-style=+%F\ %T "$HOME/organization-control-maintenance.log"
   tail -n 20 "$HOME/organization-control-maintenance.log"
   ```

   The file's time is today's run. A missing file means the line never ran: check steps 1 and 2. cron
   "logs its action to the syslog facility 'cron'", so `journalctl -u cron --since today` shows whether
   it started the command, and the log file shows what the command printed.
4. Optional, to see one run without waiting: the line's command by hand, from `deploy/dev`,
   `docker compose run --rm maintenance`. Its exit code is 0, or 3 with incidents to resolve.

## Wiring to other services

This connects Organization Control and the Identity Control API through workload tokens from the
kernel, each with the other side's resource as its audience (TDD-identity-control-006,
STD-IAM-002 §3.1). It is the server form of identity-control's `docs/run.md` §8.

**The order matters.** Each step needs a token the step before still accepts. Step 4's grant retires
the ceremony's own grant in identity-control once it is projected, permanently. From then on the
ceremony's Principal is a provider through this database alone, which is why the volume is never
dropped (§Never do).

Use plain ASCII in every `X-Administrative-Reason`. Anything else is refused with `400`
(STD-GLB-001 §Request Header Values).

**Run it as a script.** `scripts/dev-wire.ps1` is steps 1 to 6c below, and `scripts/dev-wiring-proof.ps1`
is step 7. They are the files CI runs against the three stacks on every change and every night
(`.github/workflows/deploy-dev.yml`, STD-GLB-009 1.3.0), so the procedure on the server is the
procedure that was tested:

```sh
export IDENTITY_CALLER_PASSWORD=...            # the bootstrap operator's, from identity-control's .env
export IDENTITY_CALLER_KEY_FILE=...            # identity-control-caller.pem, from the same .env
export KEYCLOAK_ISSUER=...                     # the kernel's issuer, as in this stack's .env
pwsh ./scripts/dev-wire.ps1 -IdentityRepo /srv/identity-control -KernelDeployDir /srv/identity-kernel/deploy/dev \
    -Operator "<you>" -State ~/scnehaux-wiring/state.json
export KC_BOOTSTRAP_ADMIN_PASSWORD=...         # the kernel's console administrator, for step 7's relink
export KC_ADMIN_URL=...                        # where the kernel's /admin is reachable
pwsh ./scripts/dev-wiring-proof.ps1 -IdentityRepo /srv/identity-control -KernelDeployDir /srv/identity-kernel/deploy/dev \
    -State ~/scnehaux-wiring/state.json
```

Export those three alone. Sourcing identity-control's `.env` would also export its `POSTGRES_PASSWORD`,
which overrides this stack's when the script runs `docker compose` here. identity-control's
`dev-token.ps1` derives the rest (its `deploy/dev/README.md`, §Where the scripts call): the kernel's
origin from `KEYCLOAK_ISSUER`, identity-control's API from `IDENTITY_CONTROL_PORT` (8082 when unset),
and the operator's TOTP file from `deploy/dev/keys/operator-totp.json` in the identity-control
checkout once it exists. `KC_BASE_URL`, `IDENTITY_API_URL` and `IDENTITY_OPERATOR_TOTP_FILE` still
override them. The state file's directory
receives the operator caller's private key, so keep it outside every directory a container mounts.
The steps below say what each part does.

### 1. Three keys

With the kernel's key tool, on this server. Only the public JWKs leave it. The operator caller's key
belongs to you, because you sign with it, and stays beside the state file:

```sh
$KERNEL_DEPLOY_DIR/new-client-key.sh organization-control-workload "$PWD/keys" 65532:65532
$KERNEL_DEPLOY_DIR/new-client-key.sh identity-control-workload /srv/identity-control/deploy/dev/keys 65532:65532
$KERNEL_DEPLOY_DIR/new-client-key.sh dev-provider-caller ~/scnehaux-wiring "$(id -u):$(id -g)"
```

### 2. In identity-control: this API's resource, the two workloads, and the caller's audience

With a provider token, against `http://127.0.0.1:8082`. Each request carries `Authorization`,
`Content-Type: application/json` and an `X-Administrative-Reason`. Each creation also carries an
`Idempotency-Key`.

```text
POST /v1/registrations
{"client_key":"organization-control-api","profile":"resource","audience_class":"privileged",
 "application_ref":"organization-control","lifetime_class":"L0"}

POST /v1/workloads
{"display_name":"organization-control-workload","purpose":"Delivers provider grant events to the Identity Control API",
 "workload_type":"service","owner_principal_id":"<bootstrap-operator>","client_key":"organization-control-workload",
 "application_ref":"organization-control","audience":["identity-control-api"],"public_key":<organization-control-workload.jwk.json>}

POST /v1/workloads
{"display_name":"identity-control-workload","purpose":"Reads Organization Control's provider authority snapshot and frontier",
 "workload_type":"service","owner_principal_id":"<bootstrap-operator>","client_key":"identity-control-workload",
 "application_ref":"identity-control","audience":["organization-control-api"],"public_key":<identity-control-workload.jwk.json>}

POST /v1/registrations
{"client_key":"dev-provider-caller","profile":"confidential","audience_class":"privileged",
 "privileged_form":"provider-scope","application_ref":"development-operator",
 "audience":["identity-control-api","organization-control-api"],
 "redirect_uris":["http://127.0.0.1:8099/callback"],"public_key":<dev-provider-caller.jwk.json>}
```

Note each workload's `principal_id`. The last request registers the operator's caller of both APIs,
so the same provider can call both. `identity-control-caller` cannot be given the second audience: it
was made in the kernel by `create-kernel-clients.sh` before registration existed, so it has no
registration to change. Both resources are `L0`, so the lifespan stays 240 seconds.

### 3. The first provider here

```sh
docker compose run --rm bootstrap-provider -principal-id <bootstrap-operator> \
  -operator "<you>" -reason "the first provider of Organization Control on the development server"
```

This is an emergency `provider:organization-control` grant, as every bootstrap grant is
(ADR-ORG-002 §5.2).

### 4. Organization's grant and consumer for the Identity Control API

With a `dev-provider-caller` token, whose `aud` names `organization-control-api`, against
`http://127.0.0.1:8083`. Each request carries an `X-Administrative-Reason` and an `Idempotency-Key`,
which this service requires on both (a command without one is refused `400`), so a retry is safe:

```text
POST /v1/provider-grants
{"principal_id":"<bootstrap-operator>","scope":"provider:identity-control","kind":"emergency"}

POST /v1/projections/consumers
{"consumer_id":"identity-control","principal_id":"<identity-control-workload's principal_id>",
 "projection_version":"v1","max_accepted_age_seconds":60,"stale_behavior":"fail_closed",
 "event_types":["com.scnehaux.organization.provider.lifecycle.granted","com.scnehaux.organization.provider.lifecycle.activated",
                "com.scnehaux.organization.provider.security.ended","com.scnehaux.organization.provider.security.revoked",
                "com.scnehaux.organization.membership.lifecycle.granted","com.scnehaux.organization.membership.lifecycle.restored",
                "com.scnehaux.organization.membership.security.suspended","com.scnehaux.organization.membership.security.revoked",
                "com.scnehaux.organization.tenant.lifecycle.activated","com.scnehaux.organization.tenant.lifecycle.retired",
                "com.scnehaux.organization.tenant.security.suspended","com.scnehaux.organization.tenant.security.restored",
                "com.scnehaux.organization.projection.repair.reconciled"],
 "reconciliation_interval_seconds":900}
```

`reconciliation_interval_seconds` is 900 because identity-control reconciles itself every
`IDENTITY_ORGANIZATION_RECONCILE_INTERVAL`, 15 minutes by default (ADR-ORG-001 §5.7,
TDD-organization-control-002 §Reconciliation Interval). This service alerts on a consumer's
reconciliation age against the interval it declared, a warning past one and critical past two, so
the two must agree. Change both together.

The Membership, Tenant and repair types are identity-control's Tenant context projection
(identity-control `TDD-identity-control-002` 2.3.0, `ADR-IAM-006`): it projects each Tenant as a
Keycloak Organization and its active Memberships as members.

**A server registered before them** posts the registration again with this body. Re-registering
with other types replaces the subscription and clears the recorded snapshot mark (`TDD-organization-
control-002` §Consumer Registry), so identity-control then runs `provider-bootstrap` again (step 6):
it takes both snapshots and records the lower mark. Until it does, its progress reports are refused.

### 5. Deliver to identity-control

In this stack's `.env`, then `docker compose up -d organization-control`:

```text
ORGANIZATION_DELIVERY_TARGETS=identity-control=http://identity-control:8090/v1/deliveries
ORGANIZATION_IDENTITY_CONTROL_URL=http://identity-control:8090
```

The second line is where invitation acceptance asks whether the signed-in person holds the invited
address (`POST /v1/principals/{principal_id}/identifiers:match`, ADR-ORG-007 §5.3), with the same
workload token the dispatcher carries. Without it, `POST /v1/invitations/accept` answers `503`.

### 6. Point identity-control here

In identity-control's `deploy/dev/.env`, then, in its `deploy/dev`:
`docker compose up -d identity-control` and `docker compose run --rm provider-bootstrap`.

```text
IDENTITY_DELIVERY_PRINCIPAL_ID=<organization-control-workload's principal_id>
IDENTITY_ORGANIZATION_BASE_URL=http://organization-control:8080
```

### 6b. The development reporters

A reporter is a workload this service admits on one kind of report route, from its record here
(ADR-ORG-001 §5.11, amended 2026-10-20). Production's provisioning system and each obligation-owning
domain are one each. On this server the stand-ins of §Provisioning a Tenant and §Offboarding a Tenant
report as development reporters, so development runs production's path (STD-GLB-009 rule 13). For each
of `provisioning`, `obligation:product`, `obligation:hcm`, `obligation:billing` and
`obligation:audit`, `dev-wire.ps1`:

1. makes a key pair with the kernel's tool, beside the state file, never in a directory a container
   mounts;
2. creates a workload in identity-control, `dev-reporter-<what it reports>`, owned by the operator,
   with `organization-control-api` as its audience and the public key;
3. registers it here: `POST /v1/reporters` `{"principal_id", "reports", "purpose"}`;
4. writes its client and key file into the state file's `reporters`.

`GET /v1/reporters` lists them. A reporter is retired with `POST /v1/reporters/{reporter_id}/retire`
`{"expected_version"}`, which stops its next report.

### 6c. identity-control's invitation resolve

identity-control asks this service, as its own workload, which invitation a token names when a person
with no account enrols (ADR-IAM-011 §5.7, ROADMAP item 56). That workload is admitted to
`POST /v1/invitations/resolve` alone, from its own reporter entry, and not by its consumer
registration of step 4: each route has its own entry. `dev-wire.ps1` registers it with step 2's
`identity-control-workload`, the key identity-control signs in with, and no development flag:

```text
POST /v1/reporters
{"principal_id":"<identity-control-workload's principal_id>","reports":"invitation:resolve",
 "purpose":"identity-control resolves an invitation token for an enrolment (ADR-IAM-011 §5.7)"}
```

It writes the entry into the state file's `reporters` too, with identity-control's key file. Each
resolve writes a privileged-access row with the authority `reporter` and the reason
`invitation:resolve`, reviewed through the workload's owner.

### 7. What you should see

- identity-control logs `the provider projection is fresh` within 15 seconds.
- The dispatcher delivers step 4's grant. identity-control's `identity.ceremony_grant_retirement`
  holds one row naming it, and requests by the ceremony's Principal log `emergency provider
  authority used` with basis `emergency`, no longer `ceremony`.
- Stopping this service makes identity-control log the projection stale within a minute. The
  emergency grant still authorizes, and an activation would not.
- `provider-bootstrap` prints a `memberships` count, not `tenant context not bootstrapped`.
- Granting a Membership here makes identity-control log `tenant converged` for its Tenant. The
  kernel then holds an Organization named by the `tenant_id`, with the Principal as a member.
- A Membership a Tenant administrator grants here, with a token signed in for the Tenant through a
  `tenant-scoped` privileged client, appears in identity-control's
  `GET /v1/projections/tenant-context/report` and in the kernel's Organization for the Tenant.
- After the member's Keycloak user is deleted and `:relink`ed, the Membership is unchanged here and in
  that report, and the new user joins the Organization at the Tenant's next convergence.
- 20 revocations are timed from acceptance to the response, to publication and to identity-control's
  applied receipt, against 100 ms (p95) and 10 s (maximum).
- An invitation to another person's address is refused to the operator, who holds its token, and
  stays `pending`. An invitation to the operator's own proven address is accepted by the operator
  signed in with no Tenant, and creates the operator's Membership (step 10, ADR-ORG-007).

The proof deletes a Keycloak user and registers a client and a Tenant per run, so run it only
against a kernel and a database this server or a CI job owns.

## Provisioning a Tenant on the development server

A Tenant is created `requested` and becomes `active` only after the provisioning system reports that
its boundary exists (TDD-organization-control-003 §Tenant State Machine). **No provisioning system runs
on a development server.** You stand in for it with `scripts/dev-provisioning.ps1`, which reports the
outcome through the two routes the provisioning system calls, `POST /v1/provisioning/realized` and
`/failed`, as the development provisioning reporter of §Wiring to other services step 6b, after reading
the Tenant with your provider token (TDD-organization-control-003 1.18.0 §The Provisioning System on a
Development Server). Production has a real provisioning system and never runs it.

It needs the wiring done (§Wiring to other services): the state file `dev-wire.ps1` wrote, and the
same variables as the wiring scripts, `IDENTITY_CALLER_PASSWORD` and `KEYCLOAK_ISSUER` exported, and
the operator's TOTP file (identity-control's `deploy/dev/keys/operator-totp.json`, found on its own, or
`IDENTITY_OPERATOR_TOTP_FILE`).

1. **Request the Tenant**, in Organization Experience in provider mode (Tenants, Request tenant).
   It reads `requested`, "Awaiting provisioning".
2. **Start provisioning** on its row (Tenants, Start provisioning). The Tenant moves to
   `provisioning`, and its row shows the request's correlation identifier.
3. **Report the outcome**, from this repository's root:

   ```sh
   pwsh ./scripts/dev-provisioning.ps1 -IdentityRepo /home/development/apps/identity-control \
       -State ~/scnehaux-wiring/state.json -TenantId <tenant_id> -Realized
   ```

   `-CorrelationId <correlation_id>` names the Tenant by its request instead. To try the failure path,
   `-Failed -Reason "<why>"`: the Tenant becomes `failed` with that reason, and its row offers
   Retry provisioning, which makes a new request to report on.
4. **Activate** on its row (Tenants, Activate). The Tenant is `active`, and Memberships can be granted
   into it.

The script prints the Tenant, its request, what it reported and the result. It refuses, sending
nothing and exiting 2, when the Tenant is not `provisioning` (a `requested` one: start provisioning
first; a `failed` one: retry first) or when the latest request already has an outcome. It never
activates and never retries.

**A request that waited longer than `ORGANIZATION_PROVISIONING_TIMEOUT` (30 minutes) reads
`unresolved`**: the scheduled sweep ages any request with no outcome, including one nobody
dispatched. The console then says "outcome unknown, do not retry". It is resolved by reporting its
outcome, never by a retry (TDD-organization-control-003 §Resolving an `unresolved` Request):

- a Tenant still `requested`: Start provisioning (step 2) is still allowed, because it records the same
  attempt; then steps 3 and 4;
- a Tenant in `provisioning`: steps 3 and 4. The script accepts an `unresolved` request.

`deploy-dev` runs this script on every change (`scripts/dev-wiring-proof.ps1` steps 4 and 8), so the
procedure here is the one that was tested.

## Offboarding a Tenant on the development server

An offboarding waits on two parties that do not run on a development server: the domains that own its
obligations, which resolve them, and the provisioning system, which reports the deprovisioning release
sends. Without them, an obligation stays open and Release stays refused, and a released offboarding's
deprovisioning stays `requested`, ages to `unresolved`, and Retire stays disabled. You stand in for
both with two scripts, through the routes they call, as the development reporters of §Wiring to other
services step 6b; each reads first with your provider token (TDD-organization-control-004 1.15.0
§Offboarding on a Development Server). No provider token resolves an obligation:

- `scripts/dev-obligation.ps1` resolves one obligation as its domain, through
  `POST /v1/obligations/{obligation_id}/resolve`. Organization Experience offers no control for this on
  purpose: an operator there cannot resolve another domain's obligation.
- `scripts/dev-deprovisioning.ps1` reports the deprovisioning outcome, through
  `POST /v1/offboardings/{offboarding_id}/deprovisioning`.

Production has those parties and never runs either script. Both need what §Provisioning a Tenant on the
development server needs: the wiring done, the state file, `IDENTITY_CALLER_PASSWORD` and
`KEYCLOAK_ISSUER` exported, and the operator's TOTP file.

1. **Begin the offboarding**, in Organization Experience in provider mode (Tenants, Offboarding on an
   `active` or `suspended` Tenant's row, then Begin offboarding). Type the Tenant's name and a reason. Access to the Tenant stops
   at once; nothing is deleted, and it can be cancelled until Release. The console opens the
   offboarding's page.
2. **Freeze**: Suspend the next 100 until no active Membership remains, then Complete the freeze. The
   offboarding moves to `obligations`.
3. **Raise each obligation** the Tenant owes (Raise obligation: the domain, such as `product`, and the
   obligation, such as `data-export`). The board lists each with its `obligation_id`.
4. **Resolve each one as its domain**, from this repository's root:

   ```sh
   pwsh ./scripts/dev-obligation.ps1 -IdentityRepo /home/development/apps/identity-control \
       -State ~/scnehaux-wiring/state.json -OffboardingId <offboarding_id> \
       -ObligationId <obligation_id> -Domain product -Completed
   ```

   `-Waived -Reason "<why>"` records that someone decided it will not be done, and the board shows it as
   waived, never completed. `-Failed -Reason "<why>"` records that the domain could not do it: it still
   holds Release, and may be completed or waived later. `-Domain` must be the domain the obligation was
   raised against.
5. **Release**, on the offboarding's page, once no obligation is open or failed and no legal hold is
   set. This is irreversible: it publishes the deprovisioning command.
6. **Report the deprovisioning outcome**:

   ```sh
   pwsh ./scripts/dev-deprovisioning.ps1 -IdentityRepo /home/development/apps/identity-control \
       -State ~/scnehaux-wiring/state.json -OffboardingId <offboarding_id> -Realized
   ```

   To try the failure path, `-Failed -Reason "<why>"`: the page shows the failure with its reason and
   offers Send deprovisioning again, which records a new attempt to report on (step 6 again).
7. **Retire**, on the offboarding's page, typing the Tenant's name. The Tenant is `retired`.

Each script prints what it found, what it reported and the result, and the next step. It refuses,
sending nothing and exiting 2, when the step before is not done: `dev-obligation.ps1` for an obligation
not on the board, under another domain, already completed, waived or cancelled, or on a retired or
cancelled offboarding; `dev-deprovisioning.ps1` before Release, on a `failed` deprovisioning (send it
again first), on a `realized` one (retire), or on a retired or cancelled offboarding. Neither releases,
retires nor sends the command again.

**A deprovisioning that waited longer than `ORGANIZATION_PROVISIONING_TIMEOUT` (30 minutes) reads
`unresolved`**: the scheduled sweep ages requests in both directions, provisioning and deprovisioning
(TDD-organization-control-003 §Scheduled Sweeps). The page then says the outcome is unknown, and there
is no Send deprovisioning again for it: sending it again is the retry of an unknown outcome. It is
resolved by reporting its outcome, step 6; the script accepts an `unresolved` request.

`deploy-dev` runs both scripts on every change (`scripts/dev-wiring-proof.ps1` step 9), so the
procedure here is the one that was tested.

## Inviting a member on the development server

ADR-ORG-007. The person accepts the invitation signed in as themselves, and this service asks
identity-control whether they hold the invited address as a proven address. It needs §Wiring to other
services step 5 (`ORGANIZATION_IDENTITY_CONTROL_URL`); without it, acceptance answers `503`.

1. **The person exists, can sign in, and has proven the invited email** in identity-control. The
   address counts only once its holder has returned the code sent to it. On a development server the
   code reaches identity-control's mail catcher (its `deploy/dev/README.md`).
2. **In organization-experience, in the Tenant, as its administrator:** invite the email. The page
   shows the acceptance link once. Nothing on a development server delivers it, so send it to the
   person yourself. A lost link is revoked and issued again.
3. **The person opens the link** and signs in without choosing a Tenant, then accepts. Anyone else
   who opens the link is refused, members of the Tenant included.

`deploy-dev` runs this against the real identity-control on every change
(`scripts/dev-wiring-proof.ps1` step 10).

## Keys

One private key lives here: `./keys/organization-control-workload.pem`, the workload key this service
signs its token requests to Keycloak with, mounted read-only at `/keys`. It is made with the kernel's
key tool in §Wiring to other services, step 1, mode `0600` and owned by `KEYS_OWNER` (the image's
nonroot user, `65532:65532`, unless `.env` says otherwise). Only its public JWK leaves the server, to
identity-control's workload registration. `keys/` is git-ignored; back it up with `.env`
(§Backups).

The operator caller's key, `dev-provider-caller`, is yours rather than this stack's, and stays beside
the wiring state file outside every directory a container mounts.

## Backups

The Control Database lives only in the Docker volume `scnehaux-organization-control-dev_postgres`.
**`docker compose down -v` deletes it**, and with it every provider grant identity-control's provider
projection is built from (§Never do).

Back it up daily to storage outside the volume; on the development server that is
`/mnt/dev-storage`. `./backup.sh <directory>` writes two files: `globals-<date>.sql`, the cluster's
roles from `pg_dumpall --globals-only`, and `organization_control-<date>.dump`, the database from
`pg_dump --format=custom`. A database dump holds no roles, and the roles must exist before the
objects they own or are granted are restored. Both files hold role password hashes, so the script
makes them readable by their owner alone. It runs `pg_dump` inside the `postgres` container over its
local socket, which the image trusts, so no password is typed or stored. In the operator's crontab:

```text
30 2 * * * /home/development/apps/organization-control/deploy/dev/backup.sh /mnt/dev-storage/backups/organization-control >/dev/null
```

The cron line runs `backup.sh`, never a bare `pg_dump`, because the script is what writes the two
steps and keeps the count (STD-GLB-009 rule 9, 1.10.0):

- **Each file is written as `<name>.partial`**, with `umask 077`, and renamed only once its command
  succeeded. A failed run stops there and leaves at most a `.partial`, never a file that looks like a
  backup.
- **Retention is by count: the newest 14 complete sets.** Once both files of the new set are in
  place, the script deletes the complete sets older than the newest 14 and names each on stderr. A
  set is complete when both files of one `<date>` exist under their final names. A failed run
  deletes nothing, and the set just written is never deleted. A lone file or a `.partial` is not
  counted and not deleted: look at it, then remove it by hand. Never prune with an age-based
  `find -mtime +N -delete`, which deletes the last good sets once backups have stopped for N days.
- **The directory is on this host.** A copy off the host is required before production; a
  development server is redeployed from zero, so STD-GLB-009 accepts the gap there and nowhere else.

Copy `.env` and `keys/` alongside when they change; without them a restored database has no
credentials to match and the service no workload key.

To restore into an empty volume, put `.env` and `keys/` back first, then:

```sh
PAUSE_REASON="INC-<n> restored from the <date> backup" \
./restore.sh /mnt/dev-storage/backups/organization-control/globals-<date>.sql \
             /mnt/dev-storage/backups/organization-control/organization_control-<date>.dump
docker compose up -d --build
curl -fsS http://127.0.0.1:8083/readyz
```

`restore.sh` starts `postgres` alone and refuses a cluster that already holds
`organization_control`: replacing a live database is a decision, made by deleting the volume, never a
side effect. It applies the roles, allowing only the bootstrap superuser's harmless "role already
exists", then restores the database whole with `pg_restore --create --exit-on-error`, which stops at
the first error. The migrate job that `up` runs then applies anything newer and asserts RLS, the
privileges and the five login roles' passwords from `.env` again.

Last, `restore.sh` runs the restore bump before anything serves (ADR-ORG-003 §5.5): it builds the
migrate image, runs the migrate job, then `docker compose --profile restore-bump run --rm restore-bump`,
which adds 10^9 to every Tenant version, Tenant security version and provider grant version, publishes
the withdrawn Tenants and grants at their new versions and no active one, and asks every consumer to
reconcile. A restore to an older point gives back versions consumers have already seen superseded, and
they discard a lower version as already applied. `RESTORE_BUMP=0` skips it; only the drill sets it,
because it compares the restored tables with the source first and bumps after.

`PAUSE_REASON` makes `restore.sh` record a pause of Tenant administration in the restored database
before anything serves, so no Tenant administrator changes authority until the runbook's
reconciliation is done; a provider lifts it with `POST /v1/tenant-administration-pause`
`{"paused": false}`. A backup taken before the table existed (TDD-organization-control-001 1.22.0)
cannot hold the row: `restore.sh` then stops after the restore, and the pause is made through the
API as soon as the service answers. The drill leaves it unset.

Until 2026-10-08 this section ran `migrate` first, for the roles, and then `pg_restore --clean` over
the schema it had built. That suits only a dump of the same release: a dump of an older one would
restore older migration history over tables the newer migrate job had made, and the next migration
would fail.

**What is proven, and what is not.** `deploy-dev` runs both scripts on every change and daily, as
the restore drill `scripts/dev-restore-drill.sh` (STD-GLB-002 §Restore Evidence): it begins an
offboarding, backs the wired stack up, deletes its volume, restores, and compares schema, migration
version, every table, sequence and role with the source. The outbox, its deliveries, the receipts
and the consumer registry must be among the non-empty tables. It also seeds a directory with 16 older
complete sets and requires `backup.sh` to keep exactly the newest 14, the one it wrote among them, and
to fail and delete nothing while the database is down. It then starts the service, reads the
provider grants, Organizations and offboardings through the API, and times the recovery against the
15-minute RTO. The evidence is the job's `restore-evidence` artifact. A daily dump loses up to 24
hours, against the 1-minute RPO of `PAD-PLT-002 §6.2`. A restore to an older point is reconciled by
the runbook's procedure, which moves each Membership a consumer holds at a higher version past it
(`POST /v1/projections/advance-versions`, SAD-004 §6.6); the drill does not exercise it. The drill does
run the restore bump on the restored database, and requires every Tenant version at 10^9 or above,
only withdrawals published, every consumer asked to reconcile, and identity-control holding a bumped
Tenant version. The runbook is
[`docs/runbooks/organization-database-restore.md`](../../docs/runbooks/organization-database-restore.md).

## Never do

- **Never run `docker compose down -v`.** The volume holds Organization's provider grants, and the
  Identity Control API's provider projection is built from them. A new database would revoke every
  grant there, including the one that makes the ceremony's Principal a provider. `docker compose down`
  without `-v` keeps it.
- Never rename the Compose project, its networks or its volume (§What runs).
- Never source identity-control's `.env` into the shell that runs these commands: its
  `POSTGRES_PASSWORD` overrides this stack's (§Wiring to other services).
- Never send a non-ASCII `X-Administrative-Reason`, or a command without an `Idempotency-Key`: both
  are refused `400`.
- Never commit `.env`, `keys/` or `compose.override.yaml`.

## Troubleshooting

| Symptom | Cause, and what to do |
| :-- | :-- |
| `/readyz` never answers after `up` | The migrate job failed, and the service waits on it: `docker compose logs migrate` |
| `network scnehaux-identity-api not found` (or `-identity-control-api`) | The kernel's or identity-control's stack is not running. Start them first (§Before you start) |
| The build fails fetching modules, `x509: certificate signed by unknown authority` | The module proxy is SSL-inspected. `GOPROXY=direct` in `.env`, then `docker compose up -d --build` |
| `network ... needs to be recreated` after adding the override | The subnet changed. `docker compose down` (no `-v`), then `docker compose up -d` |
| `maintenance` exits 3 | An unresolved dead letter is older than 24 hours. The run's work is done; resolve the incident |
| A command answers `400` naming `Idempotency-Key` | Every command requires one: a value unique to the command, repeated unchanged on a retry |
| A Tenant stays "Awaiting provisioning" | Nothing reports provisioning outcomes on a development server: §Provisioning a Tenant on the development server |
| `dev-provisioning.ps1` exits 2 | It refused and sent nothing; its `refused` line says why and what to do first |
| Release stays disabled, "obligations are outstanding" | An obligation is open or failed, and on a development server no domain resolves it: `dev-obligation.ps1`, §Offboarding a Tenant on the development server, step 4 |
| Retire stays disabled after Release | Nothing reports deprovisioning outcomes on a development server: `dev-deprovisioning.ps1`, §Offboarding a Tenant on the development server, step 6. A failed one is sent again first |
| `dev-obligation.ps1` or `dev-deprovisioning.ps1` exits 2 | It refused and sent nothing; its `refused` line says why and what to do first |
| `maintenance` never ran | The cron line is missing or cron is not running: §One-off tasks, Check that it is installed and runs |
| identity-control logs the provider projection stale | This service is down, or delivery is not configured: §Wiring to other services, steps 5 to 7 |
