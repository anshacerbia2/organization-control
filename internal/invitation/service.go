package invitation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/system"
)

// Service issues invitations and completes them into Membership.
//
// It holds both pools, and the split follows who can know what. Issuing, revoking, and listing are
// a Tenant administrator's work inside one Tenant, which is what the tenant-scoped role and its
// policy exist for. Resolving an identity-verification fact and sweeping expiries are not: both
// arrive without a Tenant — the first as a broker event correlated by identifier, the second as a
// clock — so neither can bind a Tenant it does not yet know.
type Service struct {
	tenantPool *db.TenantPool
	provider   *db.ProviderPool

	memberships *membership.Service

	now   func() time.Time
	newID func() (id.UUID, error)

	// newToken is a seam for tests. Production always mints from crypto/rand; a test needs a
	// token it can present back, and reading it out of the database would defeat the property
	// that the token is never stored.
	newToken func() (Token, string, error)

	// beforeGrant runs after the invitation's state change and before the Membership is created,
	// and is nil outside tests. The two must commit together, and the only honest way to assert
	// that is to fail in the window between them.
	beforeGrant func(context.Context) error
}

// New constructs the service.
func New(tenantPool *db.TenantPool, provider *db.ProviderPool,
	memberships *membership.Service) (*Service, error) {
	switch {
	case tenantPool == nil:
		return nil, errors.New("invitation: a tenant-scoped pool is required")
	case provider == nil:
		return nil, errors.New("invitation: a provider-scoped pool is required")
	case memberships == nil:
		return nil, errors.New("invitation: a membership service is required")
	}
	return &Service{
		tenantPool: tenantPool, provider: provider, memberships: memberships,
		now: time.Now, newID: id.NewV7, newToken: NewToken,
	}, nil
}

// IssueRequest records the intent that an identifier should come to hold Membership.
type IssueRequest struct {
	// TargetIdentifier is what the invitation is addressed to. Tier-2 PII under STD-GLB-007.
	TargetIdentifier string

	// WorkspaceID is optional. Absent, the invitation is into the Tenant as a whole.
	WorkspaceID *id.UUID

	SubjectType string
	Reason      string

	// TTL is optional and bounded. Absent, DefaultTTL applies; above MaxTTL it is refused rather
	// than clamped, because an inviter who asked for a year and silently got a month would
	// believe the invitation is live long after it lapsed.
	TTL time.Duration
}

// Issued is what a successful issue reports back.
type Issued struct {
	Invitation Invitation

	// Token is returned exactly once and is never persisted. A caller that loses it cannot
	// recover it — the invitation must be revoked and reissued, which is the correct outcome: a
	// token that can be re-read from storage is a token an operator with read access holds.
	Token Token
}

const insertStatement = `INSERT INTO invitation.invitation
    (invitation_id, tenant_id, workspace_id, target_identifier, target_hash, token_hash,
     subject_type, invited_by, reason, state, correlation_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', $10, $11)
RETURNING created_at`

// Issue records the intent and publishes the event that starts the identity flow.
func (s *Service) Issue(ctx context.Context, req IssueRequest) (Issued, error) {
	switch {
	case strings.TrimSpace(req.TargetIdentifier) == "":
		return Issued{}, errors.New("invitation: a target identifier is required")
	case req.SubjectType != "human" && req.SubjectType != "workload":
		return Issued{}, fmt.Errorf("invitation: subject_type %q is not human or workload", req.SubjectType)
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < 0 || ttl > MaxTTL {
		return Issued{}, fmt.Errorf("%w: %s is not between 0 and %s", ErrTTL, ttl, MaxTTL)
	}

	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Issued{}, db.ErrNoScope
	}

	invitationID, err := s.newID()
	if err != nil {
		return Issued{}, fmt.Errorf("invitation: mint identifier: %w", err)
	}
	correlationID := scope.Correlation()
	if correlationID.IsNil() {
		// The correlation is what joins the identity fact back to this intent, so it cannot be
		// absent. A tenant scope permits an empty correlation; this flow does not.
		if correlationID, err = s.newID(); err != nil {
			return Issued{}, fmt.Errorf("invitation: mint correlation: %w", err)
		}
	}

	token, tokenHash, err := s.newToken()
	if err != nil {
		return Issued{}, err
	}

	at := s.now().UTC()
	record := Invitation{
		InvitationID:     invitationID,
		TenantID:         scope.TenantID(),
		WorkspaceID:      req.WorkspaceID,
		TargetIdentifier: req.TargetIdentifier,
		TargetHash:       HashIdentifier(req.TargetIdentifier),
		SubjectType:      req.SubjectType,
		InvitedBy:        scope.Actor(),
		Reason:           req.Reason,
		State:            StatePending,
		CorrelationID:    correlationID,
		ExpiresAt:        at.Add(ttl),
	}

	if err := db.WithTenantScope(ctx, s.tenantPool, func(ctx context.Context, tx db.Tx) error {
		if err := tx.QueryRow(ctx, insertStatement,
			record.InvitationID.String(), record.TenantID.String(), nullableUUID(record.WorkspaceID),
			record.TargetIdentifier, record.TargetHash, tokenHash, record.SubjectType,
			record.InvitedBy.String(), nullableText(record.Reason), record.CorrelationID.String(),
			record.ExpiresAt).Scan(&record.CreatedAt); err != nil {
			return fmt.Errorf("invitation: insert: %w", err)
		}
		return s.publish(ctx, tx, requestedEventType, record, at)
	}); err != nil {
		return Issued{}, err
	}

	return Issued{Invitation: record, Token: token}, nil
}

