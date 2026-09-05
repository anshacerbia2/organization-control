package httpapi

import (
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

// The views below are the wire contract, and they exist rather than marshalling the domain types
// directly for two reasons.
//
// The first is that the wire contract belongs to this layer. Most domain structs here carry no JSON
// tags, so marshalling them would publish Go field names and would republish them, silently, the
// day somebody renames a field for domain reasons.
//
// The second is disclosure. `invitation.Invitation` carries `TargetIdentifier`, which STD-GLB-007
// makes Tier-2 PII, and `TargetHash`, which is a hash of it. Marshalling the struct would put both
// on every response that mentions an invitation, including ones read by an operator who has no
// business knowing who was invited. A view names each field that leaves, so adding a field to the
// domain type cannot widen a response by itself.

type membershipView struct {
	MembershipID id.UUID   `json:"membership_id"`
	PrincipalID  id.UUID   `json:"principal_id"`
	TenantID     id.UUID   `json:"tenant_id"`
	WorkspaceID  id.UUID   `json:"workspace_id"`
	SubjectType  string    `json:"subject_type"`
	Status       string    `json:"status"`
	Version      int64     `json:"version"`
	ValidFrom    time.Time `json:"valid_from"`
	ValidUntil   time.Time `json:"valid_until"`
	Provenance   string    `json:"provenance"`
}

type membershipResultView struct {
	Membership            membershipView `json:"membership"`
	AcceptedAt            time.Time      `json:"accepted_at"`
	TenantSecurityVersion int64          `json:"tenant_security_version"`
}

func viewMembership(m membership.Membership) membershipView {
	return membershipView{
		MembershipID: m.MembershipID, PrincipalID: m.PrincipalID, TenantID: m.TenantID,
		WorkspaceID: m.WorkspaceID, SubjectType: m.SubjectType, Status: string(m.Status),
		Version: m.Version, ValidFrom: m.ValidFrom, ValidUntil: m.ValidUntil,
		Provenance: m.Provenance,
	}
}

func viewMembershipResult(r membership.Result) membershipResultView {
	return membershipResultView{
		Membership: viewMembership(r.Membership), AcceptedAt: r.AcceptedAt,
		TenantSecurityVersion: r.TenantSecurityVersion,
	}
}

type tenantView struct {
	TenantID        id.UUID `json:"tenant_id"`
	OrganizationID  id.UUID `json:"organization_id"`
	Status          string  `json:"status"`
	Version         int64   `json:"version"`
	SecurityVersion int64   `json:"security_version"`
}

type tenantResultView struct {
	Tenant     tenantView `json:"tenant"`
	AcceptedAt time.Time  `json:"accepted_at"`

	// Published reports whether the lifecycle event was written in the same transaction. It is on
	// the response because a caller that saw 200 and no event has no other way to learn that, and
	// the answer changes what a downstream reconciliation should expect.
	Published bool `json:"published"`
}

// tenantRecordView is the whole Tenant, as `GET /v1/tenants/{tenant_id}` answers it.
//
// The four lifecycle stamps are omitted when nil rather than sent as null, so a reader can tell "has
// never been suspended" from "is suspended" without comparing a timestamp against zero.
type tenantRecordView struct {
	TenantID         id.UUID `json:"tenant_id"`
	OrganizationID   id.UUID `json:"organization_id"`
	DisplayName      string  `json:"display_name"`
	Status           string  `json:"status"`
	IsolationProfile string  `json:"isolation_profile"`
	ResidencyRegion  string  `json:"residency_region,omitempty"`

	Version         int64 `json:"version"`
	SecurityVersion int64 `json:"security_version"`

	ActivatedAt          *time.Time `json:"activated_at,omitempty"`
	SuspendedAt          *time.Time `json:"suspended_at,omitempty"`
	OffboardingStartedAt *time.Time `json:"offboarding_started_at,omitempty"`
	RetiredAt            *time.Time `json:"retired_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func viewTenantRecord(t tenant.Record) tenantRecordView {
	return tenantRecordView{
		TenantID: t.TenantID, OrganizationID: t.OrganizationID,
		DisplayName: t.DisplayName, Status: string(t.Status),
		IsolationProfile: string(t.IsolationProfile), ResidencyRegion: t.ResidencyRegion,
		Version: t.Version, SecurityVersion: t.SecurityVersion,
		ActivatedAt: t.ActivatedAt, SuspendedAt: t.SuspendedAt,
		OffboardingStartedAt: t.OffboardingStartedAt, RetiredAt: t.RetiredAt,
		CreatedAt: t.CreatedAt,
	}
}

// requestedView answers `POST /v1/tenants`.
//
// It carries the correlation identifier back to the caller. That is not decoration: it is what
// `POST /v1/provisioning/realized` matches on, so a caller that could not see it would have started
// a flow it had no way to finish.
type requestedView struct {
	Tenant tenantRecordView `json:"tenant"`

	ProvisioningRequestID id.UUID `json:"provisioning_request_id"`
	CorrelationID         id.UUID `json:"correlation_id"`

	AcceptedAt time.Time `json:"accepted_at"`
}

func viewRequested(r tenant.Requested) requestedView {
	return requestedView{
		Tenant:                viewTenantRecord(r.Tenant),
		ProvisioningRequestID: r.RequestID,
		CorrelationID:         r.CorrelationID,
		AcceptedAt:            r.AcceptedAt,
	}
}

// resolutionView answers the two outcome routes.
type resolutionView struct {
	ProvisioningRequestID id.UUID    `json:"provisioning_request_id"`
	TenantID              id.UUID    `json:"tenant_id"`
	State                 string     `json:"state"`
	Tenant                tenantView `json:"tenant"`

	// Replay says the outcome was already recorded and this call changed nothing. On the response
	// because a provisioning system retrying a delivery has no other way to learn that its first
	// attempt did arrive.
	Replay bool `json:"replay"`

	ResolvedAt time.Time `json:"resolved_at"`
}

func viewResolution(r tenant.Resolution) resolutionView {
	return resolutionView{
		ProvisioningRequestID: r.RequestID,
		TenantID:              r.TenantID,
		State:                 string(r.State),
		Tenant: tenantView{
			TenantID: r.Tenant.TenantID, OrganizationID: r.Tenant.OrganizationID,
			Status: string(r.Tenant.Status), Version: r.Tenant.Version,
			SecurityVersion: r.Tenant.SecurityVersion,
		},
		Replay:     r.Replay,
		ResolvedAt: r.ResolvedAt,
	}
}

func viewTenantResult(r tenant.Result) tenantResultView {
	return tenantResultView{
		Tenant: tenantView{
			TenantID: r.Tenant.TenantID, OrganizationID: r.Tenant.OrganizationID,
			Status: string(r.Tenant.Status), Version: r.Tenant.Version,
			SecurityVersion: r.Tenant.SecurityVersion,
		},
		AcceptedAt: r.AcceptedAt, Published: r.Published,
	}
}

type organizationView struct {
	OrganizationID id.UUID   `json:"organization_id"`
	DisplayName    string    `json:"display_name"`
	Classification string    `json:"classification"`
	Status         string    `json:"status"`
	ParentID       *id.UUID  `json:"parent_id,omitempty"`
	Version        int64     `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
}

func viewOrganization(o organization.Organization) organizationView {
	return organizationView{
		OrganizationID: o.OrganizationID, DisplayName: o.DisplayName,
		Classification: string(o.Classification), Status: string(o.Status), ParentID: o.ParentID,
		Version: o.Version, CreatedAt: o.CreatedAt,
	}
}

type workspaceView struct {
	WorkspaceID id.UUID   `json:"workspace_id"`
	TenantID    id.UUID   `json:"tenant_id"`
	DisplayName string    `json:"display_name"`
	Type        string    `json:"type"`
	Status      string    `json:"status"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
}

func viewWorkspace(w workspace.Workspace) workspaceView {
	return workspaceView{
		WorkspaceID: w.WorkspaceID, TenantID: w.TenantID, DisplayName: w.DisplayName,
		Type: w.Type, Status: string(w.Status), Version: w.Version, CreatedAt: w.CreatedAt,
	}
}

// invitationView omits TargetIdentifier and TargetHash.
//
// The identifier is Tier-2 PII and the hash is a hash of it, which is not the same as anonymous: an
// attacker holding a list of candidate identifiers can confirm any of them against a published
// hash. A caller that needs to know who an invitation is addressed to already knows, because they
// addressed it.
type invitationView struct {
	InvitationID  id.UUID    `json:"invitation_id"`
	TenantID      id.UUID    `json:"tenant_id"`
	WorkspaceID   *id.UUID   `json:"workspace_id,omitempty"`
	SubjectType   string     `json:"subject_type"`
	InvitedBy     id.UUID    `json:"invited_by"`
	Reason        string     `json:"reason,omitempty"`
	State         string     `json:"state"`
	CorrelationID id.UUID    `json:"correlation_id"`
	PrincipalID   *id.UUID   `json:"principal_id,omitempty"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CreatedAt     time.Time  `json:"created_at"`
	AcceptedAt    *time.Time `json:"accepted_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

func viewInvitation(i invitation.Invitation) invitationView {
	return invitationView{
		InvitationID: i.InvitationID, TenantID: i.TenantID, WorkspaceID: i.WorkspaceID,
		SubjectType: i.SubjectType, InvitedBy: i.InvitedBy, Reason: i.Reason,
		State: string(i.State), CorrelationID: i.CorrelationID, PrincipalID: i.PrincipalID,
		ExpiresAt: i.ExpiresAt, CreatedAt: i.CreatedAt, AcceptedAt: i.AcceptedAt,
		RevokedAt: i.RevokedAt,
	}
}

// issuedView is the one response that carries a token, and the only one.
//
// The token is returned here because this is the moment it exists and it is never stored — only its
// hash is. A caller that loses it must revoke and reissue.
type issuedView struct {
	Invitation invitationView `json:"invitation"`
	Token      string         `json:"token"`
}

type offboardingView struct {
	OffboardingID id.UUID    `json:"offboarding_id"`
	TenantID      id.UUID    `json:"tenant_id"`
	Stage         string     `json:"stage"`
	InitiatedBy   id.UUID    `json:"initiated_by"`
	Reason        string     `json:"reason,omitempty"`
	LegalHold     bool       `json:"legal_hold"`
	CorrelationID id.UUID    `json:"correlation_id"`
	StartedAt     time.Time  `json:"started_at"`
	FrozenAt      *time.Time `json:"frozen_at,omitempty"`
	RetiredAt     *time.Time `json:"retired_at,omitempty"`
}

func viewOffboarding(o offboarding.Offboarding) offboardingView {
	return offboardingView{
		OffboardingID: o.OffboardingID, TenantID: o.TenantID, Stage: string(o.Stage),
		InitiatedBy: o.InitiatedBy, Reason: o.Reason, LegalHold: o.LegalHold,
		CorrelationID: o.CorrelationID, StartedAt: o.StartedAt, FrozenAt: o.FrozenAt,
		RetiredAt: o.RetiredAt,
	}
}

type obligationView struct {
	ObligationID  id.UUID    `json:"obligation_id"`
	OffboardingID id.UUID    `json:"offboarding_id"`
	TenantID      id.UUID    `json:"tenant_id"`
	Domain        string     `json:"domain"`
	Type          string     `json:"type"`
	State         string     `json:"state"`
	DueAt         *time.Time `json:"due_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	Detail        string     `json:"detail,omitempty"`
}

func viewObligation(o offboarding.Obligation) obligationView {
	return obligationView{
		ObligationID: o.ObligationID, OffboardingID: o.OffboardingID, TenantID: o.TenantID,
		Domain: o.Domain, Type: o.Type, State: string(o.State), DueAt: o.DueAt,
		CompletedAt: o.CompletedAt, Detail: o.Detail,
	}
}

type consumerView struct {
	ConsumerID        string `json:"consumer_id"`
	ProjectionVersion string `json:"projection_version"`

	// Seconds rather than a Go duration. `time.Duration` marshals as a nanosecond integer, which
	// reads as a nine-digit number nobody can interpret.
	MaxAcceptedAgeSeconds int64      `json:"max_accepted_age_seconds"`
	StaleBehavior         string     `json:"stale_behavior"`
	RegisteredAt          time.Time  `json:"registered_at"`
	SnapshotMark          *int64     `json:"snapshot_mark,omitempty"`
	LastReportedMark      *int64     `json:"last_reported_mark,omitempty"`
	LastReportedAt        *time.Time `json:"last_reported_at,omitempty"`
}

func viewConsumer(c projection.Consumer) consumerView {
	return consumerView{
		ConsumerID: c.ConsumerID, ProjectionVersion: c.ProjectionVersion,
		MaxAcceptedAgeSeconds: int64(c.MaxAcceptedAge / time.Second),
		StaleBehavior:         string(c.StaleBehavior), RegisteredAt: c.RegisteredAt,
		SnapshotMark: c.SnapshotMark, LastReportedMark: c.LastReportedMark,
		LastReportedAt: c.LastReportedAt,
	}
}
