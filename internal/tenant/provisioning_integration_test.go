package tenant

// Tenant intake and provisioning correlation, against a real engine as
// `organization_provider_app`.
//
// In-package for the same reason as the transition suite: the clock and the pre-append seams are
// unexported on purpose, and the two properties that matter most here — that intake's three writes
// commit together, and that a provisioning transition publishes nothing — can only be falsified from
// inside.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// seedSponsor creates an Organization for one test to hang a Tenant from.
//
// Its own rather than the suite's, because `go test ./...` runs packages concurrently against one
// database and these tests retire and suspend what they create.
func (f *fixture) seedSponsor(t *testing.T, status string) id.UUID {
	t.Helper()

	organizationID := mustID(t)
	f.exec(t, `INSERT INTO organization.organization
	    (organization_id, display_name, classification, status)
	VALUES ($1, 'provisioning-suite sponsor', 'customer', $2)`, organizationID.String(), status)

	t.Cleanup(func() {
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`,
			organizationID.String())
	})
	return organizationID
}

// forget registers the cleanup for a Tenant the service minted, which the test could not name in
// advance.
func (f *fixture) forget(t *testing.T, tenantID id.UUID) {
	t.Helper()
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.provisioning_request WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
	})
}

// withCorrelation returns a context carrying a fresh correlation identifier.
//
// Fresh per Tenant, because the correlation identifier is what an outcome is matched on and the
// fixture's own scope reuses one for the whole test — which is a real condition
// (`TestOneCorrelationAcrossTwoTenantsIsRefused` relies on it) and the wrong default everywhere else.
func (f *fixture) withCorrelation(t *testing.T) (context.Context, id.UUID) {
	t.Helper()

	correlation := mustID(t)
	scope, err := db.ProviderScope(f.actor, correlation)
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}
	return db.WithScope(f.ctx, scope), correlation
}

// coordinator builds a coordinator whose clock the test controls, so the sweep's cutoff is a value
// the test chose rather than whatever the wall clock says.
func (f *fixture) coordinator(t *testing.T, timeout time.Duration, now time.Time) *Coordinator {
	t.Helper()

	c, err := NewCoordinator(f.service.pool, f.service, timeout)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.now = func() time.Time { return now }
	return c
}

type requestRow struct {
	requestID  id.UUID
	state      RequestState
	detail     string
	resolvedAt *time.Time
	profile    map[string]any
}

// requestRows reads every provisioning request for a Tenant, newest first.
func (f *fixture) requestRows(t *testing.T, tenantID id.UUID) []requestRow {
	t.Helper()

	var all []requestRow
	if err := db.WithProviderScope(f.ctx, f.service.pool, "provisioning suite read",
		func(ctx context.Context, tx db.Tx) error {
			rows, err := tx.Query(ctx, `SELECT request_id::text, state, coalesce(detail, ''),
			       resolved_at, desired_profile
			FROM tenant.provisioning_request
			WHERE tenant_id = $1
			ORDER BY requested_at DESC, request_id DESC`, tenantID.String())
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var (
					next    requestRow
					rawID   string
					state   string
					rawJSON []byte
				)
				if err := rows.Scan(&rawID, &state, &next.detail, &next.resolvedAt, &rawJSON); err != nil {
					return err
				}
				if next.requestID, err = id.Parse(rawID); err != nil {
					return err
				}
				next.state = RequestState(state)
				if err := json.Unmarshal(rawJSON, &next.profile); err != nil {
					return err
				}
				all = append(all, next)
			}
			return rows.Err()
		}); err != nil {
		t.Fatalf("read provisioning requests: %v", err)
	}
	return all
}

// requestedPayload reads the body of the one desired-state publication for a Tenant.
func (f *fixture) requestedPayload(t *testing.T, tenantID id.UUID) RequestedPayload {
	t.Helper()

	var raw []byte
	if err := db.WithProviderScope(f.ctx, f.service.pool, "provisioning suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT envelope->'data'
			FROM platform.outbox
			WHERE aggregate_id = $1 AND event_type = $2`,
				tenantID.String(), requestedEventType).Scan(&raw)
		}); err != nil {
		t.Fatalf("read the desired-state publication: %v", err)
	}

	var payload RequestedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode the desired-state payload: %v", err)
	}
	return payload
}