// Lookup answers the unauthenticated pre-authentication request.
//
// It reads nothing. The design requires every outcome to render identically — absent, expired,
// revoked, accepted, and valid — so no part of the answer derives from the row, and reading it
// would disclose nothing while requiring cross-Tenant authority to do: the table is Row-Level
// Security protected and an anonymous caller can bind neither a Tenant nor an actor.
//
// Response-time uniformity follows from the same property. With no query, there is no duration that
// could separate a valid token from one that never existed.
//
// The boolean is not the invitation's validity. It reports only whether the value could be a token
// this service issued, which is what lets a malformed request be rejected as malformed rather than
// consuming a request slot. A caller must render the same page either way.
func (s *Service) Lookup(token Token) bool {
	return token.WellFormed()
}

const selectByTokenHash = `SELECT invitation_id::text,
       tenant_id::text,
       coalesce(workspace_id::text, ''),
       target_identifier,
       target_hash,
       subject_type,
       invited_by::text,
       coalesce(reason, ''),
       state,
       correlation_id::text,
       coalesce(principal_id::text, ''),
       expires_at,
       created_at,
       accepted_at,
       revoked_at
FROM invitation.invitation
WHERE token_hash = $1
FOR UPDATE`

const selectForUpdate = `SELECT invitation_id::text,
       tenant_id::text,
       coalesce(workspace_id::text, ''),
       target_identifier,
       target_hash,
       subject_type,
       invited_by::text,
       coalesce(reason, ''),
       state,
       correlation_id::text,
       coalesce(principal_id::text, ''),
       expires_at,
       created_at,
       accepted_at,
       revoked_at
FROM invitation.invitation
WHERE invitation_id = $1
FOR UPDATE`

const markVerified = `UPDATE invitation.invitation
SET state = 'identity_verified', principal_id = $2
WHERE invitation_id = $1`

// VerifiedIdentity is the fact the identity kernel reports back.
type VerifiedIdentity struct {
	// CorrelationID joins the fact to the intent. Carried through the identity flow rather than
	// re-derived, so a verification cannot be matched to an invitation nobody sent.
	CorrelationID id.UUID

	// Identifier is the identifier the kernel verified. Checked against the invitation's own
	// target: a verification for a different identifier is not this invitation's second fact,
	// however well the correlation matches.
	Identifier string

	PrincipalID id.UUID
}

const selectByCorrelation = `SELECT invitation_id::text FROM invitation.invitation
WHERE correlation_id = $1 AND state = 'pending'`

