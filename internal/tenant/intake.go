package tenant

// Tenant intake: the one command that creates a Tenant, and the read behind `GET /v1/tenants/{id}`.
//
// Creation is not a transition and is deliberately not in the state machine. The machine's initial
// edge is `[*] --> requested`, which has no source state for `Resolve` to check and no version for a
// caller to have been shown — so modelling it as an Action would have added a row whose `from` set is
// empty and whose refusal rules are meaningless. It lives here instead, and every subsequent move is
// the machine's.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/system"
)

// DefaultDisplayNameMax is the `ORGANIZATION_TENANT_NAME_MAX` default from
// TDD-organization-control-003 §Configuration.
const DefaultDisplayNameMax = 120

// IsolationProfile records which of the EAD-005 §5.3 multi-tenant deployment profiles applies.
//
// It is a reference for provisioning and a fact for audit. This service does not implement
// isolation — SAD-004 §5.1 puts that in the runtime and data systems — and recording the chosen
// profile is the whole of its involvement.
type IsolationProfile string

const (
	// ProfilePooled shares infrastructure between Tenants, separated by Row-Level Security.
	ProfilePooled IsolationProfile = "pooled"

	// ProfileBridge shares infrastructure and separates schemas or databases.
	ProfileBridge IsolationProfile = "bridge"

	// ProfileSilo gives the Tenant its own infrastructure.
	ProfileSilo IsolationProfile = "silo"

	// ProfileRegional pins the Tenant to a residency region.
	ProfileRegional IsolationProfile = "regional"
)

// Valid reports whether the profile is one this service persists. The set mirrors
// `tenant_isolation_check` in schema.hcl; a value passing here and failing there would surface as a
// constraint violation rather than as a refused request.
func (p IsolationProfile) Valid() bool {
	switch p {
	case ProfilePooled, ProfileBridge, ProfileSilo, ProfileRegional:
		return true
	}
	return false
}

// requestedEventType is the desired-state publication.
//
// TDD-organization-control-003 §"Provisioning Correlation" ends tenant creation with "publish
// desired state", and §"Published Events" names exactly one event at creation. They are the same
// event: this is how the external system that owns the isolation boundary learns what to build, so
// the payload carries the desired profile and the correlation identifier the realized status comes
// back on. A separate internal channel would have given the estate two records of one intention.
//
// The three provisioning transitions that follow are silent, per `silentActions`.
const requestedEventType = "com.scnehaux.organization.tenant.lifecycle.requested"

// Record is one whole row of tenant.tenant.
//
// Separate from `Tenant`, which is the same row narrowed to what a transition reads and writes. Two
// types rather than one with half its fields unset on the transition path: `NewPayload` is built
// from `Tenant`, and widening it would have every lifecycle event carry an empty display name
// whenever the loader that fed it did not select one.
type Record struct {
	TenantID         id.UUID
	OrganizationID   id.UUID
	DisplayName      string
	Status           State
	IsolationProfile IsolationProfile

	// ResidencyRegion is empty when the profile pins the Tenant to no region.
	ResidencyRegion string

	Version         int64
	SecurityVersion int64

	// The four lifecycle stamps record the current position rather than a history, and each is nil
	// until its transition sets it. `SuspendedAt` returns to nil on restore.
	ActivatedAt          *time.Time
	SuspendedAt          *time.Time
	OffboardingStartedAt *time.Time
	RetiredAt            *time.Time

	CreatedAt time.Time
}

// Narrow returns the transition view of the record, so a caller holding one does not have to
// reassemble the other by hand.
func (r Record) Narrow() Tenant {
	return Tenant{
		TenantID:        r.TenantID,
		OrganizationID:  r.OrganizationID,
		Status:          r.Status,
		Version:         r.Version,
		SecurityVersion: r.SecurityVersion,
	}
}

// RequestTenant is a request for a new Tenant.
//
// There is no expected version, unlike every transition command: nothing exists yet for the caller
// to have been shown a version of.
type RequestTenant struct {
	OrganizationID   id.UUID
	DisplayName      string
	IsolationProfile IsolationProfile

	// ResidencyRegion is optional. It is a fact for provisioning, not a constraint this service
	// enforces, so no value is validated against a region list the estate does not publish here.
	ResidencyRegion string

	// Reason is required for the same purpose as on a transition: every Tenant command is provider
	// authority, and `db.WithProviderScope` refuses a blank one.
	Reason string
}

func (r RequestTenant) validate(displayNameMax int) error {
	name := strings.TrimSpace(r.DisplayName)
	switch {
	case r.OrganizationID.IsNil():
		return fmt.Errorf("%w: a sponsoring organization identifier is required", ErrInvalid)
	case name == "":
		return fmt.Errorf("%w: a display name is required", ErrInvalid)
	case len(name) > displayNameMax:
		return fmt.Errorf("%w: the display name is %d characters and the bound is %d",
			ErrInvalid, len(name), displayNameMax)
	case !r.IsolationProfile.Valid():
		return fmt.Errorf("%w: isolation profile %q is not one of pooled, bridge, silo, regional",
			ErrInvalid, r.IsolationProfile)
	case strings.TrimSpace(r.Reason) == "":
		return fmt.Errorf("%w: a reason is required", ErrInvalid)
	}
	return nil
}