func (f *fixture) request(t *testing.T, ctx context.Context, sponsor id.UUID) Requested {
	t.Helper()

	requested, err := f.service.Request(ctx, RequestTenant{
		OrganizationID:   sponsor,
		DisplayName:      "provisioning-suite subject",
		IsolationProfile: ProfileSilo,
		ResidencyRegion:  "ap-southeast-3",
		Reason:           "asserted by the provisioning suite",
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	f.forget(t, requested.Tenant.TenantID)
	return requested
}

func outcome(correlation id.UUID, detail string) Outcome {
	return Outcome{
		CorrelationID: correlation,
		Detail:        detail,
		Reason:        "asserted by the provisioning suite",
	}
}

// TestIntakeCreatesTheTenantItsRequestAndItsPublicationTogether is the Week 4 gap.
//
// Three writes, one transaction. A Tenant persisted without its provisioning request is a Tenant
// nothing was ever asked to build; a request recorded without the publication waits for a realized
// status no system was told to produce. Neither announces itself — both look like slow provisioning —
// which is why the atomicity is asserted rather than assumed.
func TestIntakeCreatesTheTenantItsRequestAndItsPublicationTogether(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)

	requested := f.request(t, ctx, sponsor)

	if requested.Tenant.Status != StateRequested {
		t.Errorf("intake left the Tenant %s, want %s", requested.Tenant.Status, StateRequested)
	}
	if requested.Tenant.Version != 1 || requested.Tenant.SecurityVersion != 1 {
		t.Errorf("intake wrote version %d and security version %d, want 1 and 1",
			requested.Tenant.Version, requested.Tenant.SecurityVersion)
	}
	if requested.CorrelationID != correlation {
		t.Errorf("intake reported correlation %s, want the scope's %s",
			requested.CorrelationID, correlation)
	}
	if requested.AcceptedAt != f.fixed {
		t.Errorf("intake accepted at %s, want the fixed instant %s", requested.AcceptedAt, f.fixed)
	}

	stored := f.read(t, requested.Tenant.TenantID)
	if stored.status != StateRequested {
		t.Errorf("the stored status is %s, want %s", stored.status, StateRequested)
	}
	if stored.activatedAt != nil {
		t.Error("intake stamped activated_at, and nothing has been activated")
	}

	rows := f.requestRows(t, requested.Tenant.TenantID)
	if len(rows) != 1 {
		t.Fatalf("intake wrote %d provisioning requests, want exactly 1", len(rows))
	}
	if rows[0].requestID != requested.RequestID {
		t.Errorf("the stored request is %s and the reported one is %s",
			rows[0].requestID, requested.RequestID)
	}
	if rows[0].state != RequestRequested {
		t.Errorf("the request is %s, want %s", rows[0].state, RequestRequested)
	}
	if rows[0].resolvedAt != nil {
		t.Error("intake resolved its own request, which would mean nothing is waiting on anything")
	}

	// The operation is written rather than defaulted. `internal/offboarding` writes `deprovision`
	// into this table and the activation predicate filters on the field, so a row that relied on the
	// default would be correct until somebody added a third direction.
	if rows[0].profile["operation"] != "provision" {
		t.Errorf("the desired profile records operation %v, want provision", rows[0].profile["operation"])
	}
	if rows[0].profile["isolation_profile"] != string(ProfileSilo) {
		t.Errorf("the desired profile records isolation %v, want %s",
			rows[0].profile["isolation_profile"], ProfileSilo)
	}

	// The publication carries the desired profile, which is the whole point of it. A
	// `tenant.lifecycle.requested` event with only the identifiers would tell the provisioning system
	// that something should exist and not what to build.
	payload := f.requestedPayload(t, requested.Tenant.TenantID)
	switch {
	case payload.TenantID != requested.Tenant.TenantID:
		t.Errorf("the publication names Tenant %s, want %s", payload.TenantID, requested.Tenant.TenantID)
	case payload.TenantStatus != StateRequested:
		t.Errorf("the publication carries status %s, want %s", payload.TenantStatus, StateRequested)
	case payload.IsolationProfile != ProfileSilo:
		t.Errorf("the publication carries profile %s, want %s", payload.IsolationProfile, ProfileSilo)
	case payload.ResidencyRegion != "ap-southeast-3":
		t.Errorf("the publication carries region %q, want ap-southeast-3", payload.ResidencyRegion)
	case payload.CorrelationID != correlation:
		t.Errorf("the publication carries correlation %s, want %s", payload.CorrelationID, correlation)
	case payload.ProvisioningRequestID != requested.RequestID:
		t.Errorf("the publication carries request %s, want %s",
			payload.ProvisioningRequestID, requested.RequestID)
	}

	// The standard lane. Creation stops no access, and the reserved lane exists so a live revocation
	// is not queued behind a bulk onboarding.
	all := f.events(t, requested.Tenant.TenantID)
	if len(all) != 1 {
		t.Fatalf("intake published %d events, want exactly 1", len(all))
	}
	// The named constant rather than its value: the dispatcher claims with `ORDER BY priority ASC`,
	// so the reserved lane is the lower number and a literal here would read backwards.
	if all[0].priority != outbox.PriorityStandard {
		t.Errorf("the desired-state publication took priority %d, want the standard lane",
			all[0].priority)
	}
	if !all[0].occurred.Equal(f.fixed) {
		t.Errorf("the envelope time is %s and the accepted instant is %s", all[0].occurred, f.fixed)
	}
}

// TestIntakeWritesNothingWhenThePublicationFails falsifies the atomicity claim by failing inside the
// window it protects.
//
// The seam fires after the two inserts and before the append. If the three writes were separately
// committable, the Tenant and its request would survive this and wait forever for a realized status
// against a command nobody was told about.
func TestIntakeWritesNothingWhenThePublicationFails(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)

	sentinel := errors.New("the publication failed")
	f.service.beforeAppend = func(context.Context) error { return sentinel }

	if _, err := f.service.Request(ctx, RequestTenant{
		OrganizationID:   sponsor,
		DisplayName:      "provisioning-suite subject",
		IsolationProfile: ProfilePooled,
		Reason:           "asserted by the provisioning suite",
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Request returned %v, want the injected failure", err)
	}
	f.service.beforeAppend = nil

	var tenants, requests int
	if err := db.WithProviderScope(f.ctx, f.service.pool, "provisioning suite read",
		func(ctx context.Context, tx db.Tx) error {
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM tenant.tenant WHERE organization_id = $1`,
				sponsor.String()).Scan(&tenants); err != nil {
				return err
			}
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM tenant.provisioning_request WHERE correlation_id = $1`,
				correlation.String()).Scan(&requests)
		}); err != nil {
		t.Fatalf("count what survived: %v", err)
	}

	if tenants != 0 {
		t.Errorf("%d Tenants survived a failed publication, want 0", tenants)
	}
	if requests != 0 {
		t.Errorf("%d provisioning requests survived a failed publication, want 0", requests)
	}
}

