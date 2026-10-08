package authority

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// The privileged-access record, read and reviewed (ADR-ORG-002 §5.6, TDD-organization-control-001
// §Privileged Access Review). A provider in force reads every row and records a review of another
// provider's access; a Tenant administrator reads the provider rows that name its Tenant, through a
// view the migration role owns. Nothing here changes or deletes a row.

// ReviewDue is how long after it occurred a provider access may go unreviewed. CIS Safeguard 8.11:
// "Conduct reviews on a weekly, or more frequent, basis."
const ReviewDue = 7 * 24 * time.Hour

// The outcomes a review records.
const (
	// OutcomeAppropriate is a period whose accesses the reviewer found explained.
	OutcomeAppropriate = "appropriate"
	// OutcomeEscalated is a period with a use raised as possible misuse.
	OutcomeEscalated = "escalated"
)

var (
	// ErrAccessQueryInvalid means a list of the record was asked for with a filter it does not take.
	ErrAccessQueryInvalid = errors.New("authority: invalid privileged-access query")

	// ErrReviewInvalid means a review request is incomplete or names an impossible period.
	ErrReviewInvalid = errors.New("authority: invalid privileged-access review")

	// ErrSelfReview means a provider asked to review its own access (AC-5, ADR-ORG-002 §5.6).
	ErrSelfReview = errors.New("authority: a provider does not review its own access; another provider records the review")
)

// Access is one row of the privileged-access record.
type Access struct {
	ID          id.UUID
	Actor       id.UUID
	Authority   string
	Activation  *id.UUID
	Tenant      *id.UUID
	Operation   string
	Correlation id.UUID
	Reason      string
	OccurredAt  time.Time
}

// AccessQuery selects one page of the record (STD-GLB-001 1.5.0 §Pagination). Zero values are
// absent filters. From is inclusive and To exclusive.
type AccessQuery struct {
	After       id.UUID
	Limit       int
	Actor       id.UUID
	Tenant      id.UUID
	Correlation id.UUID
	Authority   string
	From        *time.Time
	To          *time.Time
}

// AccessPage is one page of the record, in access_id order. Next is the last identifier when
// another page follows, and nil on the last.
type AccessPage struct {
	Accesses []Access
	Next     *id.UUID
}

// Review is one recorded review of a provider's access over a period.
type Review struct {
	ID                id.UUID
	Actor             id.UUID
	From              time.Time
	To                time.Time
	Outcome           string
	Statement         string
	Accesses          int64
	EmergencyAccesses int64
	ReviewedBy        id.UUID
	ReviewedAt        time.Time
}

// ReviewRequest is a review to record. Statement is the reviewer's X-Administrative-Reason.
type ReviewRequest struct {
	Actor     id.UUID
	From      time.Time
	To        time.Time
	Outcome   string
	Statement string
}

// ReviewQuery selects one page of reviews.
type ReviewQuery struct {
	After      id.UUID
	Limit      int
	Actor      id.UUID
	ReviewedBy id.UUID
}

// ReviewPage is one page of reviews, in review_id order.
type ReviewPage struct {
	Reviews []Review
	Next    *id.UUID
}

// Unreviewed is one Principal's provider access that no review covers.
type Unreviewed struct {
	Actor      id.UUID
	Unreviewed int64
	Emergency  int64
	OldestAt   time.Time
	// DueAt is ReviewDue after the oldest unreviewed access.
	DueAt   time.Time
	Overdue bool
}

// AccessReview reads and reviews the record: as the provider role in the provider scope, and as the
// tenant role through the Tenant's view.
type AccessReview struct {
	provider *db.ProviderPool
	tenants  *db.TenantPool
	id       func() (id.UUID, error)
}

// NewAccessReview builds the review on the provider pool and the tenant pool.
func NewAccessReview(provider *db.ProviderPool, tenants *db.TenantPool) (*AccessReview, error) {
	switch {
	case provider == nil:
		return nil, errors.New("authority: a provider pool is required")
	case tenants == nil:
		return nil, errors.New("authority: a tenant pool is required")
	}
	return &AccessReview{provider: provider, tenants: tenants, id: id.NewV7}, nil
}

const accessColumns = `SELECT access_id::text, actor_id::text, authority, coalesce(activation_id::text, ''),
       coalesce(tenant_id::text, ''), coalesce(operation, ''), correlation_id::text, reason, occurred_at`

// accessListStatement is one keyset page of the whole record, for a provider.
const accessListStatement = accessColumns + `
FROM audit.privileged_access
WHERE ($1::uuid IS NULL OR actor_id = $1::uuid)
  AND ($2::uuid IS NULL OR tenant_id = $2::uuid)
  AND ($3::uuid IS NULL OR correlation_id = $3::uuid)
  AND ($4::text = '' OR authority = $4::text)
  AND ($5::timestamptz IS NULL OR occurred_at >= $5::timestamptz)
  AND ($6::timestamptz IS NULL OR occurred_at < $6::timestamptz)
  AND ($7::uuid IS NULL OR access_id > $7::uuid)
ORDER BY access_id
LIMIT $8`

