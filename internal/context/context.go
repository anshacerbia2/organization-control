// Package context answers, authoritatively and synchronously, whether a Principal may assert a
// Tenant context right now.
//
// # Why this path exists at all
//
// Consumers hold a projection and decide from it without calling this service, which is what keeps
// the estate working. The fresh check is the exception: an irreversible or high-risk operation, or
// a projection whose declared freshness budget has been exceeded and whose stale behaviour is
// `revalidate`. It is not an ordinary read, and a consumer using it as one has misclassified its
// operations — which `TDD-organization-control-002` treats as a defect rather than as load, and
// which this package measures rather than assumes.
//
// # Context is not authorization
//
// A granted decision says this Principal holds an active Membership in an active Tenant. It says
// nothing about what they may do there. Permissions, entitlements, and business roles belong to
// their owning domains; a fresh check that returned them would have moved authorization into the
// context path, which EAD-006 rejects.
package context

import (
	stdcontext "context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// Refusal is why a context may not be asserted.
//
// Two values, and the split is drawn where it changes what the caller does rather than where the
// data happens to differ. A Tenant that is not active is an operational state an operator can fix
// and a user can be told about; everything else is "this Principal holds nothing here", which is
// the only thing a caller is entitled to learn.
//
// There is deliberately no separate refusal for a suspended or revoked Membership. It would be
// truthful and it would disclose that this Principal once held access in this Tenant, to a caller
// that holds nothing there now — and the caller's next move is identical either way, so the
// disclosure buys nothing it is entitled to.
type Refusal string

const (
	// RefusalNone means the context may be asserted.
	RefusalNone Refusal = ""

	// RefusalNoMembership means no active Membership backs this context. It covers an absent
	// Membership, a suspended or revoked one, and a Tenant that does not exist, on purpose.
	RefusalNoMembership Refusal = "no-membership"

	// RefusalTenantNotActive means the Membership is active inside a Tenant that is not.
	//
	// Separate because it is the one an operator can fix, and because a consumer that reported it
	// as "no membership" would tell a user they had lost their access when the Tenant is merely
	// suspended. Reached only when an active Membership exists, so it discloses nothing to a
	// caller who holds nothing.
	RefusalTenantNotActive Refusal = "tenant-not-active"
)

// Decision is the authoritative answer.
type Decision struct {
	Granted bool    `json:"granted"`
	Refusal Refusal `json:"refusal,omitempty"`

	TenantID    id.UUID `json:"tenant_id"`
	PrincipalID id.UUID `json:"principal_id"`

	// MembershipID and the versions are present only on a granted decision. On a refusal they
	// would describe a context the caller may not assert, and a caller that logged them would be
	// recording the shape of access it was denied.
	MembershipID id.UUID  `json:"membership_id,omitempty"`
	WorkspaceID  *id.UUID `json:"workspace_id,omitempty"`
	SubjectType  string   `json:"subject_type,omitempty"`

	// MembershipVersion and TenantSecurityVersion let the caller compare this answer against a
	// token it is holding. A token below either version is stale even though this check just
	// succeeded, because the check answers about now and the token was minted earlier.
	MembershipVersion     int64 `json:"membership_version,omitempty"`
	TenantSecurityVersion int64 `json:"tenant_security_version,omitempty"`

	// CheckedAt is the instant the authoritative read happened. A caller measuring how long it
	// may rely on this answer needs the origin, not the time the response arrived.
	CheckedAt time.Time `json:"checked_at"`
}

var (
	// ErrInvalid is a malformed request: a required field absent, a value outside its permitted
	// set, or two fields that contradict each other.
	//
	// It exists so the HTTP surface can answer 400. Before it, every validation failure here was
	// a bare errors.New, indistinguishable at the transport boundary from a failed statement --
	// so a caller who omitted a field received 500, which says the service is broken rather than
	// that the request is. Constructor guards and stored-value decoders deliberately do NOT carry
	// it: those are a process built wrong and a row that should not exist, and both are 500.
	ErrInvalid = errors.New("context: the request is invalid")

	// ErrNotRegistered means the calling consumer has no registry row. The fresh check is metered
	// per consumer, so an unregistered caller cannot be metered — and an unmetered fresh-check
	// path is the one that gets used as an ordinary read.
	ErrNotRegistered = errors.New("context: consumer is not registered")

	// ErrRequestRequired reports a rate report with no denominator.
	ErrRequestRequired = errors.New("context: a verify-rate report requires the request count")
)

// Service performs the authoritative fresh check.
//
// Provider-scoped. The check answers about any Tenant on behalf of a consumer that is not itself
// tenant-scoped, and `organization_rt` bound to one Tenant could not answer for another. The
// evidence obligation that comes with the provider path is appropriate here rather than
// incidental: a cross-Tenant read of who holds access is exactly the access an audit asks about.
type Service struct {
	pool *db.ProviderPool
	now  func() time.Time
}

// New constructs the service.
func New(pool *db.ProviderPool) (*Service, error) {
	if pool == nil {
		return nil, errors.New("context: a provider-scoped pool is required")
	}
	return &Service{pool: pool, now: time.Now}, nil
}

// VerifyRequest is one fresh check.
type VerifyRequest struct {
	// ConsumerID is the caller. Required: the rate signal is per consumer, and a check that could
	// be made anonymously would be a check nobody is accountable for.
	ConsumerID string

	TenantID    id.UUID
	PrincipalID id.UUID
}

// verifyStatement reads the Membership and its Tenant in one row.
//
// A LEFT JOIN from the Tenant so an absent Membership and a suspended Tenant come back
// distinguishable in one round trip. Two queries would make the pair readable at two instants,
// and a Tenant suspended between them would produce a granted decision citing an active Tenant.
const verifyStatement = `SELECT t.status,
       t.tenant_security_version,
       m.membership_id::text,
       coalesce(m.workspace_id::text, ''),
       m.subject_type,
       m.status,
       m.membership_version
FROM tenant.tenant t
LEFT JOIN membership.membership m
       ON m.tenant_id = t.tenant_id
      AND m.principal_id = $2
      AND m.status = 'active'
WHERE t.tenant_id = $1`

const countVerify = `UPDATE projection.consumer
SET verify_calls_since_report = verify_calls_since_report + 1
WHERE consumer_id = $1`

// Verify answers whether the context may be asserted, and meters the call.
//
// The read and the meter commit together. Metered afterwards in a separate transaction, a crash
// between them would answer a check that no counter records — and the counter exists precisely to
// catch a consumer making very many of these, which is the load under which such a crash is most
// likely.
//
// The meter is an UPDATE on the consumer's own row, so a consumer's checks serialise against each
// other. That is a real cost and it is accepted: a consumer generating enough fresh-check traffic
// to contend on its own counter row is exactly the consumer this signal exists to flag, and the
// contention is confined to it rather than shared with the estate.
func (s *Service) Verify(ctx stdcontext.Context, req VerifyRequest) (Decision, error) {
	switch {
	case req.ConsumerID == "":
		return Decision{}, fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	case req.TenantID.IsNil():
		return Decision{}, fmt.Errorf("%w: a tenant identifier is required", ErrInvalid)
	case req.PrincipalID.IsNil():
		return Decision{}, fmt.Errorf("%w: a principal identifier is required", ErrInvalid)
	}

	decision := Decision{
		TenantID:    req.TenantID,
		PrincipalID: req.PrincipalID,
		CheckedAt:   s.now().UTC(),
	}

	if err := db.WithProviderScope(ctx, s.pool,
		"authoritative context check for consumer "+req.ConsumerID,
		func(ctx stdcontext.Context, tx db.Tx) error {
			tag, err := tx.Exec(ctx, countVerify, req.ConsumerID)
			if err != nil {
				return fmt.Errorf("context: meter verify call: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("%w: %s", ErrNotRegistered, req.ConsumerID)
			}

			var (
				tenantStatus                             string
				rawMembership, rawWorkspace, subjectType *string
				membershipStatus                         *string
				membershipVersion                        *int64
			)
			if err := tx.QueryRow(ctx, verifyStatement,
				req.TenantID.String(), req.PrincipalID.String()).Scan(
				&tenantStatus, &decision.TenantSecurityVersion,
				&rawMembership, &rawWorkspace, &subjectType,
				&membershipStatus, &membershipVersion); err != nil {
				// An absent Tenant and an absent Membership are one answer to the caller: this
				// context cannot be asserted. Naming which would tell an unauthorised caller
				// whether a Tenant identifier exists.
				decision.Refusal = RefusalNoMembership
				return nil
			}

			if rawMembership == nil {
				// No active Membership. Whether a suspended or revoked one exists is deliberately
				// not probed: the answer is the same and the extra query would disclose that this
				// Principal once held access.
				decision.Refusal = RefusalNoMembership
				return nil
			}
			if tenantStatus != "active" {
				// Checked after the Membership so a caller cannot use the refusal to discover
				// which Tenants exist and what state they are in without holding a Membership.
				decision.Refusal = RefusalTenantNotActive
				return nil
			}

			membershipID, err := id.Parse(*rawMembership)
			if err != nil {
				return fmt.Errorf("context: stored membership id %q: %w", *rawMembership, err)
			}
			decision.MembershipID = membershipID
			if rawWorkspace != nil && *rawWorkspace != "" {
				workspace, err := id.Parse(*rawWorkspace)
				if err != nil {
					return fmt.Errorf("context: stored workspace id %q: %w", *rawWorkspace, err)
				}
				decision.WorkspaceID = &workspace
			}
			if subjectType != nil {
				decision.SubjectType = *subjectType
			}
			if membershipVersion != nil {
				decision.MembershipVersion = *membershipVersion
			}
			decision.Granted = true
			return nil
		}); err != nil {
		return Decision{}, err
	}

	return decision, nil
}

// SwitchEligible reports whether a Principal may switch into a Tenant context.
//
// The same authoritative check as Verify, and deliberately not a weaker one. A context switch mints
// authority for a context the caller was not previously operating in, so it is the one operation
// that must never be decided from a projection: the projection may be within its freshness budget
// and still predate the revocation that matters.
func (s *Service) SwitchEligible(ctx stdcontext.Context, req VerifyRequest) (Decision, error) {
	return s.Verify(ctx, req)
}

// RateReport is a consumer supplying the denominator for its fresh-check ratio.
type RateReport struct {
	ConsumerID string

	// Requests is how many requests the consumer served since its last report. It is the only
	// number this service cannot observe, which is why the consumer supplies it.
	Requests int64
}

// Rate is the measured misuse signal for one consumer.
type Rate struct {
	ConsumerID string  `json:"consumer_id"`
	Calls      int64   `json:"verify_calls"`
	Requests   int64   `json:"requests"`
	Ratio      float64 `json:"ratio"`
}

// The parameter is cast at every use. PostgreSQL deduces one type per parameter across the whole
// statement, and `$2` appears as a bigint column value and as a division operand — left uncast it
// is "inconsistent types deduced for parameter $2" rather than a silent coercion.
const recordRate = `UPDATE projection.consumer
SET last_reported_requests   = $2::bigint,
    last_verify_ratio        = CASE WHEN $2::bigint > 0
                                    THEN verify_calls_since_report::double precision
                                         / $2::bigint::double precision
                                    ELSE NULL END,
    verify_calls_since_report = 0
WHERE consumer_id = $1
RETURNING last_reported_requests, last_verify_ratio`

const readCalls = `SELECT verify_calls_since_report FROM projection.consumer WHERE consumer_id = $1`

// RecordRate closes one measurement interval and returns the ratio for it.
//
// Both counters reset together, which is what makes the ratio per-interval. A caller reporting zero
// requests leaves the ratio NULL rather than dividing: a consumer that served nothing and checked
// nothing is not misusing anything, and a consumer that served nothing and checked repeatedly has a
// ratio of infinity, which is a number no threshold comparison handles usefully. The calls are
// still cleared, so the next interval measures the next interval.
func (s *Service) RecordRate(ctx stdcontext.Context, report RateReport) (Rate, error) {
	if report.ConsumerID == "" {
		return Rate{}, fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	}
	if report.Requests < 0 {
		return Rate{}, fmt.Errorf("%w: %d is not a request count", ErrRequestRequired, report.Requests)
	}

	rate := Rate{ConsumerID: report.ConsumerID, Requests: report.Requests}

	if err := db.WithProviderScope(ctx, s.pool,
		"record verify rate for consumer "+report.ConsumerID,
		func(ctx stdcontext.Context, tx db.Tx) error {
			if err := tx.QueryRow(ctx, readCalls, report.ConsumerID).Scan(&rate.Calls); err != nil {
				return fmt.Errorf("%w: %s", ErrNotRegistered, report.ConsumerID)
			}
			var requests *int64
			var ratio *float64
			if err := tx.QueryRow(ctx, recordRate, report.ConsumerID, report.Requests).
				Scan(&requests, &ratio); err != nil {
				return fmt.Errorf("context: record verify rate: %w", err)
			}
			if ratio != nil {
				rate.Ratio = *ratio
			}
			return nil
		}); err != nil {
		return Rate{}, err
	}

	return rate, nil
}

// DefaultRateAlert matches ORGANIZATION_VERIFY_RATE_ALERT.
//
// Five per hundred requests. The number is a judgement and the shape is not: at one fresh check per
// twenty requests a consumer has stopped treating the path as reserved for high-risk operations,
// and TDD-organization-control-002 escalates ten times this as critical.
const DefaultRateAlert = 0.05

const overThresholdStatement = `SELECT consumer_id, verify_calls_since_report,
       coalesce(last_reported_requests, 0), coalesce(last_verify_ratio, 0)
FROM projection.consumer
WHERE last_verify_ratio IS NOT NULL
  AND last_verify_ratio > $1
ORDER BY last_verify_ratio DESC`

// OverThreshold names the consumers whose last measured interval exceeded the threshold.
//
// Ordered worst first. A consumer at ten times the threshold and one just over it appear in the same
// list, and the operator reading the first line should see the one that matters.
func (s *Service) OverThreshold(ctx stdcontext.Context, threshold float64) ([]Rate, error) {
	if threshold < 0 {
		return nil, fmt.Errorf("%w: a negative threshold flags every consumer", ErrInvalid)
	}

	var over []Rate
	if err := db.WithProviderScope(ctx, s.pool, "read verify-rate misuse signal",
		func(ctx stdcontext.Context, tx db.Tx) error {
			rows, err := tx.Query(ctx, overThresholdStatement, threshold)
			if err != nil {
				return fmt.Errorf("context: read verify rates: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var rate Rate
				if err := rows.Scan(&rate.ConsumerID, &rate.Calls, &rate.Requests, &rate.Ratio); err != nil {
					return fmt.Errorf("context: scan verify rate: %w", err)
				}
				over = append(over, rate)
			}
			return rows.Err()
		}); err != nil {
		return nil, err
	}
	return over, nil
}