// RecordVerifiedIdentity is the second of the two facts arriving.
//
// Provider-scoped, because the fact arrives from the broker carrying a correlation and an
// identifier and no Tenant. It records and does not accept: Membership is created by Accept, which
// runs the Tenant recheck and the duplicate check. Splitting them is what keeps a broker redelivery
// from being an acceptance.
func (s *Service) RecordVerifiedIdentity(ctx context.Context, fact VerifiedIdentity) (Invitation, error) {
	switch {
	case fact.CorrelationID.IsNil():
		return Invitation{}, errors.New("invitation: a correlation identifier is required")
	case strings.TrimSpace(fact.Identifier) == "":
		return Invitation{}, errors.New("invitation: the verified identifier is required")
	case fact.PrincipalID.IsNil():
		return Invitation{}, errors.New("invitation: a principal identifier is required")
	}

	var record Invitation
	at := s.now().UTC()

	if err := db.WithProviderScope(ctx, s.provider,
		"record verified identity for correlation "+fact.CorrelationID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var rawID string
			if err := tx.QueryRow(ctx, selectByCorrelation, fact.CorrelationID.String()).Scan(&rawID); err != nil {
				return fmt.Errorf("%w: no pending invitation for correlation %s",
					ErrNotFound, fact.CorrelationID)
			}
			invitationID, err := id.Parse(rawID)
			if err != nil {
				return fmt.Errorf("invitation: stored identifier %q: %w", rawID, err)
			}

			loaded, err := load(ctx, tx, selectForUpdate, invitationID.String())
			if err != nil {
				return err
			}
			// The identifier must match. A correlation identifies the intent; it does not certify
			// which identifier was verified, and accepting a mismatch would admit whoever the
			// kernel happened to verify rather than whoever was invited.
			if loaded.TargetHash != HashIdentifier(fact.Identifier) {
				return fmt.Errorf(
					"%w: the verified identifier is not the invited one", ErrTransitionRefused)
			}
			// Expiry is checked against the clock, not the state. The sweep may not have run.
			if loaded.Expired(at) {
				return fmt.Errorf("%w: %s", ErrExpired, loaded.InvitationID)
			}
			if _, err := Resolve(ActionVerifyIdentity, loaded.State); err != nil {
				return err
			}

			if _, err := tx.Exec(ctx, markVerified,
				invitationID.String(), fact.PrincipalID.String()); err != nil {
				return fmt.Errorf("invitation: record verified identity: %w", err)
			}
			loaded.State = StateIdentityVerified
			principal := fact.PrincipalID
			loaded.PrincipalID = &principal
			record = loaded
			return nil
		}); err != nil {
		return Invitation{}, err
	}

	return record, nil
}

const acceptStatement = `UPDATE invitation.invitation
SET state = 'accepted', accepted_at = $2
WHERE invitation_id = $1`

const tenantStatusStatement = `SELECT status FROM tenant.tenant WHERE tenant_id = $1`

const activeMembershipStatement = `SELECT count(*) FROM membership.membership
WHERE principal_id = $1
  AND tenant_id = $2
  AND coalesce(workspace_id, tenant_id) = coalesce($3::uuid, tenant_id)
  AND subject_type = $4
  AND status = 'active'`

// Accept completes the join and creates the Membership.
//
// Tenant-scoped, because it writes a Membership and the Membership service is bound to the
// tenant-scoped policy. The caller supplies the token: acceptance is the authenticated path, so
// resolving by token here is the read the anonymous lookup deliberately does not perform.
//
// Everything commits together — the invitation's state, its accepted timestamp, the Membership, and
// the Membership's grant event. An accepted invitation with no Membership is an intent recorded as
// fulfilled that granted nothing, and a Membership against an invitation still open is an intent
// that can be fulfilled twice.
func (s *Service) Accept(ctx context.Context, token Token) (Invitation, membership.Result, error) {
	if !token.WellFormed() {
		return Invitation{}, membership.Result{}, ErrToken
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Invitation{}, membership.Result{}, db.ErrNoScope
	}

	var (
		record Invitation
		result membership.Result
	)
	at := s.now().UTC()

	if err := db.WithTenantScope(ctx, s.tenantPool, func(ctx context.Context, tx db.Tx) error {
		loaded, err := load(ctx, tx, selectByTokenHash, token.Hash())
		if err != nil {
			return err
		}
		if loaded.TenantID != scope.TenantID() {
			// Unreachable through the policy, which already confines the read. Kept because the
			// refusal must not depend on the policy being the only thing that stops it.
			return fmt.Errorf("%w: %s", ErrNotFound, loaded.InvitationID)
		}
		// Expiry against the clock rather than the state, which closes the race between the sweep
		// and an acceptance arriving in the same moment: whichever runs first, the invitation is
		// past its expiry and acceptance is refused.
		if loaded.Expired(at) {
			return fmt.Errorf("%w: %s", ErrExpired, loaded.InvitationID)
		}
		if _, err := Resolve(ActionAccept, loaded.State); err != nil {
			return err
		}
		if loaded.PrincipalID == nil {
			return fmt.Errorf("%w: no verified Principal is recorded", ErrTransitionRefused)
		}

		var status string
		if err := tx.QueryRow(ctx, tenantStatusStatement, loaded.TenantID.String()).Scan(&status); err != nil {
			return fmt.Errorf("invitation: read tenant status: %w", err)
		}
		if status != "active" {
			return fmt.Errorf("%w: %s is %s", ErrTenantNotActive, loaded.TenantID, status)
		}

		var existing int
		if err := tx.QueryRow(ctx, activeMembershipStatement,
			loaded.PrincipalID.String(), loaded.TenantID.String(),
			nullableUUID(loaded.WorkspaceID), loaded.SubjectType).Scan(&existing); err != nil {
			return fmt.Errorf("invitation: count active Memberships: %w", err)
		}
		if existing > 0 {
			return fmt.Errorf("%w: %s", ErrAlreadyMember, loaded.PrincipalID)
		}

		if _, err := tx.Exec(ctx, acceptStatement, loaded.InvitationID.String(), at); err != nil {
			return fmt.Errorf("invitation: accept: %w", err)
		}
		loaded.State = StateAccepted
		accepted := at
		loaded.AcceptedAt = &accepted

		if s.beforeGrant != nil {
			if err := s.beforeGrant(ctx); err != nil {
				return err
			}
		}

		granted, err := s.memberships.GrantWithin(ctx, tx, membership.GrantRequest{
			PrincipalID: *loaded.PrincipalID,
			TenantID:    loaded.TenantID,
			WorkspaceID: derefUUID(loaded.WorkspaceID),
			SubjectType: loaded.SubjectType,
			// PAD-PLT-002 §3.2 defines provenance as how the Membership came to exist. An access
			// review asks whether access arrived by invitation, migration, or provider grant, and
			// this is the row that answers it.
			Provenance: "invitation " + loaded.InvitationID.String(),
			ValidFrom:  at,
		})
		if err != nil {
			return err
		}
		result = granted
		record = loaded

		return s.publishAction(ctx, tx, ActionAccept, loaded, at)
	}); err != nil {
		return Invitation{}, membership.Result{}, err
	}

	return record, result, nil
}