// Requested is what a committed intake reports back.
type Requested struct {
	Tenant Record

	// RequestID and CorrelationID are the handles the realized status comes back on.
	//
	// Both are returned rather than left for the caller to find, because the correlation identifier
	// is what `Coordinator.Realize` and `.Fail` match on, and a caller that cannot see it has no way
	// to complete the flow it just started.
	RequestID     id.UUID
	CorrelationID id.UUID

	AcceptedAt time.Time
}

// RequestedPayload is the body of the desired-state publication.
//
// It embeds the standard `Payload` rather than replacing it, so a consumer that projects Tenants
// reads the same five fields here as on every other Tenant event and the additions are additive.
// The extra fields are the desired profile — what the provisioning system needs in order to act —
// and the two identifiers it reports back on.
type RequestedPayload struct {
	Payload

	DisplayName      string           `json:"display_name"`
	IsolationProfile IsolationProfile `json:"isolation_profile"`
	ResidencyRegion  string           `json:"residency_region,omitempty"`

	ProvisioningRequestID id.UUID `json:"provisioning_request_id"`
	CorrelationID         id.UUID `json:"correlation_id"`
}

const insertTenant = `INSERT INTO tenant.tenant
    (tenant_id, organization_id, display_name, status, isolation_profile, residency_region)
VALUES ($1, $2, $3, 'requested', $4, $5)
RETURNING version, tenant_security_version, created_at`

// insertProvisioning records the desired state sent outward, so the realized status can be
// correlated back to it.
//
// `operation` is written explicitly even though `provision` is what a missing value reads as.
// `internal/offboarding` writes `deprovision` into the same table, and the activation predicate
// filters on this field — a row that relied on the default would be correct today and wrong the
// first time somebody added a third direction.
const insertProvisioning = `INSERT INTO tenant.provisioning_request
    (request_id, tenant_id, desired_profile, state, correlation_id, requested_at)
VALUES ($1, $2, jsonb_strip_nulls(jsonb_build_object(
            'operation', 'provision',
            'isolation_profile', $3::text,
            'residency_region', $4::text)),
        'requested', $5, $6)`

// Request creates a Tenant in `requested`, records the desired provisioning state, and publishes it.
//
// All three in one transaction, per TDD-organization-control-003 §"Provisioning Correlation". A
// Tenant persisted without its provisioning request would be a Tenant nothing was ever asked to
// build, and one whose request was recorded without the publication would wait for a realized status
// no system was told to produce. Neither failure announces itself: both look like slow provisioning.
//
// The Tenant is created in `requested` and not `active`. SAD-004 §5.1 makes activation wait for
// provisioning to confirm, because a Membership granted into a Tenant whose isolation boundary does
// not exist yet is access to nothing — or, worse, access to somebody else's boundary.
func (s *Service) Request(ctx context.Context, req RequestTenant) (Requested, error) {
	if err := req.validate(s.displayNameMax); err != nil {
		return Requested{}, err
	}

	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Requested{}, fmt.Errorf("tenant: no scope is bound to this context")
	}

	tenantID, err := s.newID()
	if err != nil {
		return Requested{}, fmt.Errorf("tenant: mint identifier: %w", err)
	}
	requestID, err := s.newID()
	if err != nil {
		return Requested{}, fmt.Errorf("tenant: mint provisioning-request identifier: %w", err)
	}

	acceptedAt := s.now().UTC()
	record := Record{
		TenantID:         tenantID,
		OrganizationID:   req.OrganizationID,
		DisplayName:      strings.TrimSpace(req.DisplayName),
		Status:           StateRequested,
		IsolationProfile: req.IsolationProfile,
		ResidencyRegion:  strings.TrimSpace(req.ResidencyRegion),
	}

	if err := db.WithProviderScope(ctx, s.pool, req.Reason,
		func(ctx context.Context, tx db.Tx) error {
			// The sponsor must exist and must not be retired. It need not be active: a Tenant may
			// legitimately be requested while its Organization is still being onboarded, which is
			// why the active check is at activation and not here. Retired is different — an
			// Organization retires only once no live Tenant remains under it, so creating one
			// beneath a retired sponsor would falsify the refusal that let it retire.
			var sponsorStatus string
			if err := tx.QueryRow(ctx, sponsorStatement, req.OrganizationID.String()).
				Scan(&sponsorStatus); err != nil {
				return fmt.Errorf("%w: sponsoring organization %s", ErrNotFound, req.OrganizationID)
			}
			if sponsorStatus == "retired" {
				return fmt.Errorf("%w: sponsoring organization %s is retired",
					ErrTransitionRefused, req.OrganizationID)
			}

			if err := tx.QueryRow(ctx, insertTenant,
				record.TenantID.String(), record.OrganizationID.String(), record.DisplayName,
				string(record.IsolationProfile), nullableText(record.ResidencyRegion),
			).Scan(&record.Version, &record.SecurityVersion, &record.CreatedAt); err != nil {
				return fmt.Errorf("tenant: insert: %w", err)
			}

			if _, err := tx.Exec(ctx, insertProvisioning,
				requestID.String(), record.TenantID.String(),
				string(record.IsolationProfile), nullableText(record.ResidencyRegion),
				scope.Correlation().String(), acceptedAt); err != nil {
				return fmt.Errorf("tenant: record desired provisioning state: %w", err)
			}

			return s.publishRequested(ctx, tx, record, requestID, scope.Correlation(), acceptedAt)
		}); err != nil {
		return Requested{}, err
	}

	return Requested{
		Tenant:        record,
		RequestID:     requestID,
		CorrelationID: scope.Correlation(),
		AcceptedAt:    acceptedAt,
	}, nil
}