// tenantAccessListStatement is one keyset page of the provider access that names the bound Tenant.
// The view is the boundary: it reads the Tenant the transaction is bound to and leaves consumer rows out, so
// nothing here names a Tenant (TDD-organization-control-001 §Privileged Access Review).
const tenantAccessListStatement = accessColumns + `
FROM audit.tenant_provider_access
WHERE ($1::text = '' OR authority = $1::text)
  AND ($2::timestamptz IS NULL OR occurred_at >= $2::timestamptz)
  AND ($3::timestamptz IS NULL OR occurred_at < $3::timestamptz)
  AND ($4::uuid IS NULL OR access_id > $4::uuid)
ORDER BY access_id
LIMIT $5`

// recordReviewStatement records a review with the counts its period holds now, in one statement,
// and only when the period has ended by the database's clock: a `to` ahead of it inserts nothing,
// rather than tripping the period check as an internal error. It answers one row always, the count
// inserted first, so no-row is not an error this package would need the driver's sentinel to tell.
const recordReviewStatement = `WITH counted AS (
    SELECT count(*) AS accesses, count(*) FILTER (WHERE authority = 'emergency') AS emergency
    FROM audit.privileged_access
    WHERE actor_id = $2 AND authority <> 'consumer'
      AND occurred_at >= $3::timestamptz AND occurred_at < $4::timestamptz),
inserted AS (
    INSERT INTO audit.privileged_access_review
        (review_id, actor_id, period_from, period_to, outcome, statement, accesses, emergency_accesses,
         reviewed_by, correlation_id)
    SELECT $1, $2, $3::timestamptz, $4::timestamptz, $5, $6, counted.accesses, counted.emergency, $7, $8
    FROM counted
    WHERE $4::timestamptz <= now()
    RETURNING accesses, emergency_accesses, reviewed_at)
SELECT count(*), coalesce(max(accesses), 0), coalesce(max(emergency_accesses), 0), max(reviewed_at)
FROM inserted`

const reviewColumns = `SELECT review_id::text, actor_id::text, period_from, period_to, outcome, statement,
       accesses, emergency_accesses, reviewed_by::text, reviewed_at
FROM audit.privileged_access_review`

const reviewListStatement = reviewColumns + `
WHERE ($1::uuid IS NULL OR actor_id = $1::uuid)
  AND ($2::uuid IS NULL OR reviewed_by = $2::uuid)
  AND ($3::uuid IS NULL OR review_id > $3::uuid)
ORDER BY review_id
LIMIT $4`

// unreviewedStatement is each Principal's provider access that no review of it covers, oldest
// first. Consumer rows are never due: a consumer is a workload, reviewed through its owner.
const unreviewedStatement = `SELECT a.actor_id::text, count(*), count(*) FILTER (WHERE a.authority = 'emergency'),
       min(a.occurred_at)
FROM audit.privileged_access a
WHERE a.authority <> 'consumer'
  AND NOT EXISTS (
        SELECT 1 FROM audit.privileged_access_review r
        WHERE r.actor_id = a.actor_id
          AND r.period_from <= a.occurred_at AND a.occurred_at < r.period_to)
GROUP BY a.actor_id
ORDER BY min(a.occurred_at), a.actor_id`

// providerAuthorities are the authority values a filter may name. The Tenant's view holds no
// consumer row, so its list takes the first three.
var providerAuthorities = map[string]bool{
	db.AuthorityEmergency: true, db.AuthorityActivation: true, db.AuthorityEligible: true,
}

func checkWindow(from, to *time.Time) error {
	if from != nil && to != nil && !to.After(*from) {
		return fmt.Errorf("%w: to must be after from", ErrAccessQueryInvalid)
	}
	return nil
}

// List reads one page of the whole record, for a provider in force. The read is itself a provider
// access, recorded with the caller's reason before the page is read.
func (a *AccessReview) List(ctx context.Context, query AccessQuery, reason string) (AccessPage, error) {
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return AccessPage{}, fmt.Errorf("%w: %w", ErrAccessQueryInvalid, err)
	}
	if query.Authority != "" && !providerAuthorities[query.Authority] && query.Authority != db.AuthorityConsumer {
		return AccessPage{}, fmt.Errorf("%w: authority must be emergency, activation, eligible or consumer",
			ErrAccessQueryInvalid)
	}
	if err := checkWindow(query.From, query.To); err != nil {
		return AccessPage{}, err
	}
	var page AccessPage
	err = db.WithProviderScope(ctx, a.provider, reason, func(ctx context.Context, tx db.Tx) error {
		page, err = readAccessPage(ctx, tx, limit, accessListStatement,
			db.Keyset(query.Actor), db.Keyset(query.Tenant), db.Keyset(query.Correlation), query.Authority,
			query.From, query.To, db.Keyset(query.After), limit+1)
		return err
	})
	if err != nil {
		return AccessPage{}, err
	}
	return page, nil
}