const revokeStatement = `UPDATE invitation.invitation
SET state = 'revoked', revoked_at = $2
WHERE invitation_id = $1`

// Revoke withdraws the intent. Tenant-scoped: it is the inviter's own decision inside its Tenant.
func (s *Service) Revoke(ctx context.Context, invitationID id.UUID) (Invitation, error) {
	if invitationID.IsNil() {
		return Invitation{}, errors.New("invitation: an invitation identifier is required")
	}
	if _, ok := db.ScopeFrom(ctx); !ok {
		return Invitation{}, db.ErrNoScope
	}

	var record Invitation
	at := s.now().UTC()

	if err := db.WithTenantScope(ctx, s.tenantPool, func(ctx context.Context, tx db.Tx) error {
		loaded, err := load(ctx, tx, selectForUpdate, invitationID.String())
		if err != nil {
			return err
		}
		if _, err := Resolve(ActionRevoke, loaded.State); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, revokeStatement, invitationID.String(), at); err != nil {
			return fmt.Errorf("invitation: revoke: %w", err)
		}
		loaded.State = StateRevoked
		revoked := at
		loaded.RevokedAt = &revoked
		record = loaded
		return s.publishAction(ctx, tx, ActionRevoke, loaded, at)
	}); err != nil {
		return Invitation{}, err
	}

	return record, nil
}

const selectLapsed = `SELECT invitation_id::text
FROM invitation.invitation
WHERE state IN ('pending', 'identity_verified')
  AND expires_at <= $1
ORDER BY expires_at
LIMIT $2
FOR UPDATE SKIP LOCKED`

const expireStatement = `UPDATE invitation.invitation SET state = 'expired' WHERE invitation_id = $1`

// ExpireLapsed materialises expiry for up to size invitations and reports how many it changed.
//
// Materialised rather than computed at read time, so an expired invitation reads as expired in
// every listing and every report without each reader reimplementing the comparison — and so the
// slot the partial unique index guards is released, letting a new invitation be issued to the same
// person.
//
// Provider-scoped and batched: expiry is a property of the clock rather than of any Tenant, and the
// batch predicate is the resume token, so a sweep that stopped halfway simply does not see the rows
// it already committed.
func (s *Service) ExpireLapsed(ctx context.Context, size int) (int, error) {
	if size <= 0 {
		return 0, errors.New("invitation: a positive batch size is required")
	}

	at := s.now().UTC()
	var expired int

	if err := db.WithProviderScope(ctx, s.provider, "materialise lapsed invitations",
		func(ctx context.Context, tx db.Tx) error {
			rows, err := tx.Query(ctx, selectLapsed, at, size)
			if err != nil {
				return fmt.Errorf("invitation: select lapsed: %w", err)
			}
			var ids []string
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					rows.Close()
					return fmt.Errorf("invitation: scan lapsed: %w", err)
				}
				ids = append(ids, raw)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("invitation: read lapsed: %w", err)
			}

			for _, raw := range ids {
				loaded, err := load(ctx, tx, selectForUpdate, raw)
				if err != nil {
					return err
				}
				if _, err := Resolve(ActionExpire, loaded.State); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, expireStatement, raw); err != nil {
					return fmt.Errorf("invitation: expire: %w", err)
				}
				loaded.State = StateExpired
				if err := s.publishAction(ctx, tx, ActionExpire, loaded, at); err != nil {
					return err
				}
				expired++
			}
			return nil
		}); err != nil {
		return 0, err
	}

	return expired, nil
}

