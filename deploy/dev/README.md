# organization-control on the development server

Organization Control beside identity-kernel's and identity-control's stacks, deployed the way every
service on the server is (STD-GLB-009 §Development Server Deployment).

| Service | What it is |
| :-- | :-- |
| `postgres` | This service's own Control Database, in the named volume `postgres` |
| `migrate` | One-shot: the cluster roles, the owned schemas, the platform schema, RLS, privileges, the five runtime login roles, and a check of the privilege shape |
| `organization-control` | The API, with its delivery dispatcher in process (ADR-GLB-018 §5.4) |
| `bootstrap-provider` | One-off task: the first provider grant |
| `maintenance` | One-off task: the daily maintenance stage |

**Never run `docker compose down -v`.** The volume holds Organization's provider grants, and the
Identity Control API's provider projection is built from them. A new database would revoke every grant
there, including the one that makes the ceremony's Principal a provider.

## Before you start

- identity-kernel's stack is running. Its `scnehaux-identity-api` network carries Keycloak.
- identity-control's stack is running, on a version that creates the `scnehaux-identity-control-api`
  network, and has moved to `identity-control-api` (its README §Moving a server). This stack joins
  both networks, so it starts after them.
- You can get a provider token from identity-control (its README §Calling the API): the ceremony's
  Principal, `bootstrap-operator`.

## First start

```sh
cd organization-control/deploy/dev
cp .env.example .env
# fill in KEYCLOAK_ISSUER (the value identity-control's .env holds) and the six passwords:
#   openssl rand -hex 32
docker compose up -d --build
curl -fsS http://127.0.0.1:8083/readyz       # ready once the migration job has succeeded
```

The service starts with no provider and no dispatcher. The next two sections make it useful.

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

## Wiring to other services

This connects Organization Control and the Identity Control API through workload tokens from the
kernel, each with the other side's resource as its audience (TDD-identity-control-006,
STD-IAM-002 §3.1). It is the server form of identity-control's `docs/run.md` §8.

**The order matters.** Each step needs a token the step before still accepts. Step 4's grant retires
the ceremony's own grant in identity-control once it is projected, permanently. From then on the
ceremony's Principal is a provider through this database alone, which is why the volume above is
never dropped.

Use plain ASCII in every `X-Administrative-Reason`. Anything else is refused with `400`
(STD-GLB-001 §Request Header Values).

**Run it as a script.** `scripts/dev-wire.ps1` is steps 1 to 6 below, and `scripts/dev-wiring-proof.ps1`
is step 7. They are the files CI runs against the three stacks on every change and every night
(`.github/workflows/deploy-dev.yml`, STD-GLB-009 1.3.0), so the procedure on the server is the
procedure that was tested:

```sh
export IDENTITY_CALLER_PASSWORD=...            # the bootstrap operator's, from identity-control's .env
export IDENTITY_CALLER_KEY_FILE=...            # identity-control-caller.pem, from the same .env
export IDENTITY_OPERATOR_TOTP_FILE=...         # the operator's TOTP file, if dev-token.ps1 keeps one
pwsh ./scripts/dev-wire.ps1 -IdentityRepo /srv/identity-control -KernelDeployDir /srv/identity-kernel/deploy/dev \
    -Operator "<you>" -State ~/scnehaux-wiring/state.json
pwsh ./scripts/dev-wiring-proof.ps1 -IdentityRepo /srv/identity-control -State ~/scnehaux-wiring/state.json
```

Export those three alone. Sourcing identity-control's `.env` would also export its `POSTGRES_PASSWORD`,
which overrides this stack's when the script runs `docker compose` here. The state file's directory
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
so a retry is safe:

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
                "com.scnehaux.organization.projection.repair.reconciled"]}
```

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
```

### 6. Point identity-control here

In identity-control's `deploy/dev/.env`, then, in its `deploy/dev`:
`docker compose up -d identity-control` and `docker compose run --rm provider-bootstrap`.

```text
IDENTITY_DELIVERY_PRINCIPAL_ID=<organization-control-workload's principal_id>
IDENTITY_ORGANIZATION_BASE_URL=http://organization-control:8080
```

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