// TestATenantMayBeRequestedUnderASponsorBeingOnboarded is the design's own asymmetry.
//
// TDD-organization-control-003 §"Tenant Activation" puts the sponsor-active check at activation and
// not at creation, because a Tenant may legitimately be requested while its Organization is still
// being onboarded. A check moved earlier would be tidier and would refuse a real sequence.
func TestATenantMayBeRequestedUnderASponsorBeingOnboarded(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "suspended")
	ctx, _ := f.withCorrelation(t)

	requested := f.request(t, ctx, sponsor)
	if requested.Tenant.Status != StateRequested {
		t.Errorf("intake under a suspended sponsor left the Tenant %s, want %s",
			requested.Tenant.Status, StateRequested)
	}
}

// TestATenantCannotBeRequestedUnderARetiredSponsor closes the other direction.
//
// An Organization retires only once no live Tenant remains beneath it — `organization.Retire`
// refuses otherwise and names them. Creating one under a retired sponsor would falsify the very
// refusal that let it retire, and no reconciliation would notice.
func TestATenantCannotBeRequestedUnderARetiredSponsor(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "retired")
	ctx, _ := f.withCorrelation(t)

	_, err := f.service.Request(ctx, RequestTenant{
		OrganizationID:   sponsor,
		DisplayName:      "provisioning-suite subject",
		IsolationProfile: ProfilePooled,
		Reason:           "asserted by the provisioning suite",
	})
	if !errors.Is(err, ErrTransitionRefused) {
		t.Fatalf("Request under a retired sponsor returned %v, want ErrTransitionRefused", err)
	}
}