// Get reads one invitation in the bound Tenant.
func (s *Service) Get(ctx context.Context, invitationID id.UUID) (Invitation, error) {
	var record Invitation
	if err := db.WithTenantScope(ctx, s.tenantPool, func(ctx context.Context, tx db.Tx) error {
		var err error
		record, err = load(ctx, tx, selectForUpdate, invitationID.String())
		return err
	}); err != nil {
		return Invitation{}, err
	}
	return record, nil
}

func load(ctx context.Context, tx db.Tx, statement, key string) (Invitation, error) {
	var (
		record                                     Invitation
		rawID, rawTenant, rawWorkspace             string
		rawInvitedBy, rawCorrelation, rawPrincipal string
		state                                      string
	)
	if err := tx.QueryRow(ctx, statement, key).Scan(
		&rawID, &rawTenant, &rawWorkspace, &record.TargetIdentifier, &record.TargetHash,
		&record.SubjectType, &rawInvitedBy, &record.Reason, &state, &rawCorrelation,
		&rawPrincipal, &record.ExpiresAt, &record.CreatedAt,
		&record.AcceptedAt, &record.RevokedAt); err != nil {
		// Under Row-Level Security an invitation in another Tenant is simply absent, and reporting
		// that it exists elsewhere would disclose a row this caller may not read.
		return Invitation{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}

	for target, raw := range map[*id.UUID]string{
		&record.InvitationID:  rawID,
		&record.TenantID:      rawTenant,
		&record.InvitedBy:     rawInvitedBy,
		&record.CorrelationID: rawCorrelation,
	} {
		parsed, err := id.Parse(raw)
		if err != nil {
			return Invitation{}, fmt.Errorf("invitation: stored identifier %q: %w", raw, err)
		}
		*target = parsed
	}
	if rawWorkspace != "" {
		workspace, err := id.Parse(rawWorkspace)
		if err != nil {
			return Invitation{}, fmt.Errorf("invitation: stored workspace %q: %w", rawWorkspace, err)
		}
		record.WorkspaceID = &workspace
	}
	if rawPrincipal != "" {
		principal, err := id.Parse(rawPrincipal)
		if err != nil {
			return Invitation{}, fmt.Errorf("invitation: stored principal %q: %w", rawPrincipal, err)
		}
		record.PrincipalID = &principal
	}

	record.State = State(state)
	if !record.State.Valid() {
		return Invitation{}, fmt.Errorf("invitation: stored state %q is not a state", state)
	}
	return record, nil
}

func (s *Service) publishAction(ctx context.Context, tx db.Tx, action Action,
	record Invitation, at time.Time) error {
	eventType, publishes, err := EventType(action)
	if err != nil {
		return err
	}
	if !publishes {
		return nil
	}
	return s.publish(ctx, tx, string(eventType), record, at)
}

func (s *Service) publish(ctx context.Context, tx db.Tx, rawType string,
	record Invitation, at time.Time) error {
	eventType, err := event.ParseType(rawType)
	if err != nil {
		return fmt.Errorf("invitation: event type %q: %w", rawType, err)
	}
	envelope, err := event.New(system.Source, eventType, at, NewPayload(record))
	if err != nil {
		return fmt.Errorf("invitation: build envelope: %w", err)
	}
	// The standard lane. An invitation grants nothing and withdraws nothing, so none of these
	// competes with a live revocation — and the event that does grant access is the Membership's
	// own, appended by the Membership service inside the acceptance transaction.
	if err := outbox.Append(ctx, tx, record.InvitationID, envelope); err != nil {
		return fmt.Errorf("invitation: append event: %w", err)
	}
	return nil
}

func nullableUUID(value *id.UUID) any {
	if value == nil {
		return nil
	}
	return value.String()
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func derefUUID(value *id.UUID) id.UUID {
	if value == nil {
		return id.UUID{}
	}
	return *value
}