func (s *Service) publishRequested(ctx context.Context, tx db.Tx, record Record,
	requestID, correlationID id.UUID, at time.Time) error {
	if s.beforeAppend != nil {
		if err := s.beforeAppend(ctx); err != nil {
			return err
		}
	}

	eventType, err := event.ParseType(requestedEventType)
	if err != nil {
		return fmt.Errorf("tenant: event type %q: %w", requestedEventType, err)
	}
	envelope, err := event.New(system.Source, eventType, at, RequestedPayload{
		Payload:               NewPayload(record.Narrow()),
		DisplayName:           record.DisplayName,
		IsolationProfile:      record.IsolationProfile,
		ResidencyRegion:       record.ResidencyRegion,
		ProvisioningRequestID: requestID,
		CorrelationID:         correlationID,
	})
	if err != nil {
		return fmt.Errorf("tenant: build envelope: %w", err)
	}

	// The standard lane. Creation stops no access, and putting it on the reserved lane would let a
	// bulk onboarding delay a live revocation — which is the one thing the reserved lane exists for.
	if err := outbox.Append(ctx, tx, record.TenantID, envelope); err != nil {
		return fmt.Errorf("tenant: append event: %w", err)
	}
	return nil
}

const selectRecord = `SELECT tenant_id::text,
       organization_id::text,
       display_name,
       status,
       isolation_profile,
       coalesce(residency_region, ''),
       version,
       tenant_security_version,
       activated_at,
       suspended_at,
       offboarding_started_at,
       retired_at,
       created_at
FROM tenant.tenant
WHERE tenant_id = $1`

// Get reads one whole Tenant.
//
// Provider-scoped like every other method here, and for the same forced reason: `organization_rt`
// cannot read across Tenants, and a Tenant record is read by an operator who is not inside it.
func (s *Service) Get(ctx context.Context, tenantID id.UUID) (Record, error) {
	if tenantID.IsNil() {
		return Record{}, fmt.Errorf("%w: a tenant identifier is required", ErrInvalid)
	}

	var record Record
	if err := db.WithProviderScope(ctx, s.pool, "read tenant "+tenantID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var err error
			record, err = loadRecord(ctx, tx, tenantID)
			return err
		}); err != nil {
		return Record{}, err
	}
	return record, nil
}

func loadRecord(ctx context.Context, tx db.Tx, tenantID id.UUID) (Record, error) {
	var (
		record       Record
		rawTenant    string
		rawSponsor   string
		status       string
		profile      string
		residencyRaw string
	)
	if err := tx.QueryRow(ctx, selectRecord, tenantID.String()).Scan(
		&rawTenant, &rawSponsor, &record.DisplayName, &status, &profile, &residencyRaw,
		&record.Version, &record.SecurityVersion,
		&record.ActivatedAt, &record.SuspendedAt, &record.OffboardingStartedAt, &record.RetiredAt,
		&record.CreatedAt); err != nil {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, tenantID)
	}

	parsed, err := id.Parse(rawTenant)
	if err != nil {
		return Record{}, fmt.Errorf("tenant: stored identifier %q is unparseable: %w", rawTenant, err)
	}
	sponsor, err := id.Parse(rawSponsor)
	if err != nil {
		return Record{}, fmt.Errorf("tenant: stored organization %q is unparseable: %w", rawSponsor, err)
	}
	record.TenantID, record.OrganizationID = parsed, sponsor
	record.ResidencyRegion = residencyRaw

	// Both stored enumerations are checked rather than trusted. A value outside either set is this
	// service's own defect — the CHECK constraints make it unreachable through the API — so it
	// carries no ErrInvalid and surfaces as a 500, which is the honest answer for a row that should
	// not exist.
	record.Status = State(status)
	if !record.Status.Valid() {
		return Record{}, fmt.Errorf("tenant: stored status %q is not in the state machine", status)
	}
	record.IsolationProfile = IsolationProfile(profile)
	if !record.IsolationProfile.Valid() {
		return Record{}, fmt.Errorf("tenant: stored isolation profile %q is not declared", profile)
	}
	return record, nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