// TenantList reads one page of the provider access that names the caller's Tenant, for its Tenant
// administrator. The Tenant is the scope's: a query naming a Tenant, an actor or a correlation is
// refused, and the view reads the binding. It records nothing: a Tenant reading its own record is not
// provider access.
func (a *AccessReview) TenantList(ctx context.Context, query AccessQuery) (AccessPage, error) {
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return AccessPage{}, fmt.Errorf("%w: %w", ErrAccessQueryInvalid, err)
	}
	switch {
	case !query.Tenant.IsNil() || !query.Actor.IsNil() || !query.Correlation.IsNil():
		return AccessPage{}, fmt.Errorf("%w: a Tenant's list takes the authority and the window alone",
			ErrAccessQueryInvalid)
	case query.Authority != "" && !providerAuthorities[query.Authority]:
		return AccessPage{}, fmt.Errorf("%w: authority must be emergency, activation or eligible",
			ErrAccessQueryInvalid)
	}
	if err := checkWindow(query.From, query.To); err != nil {
		return AccessPage{}, err
	}
	var page AccessPage
	err = db.WithTenantRead(ctx, a.tenants, func(ctx context.Context, tx db.Tx) error {
		page, err = readAccessPage(ctx, tx, limit, tenantAccessListStatement,
			query.Authority, query.From, query.To, db.Keyset(query.After), limit+1)
		return err
	})
	if err != nil {
		return AccessPage{}, err
	}
	return page, nil
}

func readAccessPage(ctx context.Context, tx db.Tx, limit int, statement string, args ...any) (AccessPage, error) {
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return AccessPage{}, fmt.Errorf("authority: list privileged access: %w", err)
	}
	defer rows.Close()
	page := AccessPage{Accesses: []Access{}}
	for rows.Next() {
		var (
			access                                           Access
			accessID, actor, activation, tenant, correlation string
		)
		if err := rows.Scan(&accessID, &actor, &access.Authority, &activation, &tenant, &access.Operation,
			&correlation, &access.Reason, &access.OccurredAt); err != nil {
			return AccessPage{}, fmt.Errorf("authority: list privileged access: %w", err)
		}
		if access.ID, err = id.Parse(accessID); err != nil {
			return AccessPage{}, err
		}
		if access.Actor, err = id.Parse(actor); err != nil {
			return AccessPage{}, err
		}
		if access.Correlation, err = id.Parse(correlation); err != nil {
			return AccessPage{}, err
		}
		if access.Activation, err = optionalID(activation); err != nil {
			return AccessPage{}, err
		}
		if access.Tenant, err = optionalID(tenant); err != nil {
			return AccessPage{}, err
		}
		page.Accesses = append(page.Accesses, access)
	}
	if err := rows.Err(); err != nil {
		return AccessPage{}, fmt.Errorf("authority: list privileged access: %w", err)
	}
	if len(page.Accesses) > limit {
		page.Accesses = page.Accesses[:limit]
		next := page.Accesses[limit-1].ID
		page.Next = &next
	}
	return page, nil
}