func TestAnAbsentSponsorIsNotFound(t *testing.T) {
	f := newFixture(t)
	ctx, _ := f.withCorrelation(t)

	_, err := f.service.Request(ctx, RequestTenant{
		OrganizationID:   mustID(t),
		DisplayName:      "provisioning-suite subject",
		IsolationProfile: ProfilePooled,
		Reason:           "asserted by the provisioning suite",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Request under an absent sponsor returned %v, want ErrNotFound", err)
	}
}

// TestAMalformedIntakeNeverReachesTheDatabase covers the four refusals that carry ErrInvalid.
//
// Each must be ErrInvalid rather than a bare error: the HTTP surface answers 400 from it, and before
// the sentinel existed a caller who omitted a field was told the service was broken.
func TestAMalformedIntakeNeverReachesTheDatabase(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, _ := f.withCorrelation(t)

	f.service.displayNameMax = 10

	cases := map[string]RequestTenant{
		"no sponsor": {
			DisplayName: "short", IsolationProfile: ProfilePooled, Reason: "because",
		},
		"no display name": {
			OrganizationID: sponsor, IsolationProfile: ProfilePooled, Reason: "because",
		},
		"a display name past the bound": {
			OrganizationID: sponsor, DisplayName: strings.Repeat("n", 11),
			IsolationProfile: ProfilePooled, Reason: "because",
		},
		"an undeclared isolation profile": {
			OrganizationID: sponsor, DisplayName: "short",
			IsolationProfile: IsolationProfile("hyperscale"), Reason: "because",
		},
		"no reason": {
			OrganizationID: sponsor, DisplayName: "short", IsolationProfile: ProfilePooled,
		},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.service.Request(ctx, req); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Request with %s returned %v, want ErrInvalid", name, err)
			}
		})
	}

	// The bound is the deployment's, and a nonsensical one is ignored rather than applied. A zero
	// bound would refuse every display name, and a configuration mistake that stops all Tenant
	// creation is worse than one that keeps the documented default.
	service, err := New(f.service.pool, WithDisplayNameMax(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.displayNameMax != DefaultDisplayNameMax {
		t.Errorf("a zero bound was applied as %d, want the default %d",
			service.displayNameMax, DefaultDisplayNameMax)
	}
}

// TestGetReadsTheWholeTenant is the read behind `GET /v1/tenants/{tenant_id}`.
//
// The stamps matter as much as the status: each is nil until its transition sets it, and a reader
// that could not tell "never suspended" from "suspended" would filter every report wrongly.
func TestGetReadsTheWholeTenant(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, _ := f.withCorrelation(t)

	requested := f.request(t, ctx, sponsor)

	record, err := f.service.Get(ctx, requested.Tenant.TenantID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	switch {
	case record.TenantID != requested.Tenant.TenantID:
		t.Errorf("Get returned Tenant %s, want %s", record.TenantID, requested.Tenant.TenantID)
	case record.OrganizationID != sponsor:
		t.Errorf("Get returned sponsor %s, want %s", record.OrganizationID, sponsor)
	case record.DisplayName != "provisioning-suite subject":
		t.Errorf("Get returned display name %q", record.DisplayName)
	case record.IsolationProfile != ProfileSilo:
		t.Errorf("Get returned profile %s, want %s", record.IsolationProfile, ProfileSilo)
	case record.ResidencyRegion != "ap-southeast-3":
		t.Errorf("Get returned region %q, want ap-southeast-3", record.ResidencyRegion)
	case record.Status != StateRequested:
		t.Errorf("Get returned status %s, want %s", record.Status, StateRequested)
	case record.ActivatedAt != nil, record.SuspendedAt != nil:
		t.Error("Get returned a lifecycle stamp for a transition that has not happened")
	case record.CreatedAt.IsZero():
		t.Error("Get returned no creation instant")
	}

	if _, err := f.service.Get(ctx, mustID(t)); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of an absent Tenant returned %v, want ErrNotFound", err)
	}
	if _, err := f.service.Get(ctx, id.UUID{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Get of a nil identifier returned %v, want ErrInvalid", err)
	}
}

// TestTheWholeProvisioningPathReachesActive walks intake to activation.
//
// It is the test that would have caught the gap this work closed: every piece existed except the
// path between them, so the front half of the state machine was reachable in `Resolve` and
// unreachable in practice.
func TestTheWholeProvisioningPathReachesActive(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	requested := f.request(t, ctx, sponsor)
	tenantID := requested.Tenant.TenantID

	provisioned, err := coordinator.Provision(ctx, command(tenantID, 1))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if provisioned.Tenant.Status != StateProvisioning {
		t.Fatalf("Provision left the Tenant %s, want %s", provisioned.Tenant.Status, StateProvisioning)
	}
	if provisioned.Published {
		t.Error("the dispatch published an event, and the three provisioning transitions are silent")
	}

	resolution, err := coordinator.Realize(ctx, outcome(correlation, "boundary built"))
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}
	switch {
	case resolution.State != RequestRealized:
		t.Errorf("Realize recorded %s, want %s", resolution.State, RequestRealized)
	case resolution.Replay:
		t.Error("the first delivery reported itself as a replay")
	case resolution.RequestID != requested.RequestID:
		t.Errorf("Realize resolved request %s, want the one intake recorded, %s",
			resolution.RequestID, requested.RequestID)
	}

	// Realized is not active. Activation also checks the sponsoring Organization, which is a decision
	// about the customer relationship rather than about infrastructure — and the provisioning system
	// has no view of it.
	if resolution.Tenant.Status != StateProvisioning {
		t.Errorf("a realized status left the Tenant %s, want it awaiting explicit activation in %s",
			resolution.Tenant.Status, StateProvisioning)
	}

	activated, err := f.service.Activate(ctx, command(tenantID, resolution.Tenant.Version))
	if err != nil {
		t.Fatalf("Activate after a realized status: %v", err)
	}
	if activated.Tenant.Status != StateActive {
		t.Fatalf("Activate left the Tenant %s, want %s", activated.Tenant.Status, StateActive)
	}

	// Two events across the whole path: the desired-state publication and the activation. The three
	// provisioning transitions in between are silent, and the silence is specified rather than
	// omitted.
	all := f.events(t, tenantID)
	if len(all) != 2 {
		t.Fatalf("the path published %d events, want 2", len(all))
	}
	if all[0].eventType != requestedEventType {
		t.Errorf("the first event is %s, want %s", all[0].eventType, requestedEventType)
	}
	if all[1].eventType != "com.scnehaux.organization.tenant.lifecycle.activated" {
		t.Errorf("the second event is %s, want the activation", all[1].eventType)
	}

	// Activation invalidates nothing, so the security version is untouched across the whole path.
	stored := f.read(t, tenantID)
	if stored.securityVersion != 1 {
		t.Errorf("the security version moved to %d across a path that invalidates no context",
			stored.securityVersion)
	}
}

// TestADuplicateRealizedStatusProducesOneEffect is TDD-organization-control-003 §Testing.
//
// A provisioning system retrying a delivery it is unsure arrived is behaving correctly, so the
// replay reports success and changes nothing. Answering it with a failure would make correct
// behaviour look broken; applying it twice would advance the Tenant a second time.
func TestADuplicateRealizedStatusProducesOneEffect(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	requested := f.request(t, ctx, sponsor)
	tenantID := requested.Tenant.TenantID

	first, err := coordinator.Realize(ctx, outcome(correlation, "boundary built"))
	if err != nil {
		t.Fatalf("the first Realize: %v", err)
	}
	second, err := coordinator.Realize(ctx, outcome(correlation, "boundary built"))
	if err != nil {
		t.Fatalf("the second Realize: %v", err)
	}

	if !second.Replay {
		t.Error("the second delivery did not report itself as a replay")
	}
	if second.Tenant.Version != first.Tenant.Version {
		t.Errorf("the replay moved the version from %d to %d",
			first.Tenant.Version, second.Tenant.Version)
	}

	rows := f.requestRows(t, tenantID)
	if len(rows) != 1 {
		t.Errorf("the replay left %d provisioning requests, want 1", len(rows))
	}
}

// TestARealizedStatusAdvancesATenantStillAwaitingItsDispatch is the ordering the design's happy path
// does not describe.
//
// A provisioning system that confirms before the dispatch was recorded is not misbehaving — it is
// faster than the round trip. Refusing it would leave a Tenant in `requested` with a realized
// boundary and no path to activation, which is the failure that looks exactly like slow provisioning.
func TestARealizedStatusAdvancesATenantStillAwaitingItsDispatch(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	requested := f.request(t, ctx, sponsor)

	resolution, err := coordinator.Realize(ctx, outcome(correlation, "boundary built"))
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}
	if resolution.Tenant.Status != StateProvisioning {
		t.Fatalf("a realized status left the Tenant %s, want %s",
			resolution.Tenant.Status, StateProvisioning)
	}

	if _, err := f.service.Activate(ctx, command(requested.Tenant.TenantID,
		resolution.Tenant.Version)); err != nil {
		t.Fatalf("Activate after an early realized status: %v", err)
	}
}

