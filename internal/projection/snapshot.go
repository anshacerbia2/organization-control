package projection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// Row is one projected context: the complete set of fields a consumer needs to decide whether a
// token may assert this context, and nothing more.
//
// The same fields the Membership and Tenant events carry, deliberately. A snapshot row and an
// event describing the same Membership must be comparable field by field, because the consumer's
// only ordering rule is version comparison — and it cannot compare a field one side omits.
type Row struct {
	MembershipID id.UUID  `json:"membership_id"`
	PrincipalID  id.UUID  `json:"principal_id"`
	TenantID     id.UUID  `json:"tenant_id"`
	WorkspaceID  *id.UUID `json:"workspace_id"`
	SubjectType  string   `json:"subject_type"`

	MembershipStatus  string `json:"membership_status"`
	MembershipVersion int64  `json:"membership_version"`

	TenantStatus          string `json:"tenant_status"`
	TenantSecurityVersion int64  `json:"tenant_security_version"`
}

// Page is one page of a snapshot.
type Page struct {
	// HighWaterMark is the outbox position this snapshot corresponds to. Identical on every page
	// of one snapshot, because every page is read from one database snapshot.
	HighWaterMark int64 `json:"high_water_mark"`

	// TakenAt is when the snapshot transaction began. A consumer measures its own freshness from
	// this rather than from when the page arrived, so a slow page does not read as stale data.
	TakenAt time.Time `json:"taken_at"`

	Rows []Row `json:"rows"`

	// Cursor is the opaque continuation. Empty when this is the last page.
	//
	// Keyed on membership_id rather than an offset. OFFSET re-reads and discards the rows it
	// skips, which turns a paged snapshot into a quadratic scan, and it is only stable under a
	// consistent read — the cursor stays correct even if a caller resumes against a later
	// snapshot, where it would simply return a different but internally consistent page.
	Cursor string `json:"cursor,omitempty"`
}

var (
	// ErrPageSize reports a page size outside the permitted range.
	ErrPageSize = errors.New("projection: page size is outside the permitted range")

	// ErrCursor reports an unparseable continuation cursor.
	ErrCursor = errors.New("projection: cursor is not a valid continuation")
)

const (
	// DefaultPageSize matches ORGANIZATION_SNAPSHOT_PAGE_SIZE.
	DefaultPageSize = 1000

	// MaxPageSize is admission control, not a preference. A consumer that asks for the whole
	// estate in one page turns a snapshot into an unbounded allocation on the publisher, and the
	// publisher is shared — so the cost of one consumer's request is capped before it is served.
	MaxPageSize = 5000
)

// SnapshotRequest asks for one page.
type SnapshotRequest struct {
	// ConsumerID must name a registered consumer. A consumer that has not registered receives no
	// projection at all.
	ConsumerID string

	PageSize int

	// Cursor continues a previous page. Empty starts a new snapshot.
	Cursor string

	// Mark carries the high-water mark of the snapshot being continued. Required with a cursor:
	// each page is a separate transaction and therefore a separate database snapshot, so without
	// it page two would silently report its own mark and a consumer stitching the pages together
	// would hold rows from two instants under one watermark.
	//
	// A pointer, because zero is a real mark. An estate that has published nothing has mark 0, and
	// a plain int64 cannot tell that apart from a caller who forgot the field — which would refuse
	// the second page of the very first snapshot a deployment ever takes.
	Mark *int64
}

// Publisher produces snapshots and records the mark a consumer bootstrapped from.
type Publisher struct {
	pool     *db.ProviderPool
	registry *Registry
	now      func() time.Time
}

// NewPublisher constructs the publisher.
func NewPublisher(pool *db.ProviderPool, registry *Registry) (*Publisher, error) {
	if pool == nil {
		return nil, errors.New("projection: a provider-scoped pool is required")
	}
	if registry == nil {
		return nil, errors.New("projection: a consumer registry is required")
	}
	return &Publisher{pool: pool, registry: registry, now: time.Now}, nil
}

// markStatement reads the outbox position the snapshot corresponds to.
//
// `coalesce(..., 0)` because an estate that has published nothing has no maximum, and a NULL mark
// would be indistinguishable from "no mark taken" at every layer above this one.
const markStatement = `SELECT coalesce(max(sequence), 0) FROM platform.outbox`

// selectRows is the projection itself: active Memberships joined to their Tenant.
//
// Only `active` Memberships appear. A revoked Membership is not a context a token may assert, and
// a consumer that received revoked rows would have to filter them — which is a rule that would
// then live in every consumer instead of here.
//
// The Tenant status travels with each row rather than in a separate collection, because the
// consumer's decision needs both: an active Membership inside a suspended Tenant grants nothing,
// and a consumer holding the two facts separately can apply one and miss the other.
const selectRows = `SELECT m.membership_id::text,
       m.principal_id::text,
       m.tenant_id::text,
       coalesce(m.workspace_id::text, ''),
       m.subject_type,
       m.status,
       m.membership_version,
       t.status,
       t.tenant_security_version
FROM membership.membership m
JOIN tenant.tenant t ON t.tenant_id = m.tenant_id
WHERE m.status = 'active'
  AND ($1 = '' OR m.membership_id > $1::uuid)
ORDER BY m.membership_id
LIMIT $2`