func optionalID(raw string) (*id.UUID, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := id.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// Record records a review of another provider's access over a period. The reviewer is the scope's
// actor, never the request's, and a review of its own access is refused here and by the table's
// separation check (AC-5). The attempt is recorded before the transaction, as every provider access
// is, so a refused self-review still leaves evidence that it was asked for.
func (a *AccessReview) Record(ctx context.Context, req ReviewRequest) (Review, error) {
	req.Statement = strings.TrimSpace(req.Statement)
	switch {
	case req.Actor.IsNil():
		return Review{}, fmt.Errorf("%w: actor_id is required", ErrReviewInvalid)
	case req.From.IsZero() || req.To.IsZero():
		return Review{}, fmt.Errorf("%w: from and to are required", ErrReviewInvalid)
	case !req.To.After(req.From):
		return Review{}, fmt.Errorf("%w: to must be after from", ErrReviewInvalid)
	case req.Outcome != OutcomeAppropriate && req.Outcome != OutcomeEscalated:
		return Review{}, fmt.Errorf("%w: outcome must be appropriate or escalated", ErrReviewInvalid)
	case req.Statement == "":
		return Review{}, db.ErrReasonRequired
	}
	reviewID, err := a.id()
	if err != nil {
		return Review{}, fmt.Errorf("authority: mint review identifier: %w", err)
	}
	review := Review{ID: reviewID, Actor: req.Actor, From: req.From.UTC(), To: req.To.UTC(),
		Outcome: req.Outcome, Statement: req.Statement}
	err = db.WithProviderScope(ctx, a.provider, req.Statement, func(ctx context.Context, tx db.Tx) error {
		scope, ok := db.ScopeFrom(ctx)
		if !ok {
			return db.ErrNoScope
		}
		if scope.Actor() == req.Actor {
			return ErrSelfReview
		}
		review.ReviewedBy = scope.Actor()
		var (
			inserted   int
			reviewedAt *time.Time
		)
		if err := tx.QueryRow(ctx, recordReviewStatement, reviewID.String(), req.Actor.String(),
			review.From, review.To, req.Outcome, req.Statement, scope.Actor().String(),
			scope.Correlation().String()).
			Scan(&inserted, &review.Accesses, &review.EmergencyAccesses, &reviewedAt); err != nil {
			return fmt.Errorf("authority: record privileged-access review: %w", err)
		}
		if inserted == 0 || reviewedAt == nil {
			return fmt.Errorf("%w: to is in the future", ErrReviewInvalid)
		}
		review.ReviewedAt = *reviewedAt
		// The response is recorded with the review, so a retry with the same key is answered this
		// review rather than recording a second (TDD-organization-control-003 §The Response Is
		// Recorded with the Effect).
		return db.Respond(ctx, tx, review)
	})
	if err != nil {
		return Review{}, err
	}
	return review, nil
}

// Reviews reads one page of the recorded reviews, for a provider in force.
func (a *AccessReview) Reviews(ctx context.Context, query ReviewQuery, reason string) (ReviewPage, error) {
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return ReviewPage{}, fmt.Errorf("%w: %w", ErrAccessQueryInvalid, err)
	}
	page := ReviewPage{Reviews: []Review{}}
	err = db.WithProviderScope(ctx, a.provider, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, reviewListStatement, db.Keyset(query.Actor), db.Keyset(query.ReviewedBy),
			db.Keyset(query.After), limit+1)
		if err != nil {
			return fmt.Errorf("authority: list privileged-access reviews: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				review                 Review
				reviewID, actor, revBy string
			)
			if err := rows.Scan(&reviewID, &actor, &review.From, &review.To, &review.Outcome,
				&review.Statement, &review.Accesses, &review.EmergencyAccesses, &revBy,
				&review.ReviewedAt); err != nil {
				return fmt.Errorf("authority: list privileged-access reviews: %w", err)
			}
			if review.ID, err = id.Parse(reviewID); err != nil {
				return err
			}
			if review.Actor, err = id.Parse(actor); err != nil {
				return err
			}
			if review.ReviewedBy, err = id.Parse(revBy); err != nil {
				return err
			}
			page.Reviews = append(page.Reviews, review)
		}
		return rows.Err()
	})
	if err != nil {
		return ReviewPage{}, err
	}
	if len(page.Reviews) > limit {
		page.Reviews = page.Reviews[:limit]
		next := page.Reviews[limit-1].ID
		page.Next = &next
	}
	return page, nil
}

func readUnreviewed(ctx context.Context, tx db.Tx, now time.Time) ([]Unreviewed, error) {
	rows, err := tx.Query(ctx, unreviewedStatement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Unreviewed{}
	for rows.Next() {
		var (
			u     Unreviewed
			actor string
		)
		if err := rows.Scan(&actor, &u.Unreviewed, &u.Emergency, &u.OldestAt); err != nil {
			return nil, err
		}
		if u.Actor, err = id.Parse(actor); err != nil {
			return nil, err
		}
		u.DueAt = u.OldestAt.Add(ReviewDue)
		u.Overdue = now.After(u.DueAt)
		out = append(out, u)
	}
	return out, rows.Err()
}

// Unreviewed reads the unreviewed-access report for a provider, in the provider scope.
func (a *AccessReview) Unreviewed(ctx context.Context, reason string, now time.Time) ([]Unreviewed, error) {
	var out []Unreviewed
	err := db.WithProviderScope(ctx, a.provider, reason, func(ctx context.Context, tx db.Tx) error {
		var err error
		out, err = readUnreviewed(ctx, tx, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("authority: read unreviewed provider access: %w", err)
	}
	return out, nil
}

// UnreviewedAccess reads the same report on the raw connections, for the scheduled maintenance stage,
// which has no caller and so no access record to write.
func (r *Reader) UnreviewedAccess(ctx context.Context, now time.Time) ([]Unreviewed, error) {
	var out []Unreviewed
	err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		out, err = readUnreviewed(ctx, tx, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("authority: read unreviewed provider access: %w", err)
	}
	return out, nil
}