// TestAFailureRecordsTheOutcomeAndMovesTheTenantToFailed covers the ordinary refusal.
func TestAFailureRecordsTheOutcomeAndMovesTheTenantToFailed(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	requested := f.request(t, ctx, sponsor)
	tenantID := requested.Tenant.TenantID

	if _, err := coordinator.Provision(ctx, command(tenantID, 1)); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	resolution, err := coordinator.Fail(ctx, outcome(correlation, "no capacity in ap-southeast-3"))
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if resolution.State != RequestFailed {
		t.Errorf("Fail recorded %s, want %s", resolution.State, RequestFailed)
	}
	if resolution.Tenant.Status != StateFailed {
		t.Errorf("Fail left the Tenant %s, want %s", resolution.Tenant.Status, StateFailed)
	}

	rows := f.requestRows(t, tenantID)
	if len(rows) != 1 || rows[0].state != RequestFailed {
		t.Fatalf("the stored requests are %+v, want one failed", rows)
	}
	if rows[0].detail != "no capacity in ap-southeast-3" {
		t.Errorf("the stored detail is %q, and a refusal that says only \"failed\" leaves the retry "+
			"decision with nothing to go on", rows[0].detail)
	}
	if rows[0].resolvedAt == nil {
		t.Error("a resolved request carries no resolution instant")
	}

	// A failure requires a detail and a realized status does not: the first is the one an operator
	// has to act on.
	if _, err := coordinator.Fail(ctx, outcome(correlation, "   ")); !errors.Is(err, ErrInvalid) {
		t.Errorf("a failure with no detail returned %v, want ErrInvalid", err)
	}
}