// Snapshot produces one page.
//
// Every page is read inside a read-only REPEATABLE READ transaction, and the mark is taken in the
// same transaction as the rows. Under READ COMMITTED the two reads would see different instants,
// and a Membership committing between them would be absent from the rows while the mark claimed
// its event was already represented — the one outcome the bootstrap contract exists to prevent.
func (p *Publisher) Snapshot(ctx context.Context, req SnapshotRequest) (Page, error) {
	size := req.PageSize
	if size == 0 {
		size = DefaultPageSize
	}
	if size < 0 || size > MaxPageSize {
		return Page{}, fmt.Errorf("%w: %d is not between 1 and %d", ErrPageSize, size, MaxPageSize)
	}
	if req.Cursor != "" {
		if _, err := id.Parse(req.Cursor); err != nil {
			return Page{}, fmt.Errorf("%w: %v", ErrCursor, err)
		}
		if req.Mark == nil {
			return Page{}, fmt.Errorf("%w: continuing a snapshot requires its high-water mark", ErrCursor)
		}
		if *req.Mark < 0 {
			return Page{}, fmt.Errorf("%w: a high-water mark cannot be negative", ErrCursor)
		}
	}

	page := Page{TakenAt: p.now().UTC()}
	if req.Mark != nil {
		page.HighWaterMark = *req.Mark
	}

	if err := db.WithProviderSnapshot(ctx, p.pool,
		"projection snapshot for "+req.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			// The registration check runs inside the snapshot transaction. Checked before it, a
			// consumer deregistered in between would still be served a page.
			var consumer Consumer
			if err := load(ctx, tx, req.ConsumerID, &consumer); err != nil {
				return err
			}

			if req.Cursor == "" {
				if err := tx.QueryRow(ctx, markStatement).Scan(&page.HighWaterMark); err != nil {
					return fmt.Errorf("projection: read high-water mark: %w", err)
				}
			}

			rows, err := tx.Query(ctx, selectRows, req.Cursor, size)
			if err != nil {
				return fmt.Errorf("projection: read rows: %w", err)
			}
			defer rows.Close()

			for rows.Next() {
				var (
					row                                    Row
					rawMembership, rawPrincipal, rawTenant string
					rawWorkspace                           string
				)
				if err := rows.Scan(&rawMembership, &rawPrincipal, &rawTenant, &rawWorkspace,
					&row.SubjectType, &row.MembershipStatus, &row.MembershipVersion,
					&row.TenantStatus, &row.TenantSecurityVersion); err != nil {
					return fmt.Errorf("projection: scan row: %w", err)
				}
				if row.MembershipID, err = id.Parse(rawMembership); err != nil {
					return fmt.Errorf("projection: stored membership id %q: %w", rawMembership, err)
				}
				if row.PrincipalID, err = id.Parse(rawPrincipal); err != nil {
					return fmt.Errorf("projection: stored principal id %q: %w", rawPrincipal, err)
				}
				if row.TenantID, err = id.Parse(rawTenant); err != nil {
					return fmt.Errorf("projection: stored tenant id %q: %w", rawTenant, err)
				}
				if rawWorkspace != "" {
					workspace, err := id.Parse(rawWorkspace)
					if err != nil {
						return fmt.Errorf("projection: stored workspace id %q: %w", rawWorkspace, err)
					}
					row.WorkspaceID = &workspace
				}
				page.Rows = append(page.Rows, row)
			}
			return rows.Err()
		}); err != nil {
		return Page{}, err
	}

	// A full page means there may be more. A short page ends the snapshot, and so does a full page
	// whose successor turns out to be empty — which costs one extra request and avoids reporting
	// "no more rows" from a count that a concurrent snapshot could make wrong.
	if len(page.Rows) == size {
		page.Cursor = page.Rows[len(page.Rows)-1].MembershipID.String()
	}
	return page, nil
}

// Bootstrap records the mark a consumer is bootstrapping from, which is what later permits its
// progress reports.
//
// Separate from Snapshot because the first page can be produced and then lost — a consumer that
// crashed mid-bootstrap must not be recorded as having a snapshot it never applied. The consumer
// calls this once its local model has been replaced.
func (p *Publisher) Bootstrap(ctx context.Context, consumerID string, mark int64) (Consumer, error) {
	if mark < 0 {
		return Consumer{}, fmt.Errorf("%w: a snapshot mark cannot be negative", ErrInvalid)
	}

	var consumer Consumer
	if err := db.WithProviderScope(ctx, p.pool,
		"record projection bootstrap for "+consumerID,
		func(ctx context.Context, tx db.Tx) error {
			if err := load(ctx, tx, consumerID, &consumer); err != nil {
				return err
			}
			// A re-bootstrap is permitted and must move the mark forward only. A lower mark would
			// claim the consumer rebuilt from an older instant than one it already reported
			// progress against, which no sequence of correct operations produces.
			if consumer.SnapshotMark != nil && mark < *consumer.SnapshotMark {
				return fmt.Errorf("%w: bootstrapping at %d below the recorded %d",
					ErrMarkWentBackwards, mark, *consumer.SnapshotMark)
			}
			if _, err := tx.Exec(ctx, recordSnapshotMark, consumerID, mark); err != nil {
				return fmt.Errorf("projection: record snapshot mark: %w", err)
			}
			return nil
		}); err != nil {
		return Consumer{}, err
	}

	recorded := mark
	consumer.SnapshotMark = &recorded
	return consumer, nil
}
