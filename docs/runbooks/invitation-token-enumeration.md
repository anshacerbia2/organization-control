# Runbook: invitation token enumeration

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-09.

`POST /v1/invitations/lookup` is the one anonymous route this service serves. SAD-004 §8.1 permits
it only with enumeration resistance. "A rising unauthenticated lookup rate from one source is token
enumeration in progress, and the uniform response is what makes it expensive rather than impossible"
(TDD-organization-control-004 §Operational Notes). This runbook confirms an enumeration and stops it
at the edge.

## Trigger

- The lookup rate rises: `rate(organization_invitation_lookups_total[5m])` against its usual level.
  Every request to the route is counted, malformed tokens included.
- The gateway or reverse proxy in front of the service reports many `POST /v1/invitations/lookup`
  from one source.
- `http request completed` log lines with `http.path=/v1/invitations/lookup` rise.

No alert rule reads the counter (see Gaps).

## Impact

- The lookup answers `{"accepted": true, "next": "verify-identity"}` to every well-formed token, and
  `400` to a malformed one, and reads nothing to do it (TDD-004 §Acceptance, and Enumeration
  Resistance). An enumeration learns nothing from it: not whether a token exists, not a Tenant, not an
  inviter, and no timing difference, since no query runs.
- The token is 32 random bytes (`internal/invitation`, `tokenBytes`). Guessing one is not a practical
  attack. A sustained enumeration is still a signal: someone believes the token space is worth
  probing, perhaps holding leaked links.
- The resolution of a token happens only on authenticated paths: `POST /v1/invitations/accept` (a
  Tenant-scoped command) and the provider's `POST /v1/invitations/verify-identity`. An anonymous
  caller reaches neither.
- Load. The route is cheap, and the platform chain sheds overload before the handler runs.

## Diagnosis

1. Confirm the rise is real: compare `rate(organization_invitation_lookups_total[5m])` with the same
   hour on previous days.
2. Find the source. This service's request log carries the method, path, status and duration, and no
   client address. The source is in the gateway's or reverse proxy's access log for the same window.
3. Check the shape of the traffic in the service log: a high share of `400` (malformed tokens) on the
   lookup path is blind guessing; well-formed tokens at a high rate from one source can be a list of
   real links.
4. If real links may have leaked, ask which Tenants issued invitations recently: each Tenant
   administrator reads `GET /v1/invitations?state=pending` (and `state=identity_verified`) for its own Tenant.

## Decision table

| Finding | Act |
| :-- | :-- |
| One or a few sources, mostly malformed tokens | Block or rate-limit the sources at the edge (A) |
| Well-formed tokens at a high rate | Edge block (A), and treat links as possibly leaked (B) |
| Many sources, distributed | Edge rate limit on the route (A); escalate to security |
| The rate rose with a product launch or a bulk invitation | No incident; record the new usual level |

## Remediation

- **A. At the edge.** Block or rate-limit the source addresses on `POST /v1/invitations/lookup` at the
  gateway or reverse proxy. This service has no per-source control of its own, and a lookup that
  answers differently under attack would become the oracle the uniform response prevents.
- **B. Possibly leaked links.** The affected Tenant's administrator revokes pending invitations,
  `POST /v1/invitations/{invitation_id}/revoke` (`Idempotency-Key` required), and issues new ones.
  A provider expires those already past their lifetime estate-wide:
  `POST /v1/invitations/expire-lapsed` `{"size": 100}` (provider, reason). A leaked token still needs
  an authenticated Principal whose identity the identity flow verifies before it grants anything
  (SAD-004 §5.5: invitation possession never proves identity).

## Verification

- `rate(organization_invitation_lookups_total[5m])` returns to its usual level, and the gateway shows
  the sources refused.
- For B: the revoked invitations read `"state": "revoked"` in `GET /v1/invitations/{invitation_id}`.

## Escalation

- Distributed enumeration, or evidence of leaked links: security lead.
- Edge controls: the platform team that runs the gateway.

## Gaps

- **No alert rule.** TDD-004 §Operational Notes sets the warning at "above baseline" and states no
  number, and no baseline has been measured. The counter is exported so one can be.
- **No source on the counter.** A per-source label would make one series per client address.
  Prometheus: "Remember that every unique combination of key-value label pairs represents a new time
  series", and "Do not use labels to store dimensions with high cardinality (many different label
  values), such as user IDs, email addresses, or other unbounded sets of values" [R2]. The
  "sustained from one source" critical belongs to the edge, which sees the address.
- This service's request log carries no client address.

## References

| # | Source |
| :-- | :-- |
| R1 | TDD-organization-control-004 §Acceptance, and Enumeration Resistance, §Operational Notes; SAD-004 §5.5, §8.1 |
| R2 | Prometheus, *Metric and label naming*, "Labels", <https://prometheus.io/docs/practices/naming/>, accessed 2026-10-09: "Remember that every unique combination of key-value label pairs represents a new time series, which can dramatically increase the amount of data stored. Do not use labels to store dimensions with high cardinality (many different label values), such as user IDs, email addresses, or other unbounded sets of values." |
| R3 | NIST SP 800-61r3, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "Playbooks provide actionable steps or tasks for people to perform during various scenarios or situations." |