// TestAFailureAgainstATenantStillInRequestedWalksTheDeclaredPath is the edge the machine does not
// have.
//
// There is no `requested -> failed` transition and this does not add one. The refusal traverses
// `provision` then `fail`, which is safe precisely because both are silent and neither increments the
// security version. Adding the missing edge would have been the smaller diff and the larger change:
// the machine is asserted as a whole table, and an edge added for one caller is inherited by all.
func TestAFailureAgainstATenantStillInRequestedWalksTheDeclaredPath(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	requested := f.request(t, ctx, sponsor)
	tenantID := requested.Tenant.TenantID

	resolution, err := coordinator.Fail(ctx, outcome(correlation, "refused before dispatch landed"))
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if resolution.Tenant.Status != StateFailed {
		t.Fatalf("Fail left the Tenant %s, want %s", resolution.Tenant.Status, StateFailed)
	}

	// Two transitions, so two version increments on top of the intake's 1.
	if resolution.Tenant.Version != 3 {
		t.Errorf("the version is %d after two transitions from 1, want 3", resolution.Tenant.Version)
	}

	stored := f.read(t, tenantID)
	if stored.securityVersion != 1 {
		t.Errorf("walking the declared path moved the security version to %d, and no context exists "+
			"to invalidate inside a Tenant that has never been active", stored.securityVersion)
	}

	all := f.events(t, tenantID)
	if len(all) != 1 {
		t.Errorf("the path published %d events, want only the desired-state publication", len(all))
	}
}

// TestAResolvedOutcomeCannotBeContradicted keeps the evidence of a twice-provisioned Tenant readable.
//
// An attempt has one result. A retry is a new attempt with its own row, and overwriting the first
// outcome would erase the fact that there had been two.
func TestAResolvedOutcomeCannotBeContradicted(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	f.request(t, ctx, sponsor)

	if _, err := coordinator.Realize(ctx, outcome(correlation, "boundary built")); err != nil {
		t.Fatalf("Realize: %v", err)
	}
	if _, err := coordinator.Fail(ctx, outcome(correlation, "actually it was not")); !errors.Is(
		err, ErrOutcomeAlreadyRecorded) {
		t.Fatalf("contradicting a realized outcome returned %v, want ErrOutcomeAlreadyRecorded", err)
	}
}

// TestOneCorrelationAcrossTwoTenantsIsRefused fails closed on an ambiguous match.
//
// A correlation identifier is per request, so two Tenants sharing one means both were created by a
// single call. Resolving the newest would silently mark the wrong Tenant's boundary as built, and
// activation reads exactly that statement.
func TestOneCorrelationAcrossTwoTenantsIsRefused(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	f.request(t, ctx, sponsor)
	f.request(t, ctx, sponsor)

	if _, err := coordinator.Realize(ctx, outcome(correlation, "boundary built")); !errors.Is(
		err, ErrAmbiguousCorrelation) {
		t.Fatalf("an ambiguous correlation returned %v, want ErrAmbiguousCorrelation", err)
	}
}

func TestAnUnknownCorrelationIsNotFound(t *testing.T) {
	f := newFixture(t)
	ctx, _ := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	if _, err := coordinator.Realize(ctx, outcome(mustID(t), "boundary built")); !errors.Is(
		err, ErrNoProvisioningRequest) {
		t.Fatalf("an unmatched correlation returned %v, want ErrNoProvisioningRequest", err)
	}
}

