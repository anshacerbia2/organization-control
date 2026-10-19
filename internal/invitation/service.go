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
// It holds three pools, and the split follows who can know what. Issuing, revoking, and listing are
// a Tenant administrator's work inside one Tenant, which is what the tenant-scoped role and its
// policy exist for. Sweeping expiries is not: it arrives as a clock, with no Tenant. Accepting is the
// invited person's own command, signed in with no Tenant (ADR-ORG-007 §5.1): the token selects the
// row through the self pool, and the row names the Tenant the Membership is written in.
type Service struct {
	tenantPool *db.TenantPool
	provider   *db.ProviderPool
	self       *db.SelfPool

	memberships *membership.Service

	// matcher asks the Identity Control API whether the caller holds the invited address. Nil when
	// no client is configured, and then every acceptance is ErrIdentityUnavailable: unknown is not no.
	matcher AddressMatcher

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

// AddressMatcher answers whether a Principal holds an address as a proven address: an `active`
// address with `verified_at` set, of an active Principal (ADR-ORG-007 §5.3). internal/identity's
// Client is the production one; the composition root hands it here, so this package holds no
// network client.
type AddressMatcher interface {
	Match(ctx context.Context, principal id.UUID, address string) (bool, error)
}

// New constructs the service. matcher may be nil, for a deployment with no Identity Control API
// configured; acceptance then answers ErrIdentityUnavailable and changes nothing.
func New(tenantPool *db.TenantPool, provider *db.ProviderPool, self *db.SelfPool,
	memberships *membership.Service, matcher AddressMatcher) (*Service, error) {
	switch {
	case tenantPool == nil:
		return nil, errors.New("invitation: a tenant-scoped pool is required")
	case provider == nil:
		return nil, errors.New("invitation: a provider-scoped pool is required")
	case self == nil:
		return nil, errors.New("invitation: a self pool is required")
	case memberships == nil:
		return nil, errors.New("invitation: a membership service is required")
	}
	return &Service{
		tenantPool: tenantPool, provider: provider, self: self, memberships: memberships, matcher: matcher,
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

// Issue records the intent and publishes `invitation.requested`.
func (s *Service) Issue(ctx context.Context, req IssueRequest) (Issued, error) {
	switch {
	case strings.TrimSpace(req.TargetIdentifier) == "":
		return Issued{}, fmt.Errorf("%w: a target identifier is required", ErrInvalid)
	case req.SubjectType != "human" && req.SubjectType != "workload":
		return Issued{}, fmt.Errorf("%w: subject_type %q is not human or workload", ErrInvalid, req.SubjectType)
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
		// The correlation names the intent in every event it publishes, so it cannot be absent. A
		// tenant scope permits an empty correlation; this flow does not.
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
		if err := s.publish(ctx, tx, requestedEventType, record, at); err != nil {
			return err
		}
		return db.Respond(ctx, tx, Issued{Invitation: record, Token: token})
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

// selectRedeemable is acceptance's narrow read, as the self role through invitation_self_redeem: the
// one row whose token_hash the transaction bound, and the columns the self role is granted. The
// WHERE repeats the binding so the statement reads the same row whatever the policy admits.
const selectRedeemable = `SELECT invitation_id::text, tenant_id::text, coalesce(workspace_id::text, ''),
       target_identifier, subject_type, state, expires_at
FROM invitation.invitation
WHERE token_hash = $1`

// selectByTokenHash reloads the row in the acceptance transaction, under the Tenant's policy, and
// locks it: the row the token selected, in the Tenant the read named, or nothing.
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

const acceptStatement = `UPDATE invitation.invitation
SET state = 'accepted', principal_id = $2, accepted_at = $3
WHERE invitation_id = $1`

const tenantStatusStatement = `SELECT status FROM tenant.tenant WHERE tenant_id = $1`

const activeMembershipStatement = `SELECT count(*) FROM membership.membership
WHERE principal_id = $1
  AND tenant_id = $2
  AND coalesce(workspace_id, tenant_id) = coalesce($3::uuid, tenant_id)
  AND subject_type = $4
  AND status = 'active'`

// Acceptance is what Accept returns, as one value: the HTTP surface renders it, and Accept hands it
// to db.Respond so the response is recorded with the effect.
type Acceptance struct {
	Invitation Invitation
	Membership membership.Result
}

// redeemable is what the narrow read returns: enough to check the invitation and ask the match.
type redeemable struct {
	tenantID    id.UUID
	target      string
	subjectType string
	state       State
	expiresAt   time.Time
}

// Accept completes the join and creates the Membership for the caller (ADR-ORG-007 §5.1 to §5.4).
//
// The caller is the self scope's actor, the signed-in person; no field of the request names a
// Principal. In order:
//
//  1. The token's hash selects at most one row, read as the self role (db.WithSelfInvitation).
//  2. It must be pending and not past its expiry by the clock.
//  3. The Identity Control API is asked whether the caller holds the row's address as a proven
//     address. Asked before any transaction opens, so no lock is held across the call. Unreachable or
//     unconfigured is ErrIdentityUnavailable, and nothing changes.
//  4. One transaction in the Tenant the row names (db.WithSelfInTenant) reloads the row by its token
//     hash FOR UPDATE, rechecks it, refuses a Tenant that is not active or an existing Membership,
//     creates the Membership for the caller, records the acceptance, and publishes.
//
// Every failure in 1 to 3, the match answering no included, and a row the reload no longer finds
// pending, is ErrNotAcceptable: one refusal, so no answer tells a token holder which tokens are live
// or that an invitation is addressed to someone else (ADR-ORG-007 §5.4).
//
// Everything in 4 commits together — the invitation's state, its accepted timestamp, the Membership,
// and both events. An accepted invitation with no Membership is an intent recorded as fulfilled that
// granted nothing, and a Membership against an invitation still open is an intent that can be
// fulfilled twice.
func (s *Service) Accept(ctx context.Context, token Token) (Invitation, membership.Result, error) {
	if !token.WellFormed() {
		return Invitation{}, membership.Result{}, ErrToken
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Invitation{}, membership.Result{}, db.ErrNoScope
	}
	if !scope.IsSelf() {
		// The route admits the self caller alone; a service reached with another scope is a wiring
		// defect, and the Membership would otherwise go to whoever that scope names.
		return Invitation{}, membership.Result{}, fmt.Errorf("%w: acceptance is the invited person's own command", db.ErrWrongScope)
	}
	caller := scope.Actor()
	hash := token.Hash()
	at := s.now().UTC()

	var row redeemable
	found := false
	if err := db.WithSelfInvitation(ctx, s.self, hash, func(ctx context.Context, tx db.Tx) error {
		// Query rather than QueryRow, so an absent row (no Next) is told apart from a failed read
		// (Err): the first is a refusal, the second an internal error, and the driver that would name
		// its no-rows sentinel is not importable here.
		rows, err := tx.Query(ctx, selectRedeemable, hash)
		if err != nil {
			return fmt.Errorf("invitation: read by token: %w", err)
		}
		defer rows.Close()
		if !rows.Next() {
			return rows.Err()
		}
		var rawID, rawTenant, rawWorkspace, state string
		if err := rows.Scan(&rawID, &rawTenant, &rawWorkspace,
			&row.target, &row.subjectType, &state, &row.expiresAt); err != nil {
			return fmt.Errorf("invitation: scan by token: %w", err)
		}
		tenantID, err := id.Parse(rawTenant)
		if err != nil {
			return fmt.Errorf("invitation: stored tenant %q: %w", rawTenant, err)
		}
		row.tenantID, row.state, found = tenantID, State(state), true
		return nil
	}); err != nil {
		return Invitation{}, membership.Result{}, err
	}
	// Absent, settled, or lapsed by the clock rather than the state, which closes the race between
	// the sweep and an acceptance arriving in the same moment.
	if !found || row.state != StatePending || !at.Before(row.expiresAt) {
		return Invitation{}, membership.Result{}, ErrNotAcceptable
	}

	if s.matcher == nil {
		return Invitation{}, membership.Result{}, fmt.Errorf("%w: no Identity Control API is configured", ErrIdentityUnavailable)
	}
	match, err := s.matcher.Match(ctx, caller, row.target)
	if err != nil {
		// The matcher's error names no address (internal/identity), and it is not returned to the
		// caller: the 503 says only that the question could not be asked.
		return Invitation{}, membership.Result{}, fmt.Errorf("%w: %w", ErrIdentityUnavailable, err)
	}
	if !match {
		return Invitation{}, membership.Result{}, ErrNotAcceptable
	}

	var (
		record Invitation
		result membership.Result
	)
	if err := db.WithSelfInTenant(ctx, s.tenantPool, row.tenantID, func(ctx context.Context, tx db.Tx) error {
		loaded, err := load(ctx, tx, selectByTokenHash, hash)
		if errors.Is(err, ErrNotFound) {
			// The Tenant the read named no longer holds the token's row under its own policy: nothing
			// this caller may accept.
			return ErrNotAcceptable
		}
		if err != nil {
			return err
		}
		if loaded.TenantID != row.tenantID {
			// Unreachable through the policy, which already confines the reload to the bound Tenant.
			// Kept because the refusal must not depend on the policy being the only thing that stops it.
			return ErrNotAcceptable
		}
		// Rechecked under the lock: a concurrent acceptance, revocation or expiry since the read
		// leaves this one nothing to accept. Used once (ADR-ORG-007 §5.4).
		// The address must still be the one the match was asked about.
		if loaded.State != StatePending || loaded.Expired(at) || loaded.TargetIdentifier != row.target {
			return ErrNotAcceptable
		}
		if _, err := Resolve(ActionAccept, loaded.State); err != nil {
			return err
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
			caller.String(), loaded.TenantID.String(),
			nullableUUID(loaded.WorkspaceID), loaded.SubjectType).Scan(&existing); err != nil {
			return fmt.Errorf("invitation: count active Memberships: %w", err)
		}
		if existing > 0 {
			return fmt.Errorf("%w: %s", ErrAlreadyMember, caller)
		}

		if _, err := tx.Exec(ctx, acceptStatement, loaded.InvitationID.String(), caller.String(), at); err != nil {
			return fmt.Errorf("invitation: accept: %w", err)
		}
		loaded.State = StateAccepted
		accepted := at
		loaded.AcceptedAt = &accepted
		principal := caller
		loaded.PrincipalID = &principal

		if s.beforeGrant != nil {
			if err := s.beforeGrant(ctx); err != nil {
				return err
			}
		}

		granted, err := s.memberships.GrantWithin(ctx, tx, membership.GrantRequest{
			PrincipalID: caller,
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

		if err := s.publishAction(ctx, tx, ActionAccept, loaded, at); err != nil {
			return err
		}
		return db.Respond(ctx, tx, Acceptance{Invitation: record, Membership: result})
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
		return Invitation{}, fmt.Errorf("%w: an invitation identifier is required", ErrInvalid)
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
		if err := s.publishAction(ctx, tx, ActionRevoke, loaded, at); err != nil {
			return err
		}
		return db.Respond(ctx, tx, record)
	}); err != nil {
		return Invitation{}, err
	}

	return record, nil
}

// selectLapsed and expireStatement go through operation.invitation_expiry, a view the migration role
// owns, whose rows are the invitations past their expiry by the database clock and whose policy lets
// the state become `expired` and nothing else (TDD-organization-control-004 §Expiry Sweep). It carries
// what the event needs and never the target identifier. The route and the schedule run these same
// statements: the route under a provider scope, the schedule on the raw provider connections.
//
// `expires_at <= $1` as well as the view's own `now()`: the service's clock decides which invitations
// a run takes, and the database's clock bounds it, so neither can expire an invitation the other
// still holds to be live.
const selectLapsed = `SELECT invitation_id::text,
       tenant_id::text,
       coalesce(workspace_id::text, ''),
       subject_type,
       state,
       correlation_id::text,
       coalesce(principal_id::text, ''),
       expires_at
FROM operation.invitation_expiry
WHERE expires_at <= $1
ORDER BY expires_at
LIMIT $2
FOR UPDATE SKIP LOCKED`

const expireStatement = `UPDATE operation.invitation_expiry SET state = 'expired' WHERE invitation_id = $1`

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
		return 0, fmt.Errorf("%w: a positive batch size is required", ErrInvalid)
	}

	var expired int
	if err := db.WithProviderScope(ctx, s.provider, "materialise lapsed invitations",
		func(ctx context.Context, tx db.Tx) error {
			var err error
			expired, err = s.expireLapsed(ctx, tx, size)
			return err
		}); err != nil {
		return 0, err
	}
	return expired, nil
}

func (s *Service) expireLapsed(ctx context.Context, tx db.Tx, size int) (int, error) {
	at := s.now().UTC()

	rows, err := tx.Query(ctx, selectLapsed, at, size)
	if err != nil {
		return 0, fmt.Errorf("invitation: select lapsed: %w", err)
	}
	var lapsed []Invitation
	for rows.Next() {
		record, err := scanLapsed(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		lapsed = append(lapsed, record)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("invitation: read lapsed: %w", err)
	}

	expired := 0
	for _, record := range lapsed {
		if _, err := Resolve(ActionExpire, record.State); err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, expireStatement, record.InvitationID.String())
		if err != nil {
			return 0, fmt.Errorf("invitation: expire: %w", err)
		}
		if tag.RowsAffected() != 1 {
			// Locked by the select, so nothing else changed it; a row the view no longer returns is
			// one this run does not expire, and it publishes nothing for it.
			continue
		}
		record.State = StateExpired
		if err := s.publishAction(ctx, tx, ActionExpire, record, at); err != nil {
			return 0, err
		}
		expired++
	}
	return expired, nil
}

// scanLapsed reads one row of selectLapsed: the identifiers, state and expiry the event is built from.
func scanLapsed(r rowScanner) (Invitation, error) {
	var (
		record                                    Invitation
		rawID, rawTenant, rawWorkspace, rawCorrel string
		rawPrincipal, state                       string
	)
	if err := r.Scan(&rawID, &rawTenant, &rawWorkspace, &record.SubjectType, &state, &rawCorrel,
		&rawPrincipal, &record.ExpiresAt); err != nil {
		return Invitation{}, fmt.Errorf("invitation: scan lapsed: %w", err)
	}
	for target, raw := range map[*id.UUID]string{
		&record.InvitationID:  rawID,
		&record.TenantID:      rawTenant,
		&record.CorrelationID: rawCorrel,
	} {
		parsed, err := id.Parse(raw)
		if err != nil {
			return Invitation{}, fmt.Errorf("invitation: stored identifier %q: %w", raw, err)
		}
		*target = parsed
	}
	for target, raw := range map[**id.UUID]string{&record.WorkspaceID: rawWorkspace, &record.PrincipalID: rawPrincipal} {
		if raw == "" {
			continue
		}
		parsed, err := id.Parse(raw)
		if err != nil {
			return Invitation{}, fmt.Errorf("invitation: stored identifier %q: %w", raw, err)
		}
		*target = &parsed
	}
	record.State = State(state)
	return record, nil
}

// ScheduledExpiry is the expiry sweep the serving process runs every
// `ORGANIZATION_INVITATION_SWEEP_INTERVAL` (TDD-organization-control-004 §Expiry Sweep).
//
// It holds the raw provider connections and binds no scope, so a run files no privileged-access
// record: a timer is not a provider. What a run did is the `invitation.expired` event each expiry
// publishes in its own transaction.
type ScheduledExpiry struct {
	tx      db.Transactor
	service *Service
}

// Scheduled returns the scheduled form of ExpireLapsed over tx, the raw provider connections.
func (s *Service) Scheduled(tx db.Transactor) (*ScheduledExpiry, error) {
	if tx == nil {
		return nil, errors.New("invitation: the scheduled expiry requires the provider connections")
	}
	return &ScheduledExpiry{tx: tx, service: s}, nil
}

// ExpireLapsed materialises expiry for up to size invitations and reports how many it changed.
func (e *ScheduledExpiry) ExpireLapsed(ctx context.Context, size int) (int, error) {
	if size <= 0 {
		return 0, fmt.Errorf("%w: a positive batch size is required", ErrInvalid)
	}
	var expired int
	if err := e.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		expired, err = e.service.expireLapsed(ctx, tx, size)
		return err
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

// ListQuery selects one page of the bound Tenant's invitations (STD-GLB-001 1.3.0 §Pagination).
type ListQuery struct {
	// After is the last invitation_id of the previous page; the nil identifier starts at the first.
	After id.UUID

	// Limit is the page size, 1 to db.MaxListLimit; zero takes db.DefaultListLimit.
	Limit int

	// State narrows the list to one stored state; empty is every state. An invitation past its
	// expiry that the sweep has not reached is still `pending`, and acceptance refuses it anyway.
	State State
}

// Page is one page of invitations in creation order. Next is the After of the following page, and
// nil on the last.
type Page struct {
	Invitations []Invitation
	Next        *id.UUID
}

// listStatement is one keyset page of the bound Tenant's invitations, in creation order. Row-Level
// Security confines it to the Tenant. It reads the columns load reads, and the HTTP view drops the
// target identifier and its hash as it does for a single read.
const listStatement = `SELECT invitation_id::text,
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
WHERE ($1::text = '' OR state = $1::text)
  AND ($2::uuid IS NULL OR invitation_id > $2::uuid)
ORDER BY invitation_id
LIMIT $3`

// List reads one page of the bound Tenant's invitations, in a read-only transaction.
func (s *Service) List(ctx context.Context, query ListQuery) (Page, error) {
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return Page{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if query.State != "" && !query.State.Valid() {
		return Page{}, fmt.Errorf("%w: state must be pending, accepted, expired or revoked", ErrInvalid)
	}

	page := Page{Invitations: []Invitation{}}
	if err := db.WithTenantRead(ctx, s.tenantPool, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, listStatement, string(query.State), db.Keyset(query.After), limit+1)
		if err != nil {
			return fmt.Errorf("invitation: list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			record, err := scanInvitation(rows)
			if err != nil {
				return err
			}
			page.Invitations = append(page.Invitations, record)
		}
		return rows.Err()
	}); err != nil {
		return Page{}, err
	}

	if len(page.Invitations) > limit {
		page.Invitations = page.Invitations[:limit]
		next := page.Invitations[limit-1].InvitationID
		page.Next = &next
	}
	return page, nil
}

// rowScanner is what scanInvitation reads from: one row of a QueryRow or of a Query.
type rowScanner interface {
	Scan(dest ...any) error
}

// errScan marks a row that could not be read, which for a single read is an absent one.
var errScan = errors.New("invitation: scan")

func load(ctx context.Context, tx db.Tx, statement, key string) (Invitation, error) {
	record, err := scanInvitation(tx.QueryRow(ctx, statement, key))
	if errors.Is(err, errScan) {
		// Under Row-Level Security an invitation in another Tenant is simply absent, and reporting
		// that it exists elsewhere would disclose a row this caller may not read.
		return Invitation{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return record, err
}

func scanInvitation(r rowScanner) (Invitation, error) {
	var (
		record                                     Invitation
		rawID, rawTenant, rawWorkspace             string
		rawInvitedBy, rawCorrelation, rawPrincipal string
		state                                      string
	)
	if err := r.Scan(
		&rawID, &rawTenant, &rawWorkspace, &record.TargetIdentifier, &record.TargetHash,
		&record.SubjectType, &rawInvitedBy, &record.Reason, &state, &rawCorrelation,
		&rawPrincipal, &record.ExpiresAt, &record.CreatedAt,
		&record.AcceptedAt, &record.RevokedAt); err != nil {
		return Invitation{}, fmt.Errorf("%w: %w", errScan, err)
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