// TestARetryFromFailedRecordsANewAttempt covers the `failed -> provisioning` edge.
//
// `failed` is not terminal because the cause is usually external and usually fixable. The retry
// writes a new request row rather than reopening the old one, and it copies the desired profile from
// the Tenant row — taking it from the caller would let a retry quietly change what is being built.
func TestARetryFromFailedRecordsANewAttempt(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	requested := f.request(t, ctx, sponsor)
	tenantID := requested.Tenant.TenantID

	failed, err := coordinator.Fail(ctx, outcome(correlation, "no capacity"))
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}

	retryCtx, retryCorrelation := f.withCorrelation(t)
	retried, err := coordinator.Provision(retryCtx, command(tenantID, failed.Tenant.Version))
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if retried.Tenant.Status != StateProvisioning {
		t.Fatalf("the retry left the Tenant %s, want %s", retried.Tenant.Status, StateProvisioning)
	}

	rows := f.requestRows(t, tenantID)
	if len(rows) != 2 {
		t.Fatalf("the retry left %d provisioning requests, want 2", len(rows))
	}
	if rows[0].state != RequestRequested {
		t.Errorf("the newest request is %s, want %s", rows[0].state, RequestRequested)
	}
	if rows[1].state != RequestFailed {
		t.Errorf("the previous outcome is %s, want it preserved as %s", rows[1].state, RequestFailed)
	}
	if rows[0].profile["isolation_profile"] != string(ProfileSilo) {
		t.Errorf("the retry recorded profile %v, want the Tenant's own %s",
			rows[0].profile["isolation_profile"], ProfileSilo)
	}

	// The new attempt is resolvable on its own correlation identifier, and the old one is spent.
	if _, err := coordinator.Realize(retryCtx, outcome(retryCorrelation, "boundary built")); err != nil {
		t.Fatalf("Realize against the retry: %v", err)
	}
}

// TestProvisionRefusesWhenNothingIsOutstanding stops an operator asserting that a boundary is being
// built when nothing was asked for.
//
// Without the check, activation would then wait on a realized status for a command that was never
// sent — and the Tenant would look like it was provisioning.
func TestProvisionRefusesWhenNothingIsOutstanding(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, correlation := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	requested := f.request(t, ctx, sponsor)

	// Spend the intake's request without moving the Tenant out of `requested`.
	f.exec(t, `UPDATE tenant.provisioning_request SET state = 'realized', resolved_at = now()
	WHERE correlation_id = $1`, correlation.String())

	_, err := coordinator.Provision(ctx, command(requested.Tenant.TenantID, 1))
	if !errors.Is(err, ErrProvisioningNotRequested) {
		t.Fatalf("Provision with nothing outstanding returned %v, want ErrProvisioningNotRequested", err)
	}
}

// TestATimeoutWithNoStatusProducesUnresolvedNotFailed is the SAD-004 §7.5 requirement.
//
// An ambiguous outcome must remain pending or failed and must never be inferred as success — and it
// must not be inferred as failure either. The target may have built the boundary or may not, and
// treating a timeout as a refusal and retrying is how a Tenant gets provisioned twice.
func TestATimeoutWithNoStatusProducesUnresolvedNotFailed(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, _ := f.withCorrelation(t)

	requested := f.request(t, ctx, sponsor)
	tenantID := requested.Tenant.TenantID

	// An hour past the accepted instant, with a thirty-minute timeout.
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed.Add(time.Hour))

	affected, err := coordinator.SweepUnresolved(ctx, 100)
	if err != nil {
		t.Fatalf("SweepUnresolved: %v", err)
	}
	if affected < 1 {
		t.Fatalf("the sweep aged %d requests, want at least this Tenant's", affected)
	}

	rows := f.requestRows(t, tenantID)
	if len(rows) != 1 {
		t.Fatalf("the sweep left %d provisioning requests, want 1 — it must not retry", len(rows))
	}
	if rows[0].state != RequestUnresolved {
		t.Errorf("the sweep recorded %s, want %s", rows[0].state, RequestUnresolved)
	}
	if rows[0].detail == "" {
		t.Error("an unresolved request carries no detail for the operator who has to resolve it")
	}

	// The Tenant does not move. `unresolved` is a statement about the command's outcome, not about
	// the Tenant's lifecycle, and advancing it would be inferring one from the other.
	stored := f.read(t, tenantID)
	if stored.status != StateRequested {
		t.Errorf("the sweep moved the Tenant to %s, want it left in %s", stored.status, StateRequested)
	}

	// A realized status arriving after the timeout still resolves by correlation, which is the case
	// the state exists for. `unresolved` means the outcome is unknown, not that the request is over.
	late, err := coordinator.Realize(ctx, outcome(requested.CorrelationID, "boundary built, late"))
	if err != nil {
		t.Fatalf("a late realized status: %v", err)
	}
	if late.State != RequestRealized {
		t.Errorf("the late status recorded %s, want %s", late.State, RequestRealized)
	}
	if late.Tenant.Status != StateProvisioning {
		t.Errorf("the late status left the Tenant %s, want %s", late.Tenant.Status, StateProvisioning)
	}
}

// TestTheSweepIsBatchedAndRefusesANonsenseSize keeps an unbounded UPDATE off a table that grows with
// the estate.
func TestTheSweepIsBatchedAndRefusesANonsenseSize(t *testing.T) {
	f := newFixture(t)
	ctx, _ := f.withCorrelation(t)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	if _, err := coordinator.SweepUnresolved(ctx, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a zero batch size returned %v, want ErrInvalid", err)
	}
}

// TestTheSweepAlsoAgesADeprovisioning makes an unreachable gate reachable.
//
// `internal/offboarding` refuses retirement on an `unresolved` deprovisioning — SAD-004 §7.5 again,
// because a timeout is not proof the target did nothing and retiring on it would release the only
// record of infrastructure that may still exist. Nothing produced that state before this sweep, so
// the gate was correct and unreachable.
func TestTheSweepAlsoAgesADeprovisioning(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateOffboarding, "active")
	ctx, _ := f.withCorrelation(t)

	f.exec(t, `INSERT INTO tenant.provisioning_request
	    (request_id, tenant_id, desired_profile, state, correlation_id, requested_at)
	VALUES ($1, $2, jsonb_build_object('operation', 'deprovision'), 'requested', $3, $4)`,
		mustID(t).String(), seeded.TenantID.String(), mustID(t).String(), f.fixed)

	coordinator := f.coordinator(t, 30*time.Minute, f.fixed.Add(time.Hour))
	if _, err := coordinator.SweepUnresolved(ctx, 100); err != nil {
		t.Fatalf("SweepUnresolved: %v", err)
	}

	rows := f.requestRows(t, seeded.TenantID)
	if len(rows) != 1 || rows[0].state != RequestUnresolved {
		t.Fatalf("the deprovisioning rows are %+v, want one unresolved", rows)
	}
}

// TestTheCoordinatorRefusesToBeBuiltWithoutATimeout keeps the sweep cadence a decision.
//
// The timeout decides when this service declares an outcome unknown. A coordinator that defaulted it
// would sweep on a schedule no operator chose, and the value would be invisible in the deployment.
func TestTheCoordinatorRefusesToBeBuiltWithoutATimeout(t *testing.T) {
	f := newFixture(t)

	for _, timeout := range []time.Duration{0, -time.Minute} {
		if _, err := NewCoordinator(f.service.pool, f.service, timeout); err == nil {
			t.Errorf("NewCoordinator accepted a timeout of %s", timeout)
		}
	}
	if _, err := NewCoordinator(nil, f.service, time.Minute); err == nil {
		t.Error("NewCoordinator accepted a nil pool")
	}
	if _, err := NewCoordinator(f.service.pool, nil, time.Minute); err == nil {
		t.Error("NewCoordinator accepted a nil tenant service")
	}
}

// TestATenantScopeCannotDriveProvisioning is the isolation posture, asserted on the new surface.
//
// Every method here binds the provider pool. A tenant-scoped caller reaching one would be
// cross-Tenant administration under a role that holds no privilege for it.
func TestATenantScopeCannotDriveProvisioning(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed)

	scope, err := db.TenantScope(mustID(t), f.actor, mustID(t))
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}
	tenantCtx := db.WithScope(f.ctx, scope)

	if _, err := f.service.Request(tenantCtx, RequestTenant{
		OrganizationID:   sponsor,
		DisplayName:      "provisioning-suite subject",
		IsolationProfile: ProfilePooled,
		Reason:           "asserted by the provisioning suite",
	}); !errors.Is(err, db.ErrWrongScope) {
		t.Errorf("Request under a tenant scope returned %v, want db.ErrWrongScope", err)
	}
	if _, err := coordinator.Realize(tenantCtx, outcome(mustID(t), "built")); !errors.Is(
		err, db.ErrWrongScope) {
		t.Errorf("Realize under a tenant scope returned %v, want db.ErrWrongScope", err)
	}
	if _, err := coordinator.SweepUnresolved(tenantCtx, 10); !errors.Is(err, db.ErrWrongScope) {
		t.Errorf("SweepUnresolved under a tenant scope returned %v, want db.ErrWrongScope", err)
	}
}
