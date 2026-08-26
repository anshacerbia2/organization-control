package projection

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/system"
)

// Classification is what reconciliation found about one context.
type Classification string

const (
	// ClassMissing means authority grants a context the consumer does not project. The consumer is
	// denying access it should allow, which presents as a support ticket.
	ClassMissing Classification = "missing"

	// ClassExtra means the consumer projects a context authority does not grant.
	//
	// Escalated as a potential privilege escalation rather than filed as a data-quality issue.
	// Reaching this state requires either a defect in the projection path or a write outside it,
	// and in both cases somebody currently holds access nothing granted.
	ClassExtra Classification = "extra"

	// ClassMismatch means both sides know the context and disagree about its version. Repaired by
	// republishing the authoritative value; the consumer's version is never adopted.
	ClassMismatch Classification = "mismatch"
)

// Security reports whether a finding is a security event rather than a data-quality one.
func (c Classification) Security() bool { return c == ClassExtra }

// Finding is one difference between authority and a consumer's report.
type Finding struct {
	Classification Classification `json:"classification"`
	MembershipID   id.UUID        `json:"membership_id"`
	TenantID       id.UUID        `json:"tenant_id"`
	PrincipalID    id.UUID        `json:"principal_id"`

	// AuthoritativeVersion is the version authority holds, or 0 when authority holds nothing.
	AuthoritativeVersion int64 `json:"authoritative_version"`

	// ProjectedVersion is the version the consumer reported, or 0 when it reported nothing.
	ProjectedVersion int64 `json:"projected_version"`
}

// ReportedRow is one context a consumer says it is projecting.
type ReportedRow struct {
	MembershipID      id.UUID
	MembershipVersion int64
}

// Report is a consumer's account of its own projection at a stated position.
type Report struct {
	ConsumerID string

	// Mark is the position the reported state corresponds to. Required: comparing a report against
	// authority read now would classify every change made since the report as a divergence, and
	// the sweep would manufacture findings out of ordinary progress.
	Mark int64

	Rows []ReportedRow
}

// Result is one reconciliation sweep.
type Result struct {
	ConsumerID string    `json:"consumer_id"`
	Mark       int64     `json:"mark"`
	RunAt      time.Time `json:"run_at"`
	Findings   []Finding `json:"findings"`
}

// SecurityFindings returns the subset that must be escalated rather than queued for repair.
func (r Result) SecurityFindings() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Classification.Security() {
			out = append(out, f)
		}
	}
	return out
}

// ErrReportMarkRequired reports a sweep requested against an unpositioned report.
var ErrReportMarkRequired = errors.New("projection: a reported projection must state its position")

// Reconciler compares authority against what a consumer reported.
type Reconciler struct {
	pool  *db.ProviderPool
	now   func() time.Time
	newID func() (id.UUID, error)
}

// NewReconciler constructs the reconciler.
func NewReconciler(pool *db.ProviderPool) (*Reconciler, error) {
	if pool == nil {
		return nil, errors.New("projection: a provider-scoped pool is required")
	}
	return &Reconciler{pool: pool, now: time.Now, newID: id.NewV7}, nil
}

// authoritativeStatement reads the active set the same way the snapshot does.
//
// Identical predicate on purpose. If reconciliation read a different set from the snapshot, every
// row in the difference would be reported as a finding forever, and the sweep would be a generator
// of false positives rather than a detector of real ones.
const authoritativeStatement = `SELECT m.membership_id::text,
       m.principal_id::text,
       m.tenant_id::text,
       m.membership_version
FROM membership.membership m
JOIN tenant.tenant t ON t.tenant_id = m.tenant_id
WHERE m.status = 'active'`

// Reconcile compares authority against a report and returns the differences, most severe first.
//
// It repairs in one direction. The result says what authority holds; nothing here writes a
// consumer's value into authority, on any classification, including `extra`.
func (r *Reconciler) Reconcile(ctx context.Context, report Report) (Result, error) {
	if report.ConsumerID == "" {
		return Result{}, errors.New("projection: a consumer identifier is required")
	}
	if report.Mark <= 0 {
		return Result{}, ErrReportMarkRequired
	}

	result := Result{ConsumerID: report.ConsumerID, Mark: report.Mark, RunAt: r.now().UTC()}

	type authoritative struct {
		principal id.UUID
		tenant    id.UUID
		version   int64
	}
	authority := map[id.UUID]authoritative{}

	if err := db.WithProviderSnapshot(ctx, r.pool,
		"reconcile projection for "+report.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			var consumer Consumer
			if err := load(ctx, tx, report.ConsumerID, &consumer); err != nil {
				return err
			}

			rows, err := tx.Query(ctx, authoritativeStatement)
			if err != nil {
				return fmt.Errorf("projection: read authoritative set: %w", err)
			}
			defer rows.Close()

			for rows.Next() {
				var rawMembership, rawPrincipal, rawTenant string
				var version int64
				if err := rows.Scan(&rawMembership, &rawPrincipal, &rawTenant, &version); err != nil {
					return fmt.Errorf("projection: scan authoritative row: %w", err)
				}
				membershipID, err := id.Parse(rawMembership)
				if err != nil {
					return fmt.Errorf("projection: stored membership id %q: %w", rawMembership, err)
				}
				principalID, err := id.Parse(rawPrincipal)
				if err != nil {
					return fmt.Errorf("projection: stored principal id %q: %w", rawPrincipal, err)
				}
				tenantID, err := id.Parse(rawTenant)
				if err != nil {
					return fmt.Errorf("projection: stored tenant id %q: %w", rawTenant, err)
				}
				authority[membershipID] = authoritative{principal: principalID, tenant: tenantID, version: version}
			}
			return rows.Err()
		}); err != nil {
		return Result{}, err
	}

	projected := make(map[id.UUID]int64, len(report.Rows))
	for _, row := range report.Rows {
		projected[row.MembershipID] = row.MembershipVersion
	}

	for membershipID, auth := range authority {
		reportedVersion, present := projected[membershipID]
		switch {
		case !present:
			result.Findings = append(result.Findings, Finding{
				Classification: ClassMissing, MembershipID: membershipID,
				TenantID: auth.tenant, PrincipalID: auth.principal,
				AuthoritativeVersion: auth.version,
			})
		case reportedVersion != auth.version:
			result.Findings = append(result.Findings, Finding{
				Classification: ClassMismatch, MembershipID: membershipID,
				TenantID: auth.tenant, PrincipalID: auth.principal,
				AuthoritativeVersion: auth.version, ProjectedVersion: reportedVersion,
			})
		}
	}

	for membershipID, reportedVersion := range projected {
		if _, granted := authority[membershipID]; !granted {
			result.Findings = append(result.Findings, Finding{
				Classification: ClassExtra, MembershipID: membershipID,
				ProjectedVersion: reportedVersion,
			})
		}
	}

	// Sorted, and security findings first. Map iteration order is deliberately random in Go, so an
	// unsorted result would make two sweeps over identical state return different output — which
	// breaks the idempotence the design requires and makes any diff of two runs meaningless. The
	// severity ordering is the same reason a runbook exists: whoever reads the first line of a
	// sweep should read the privilege escalation, not the first row a hash bucket happened to hold.
	sort.SliceStable(result.Findings, func(i, j int) bool {
		a, b := result.Findings[i], result.Findings[j]
		if a.Classification.Security() != b.Classification.Security() {
			return a.Classification.Security()
		}
		if a.Classification != b.Classification {
			return a.Classification < b.Classification
		}
		return a.MembershipID.String() < b.MembershipID.String()
	})

	return result, nil
}

// ReconciledEventType is published once per sweep that found something.
//
// DEPARTURE from TDD-organization-control-002 §"Published Events", which names it
// `com.scnehaux.organization.projection.reconciled`. That has five segments and
// `event.ParseType` requires six or seven — the fifth segment carries the class of the event, and
// a five-segment name has no room for one. The design's name is not merely rejected by the
// validator; it cannot express the classification every other type in this estate carries.
//
// `repair` is the class. It is deliberately not `security`, which is the segment that routes an
// event to the reserved dispatch lane: a sweep corrects a divergence that has already been
// delivered, so putting a large sweep on that lane would delay exactly the live revocations the
// lane exists for.
const ReconciledEventType = "com.scnehaux.organization.projection.repair.reconciled"

// PublishReconciled appends the repair event for a sweep.
//
// One event per sweep rather than one per finding. The repair is a set operation — a consumer
// applies the authoritative values it was told about — and a finding-per-event stream would let a
// consumer apply half a sweep and report itself reconciled.
//
// The standard lane. A reconciliation sweep is a correction of an already-delivered divergence, so
// it does not compete with a live revocation for the priority lane; putting it there would let a
// large sweep delay exactly the events the lane is reserved for.
func (r *Reconciler) PublishReconciled(ctx context.Context, result Result) error {
	if len(result.Findings) == 0 {
		return nil
	}

	eventType, err := event.ParseType(ReconciledEventType)
	if err != nil {
		return fmt.Errorf("projection: reconciled event type: %w", err)
	}

	// Each sweep is its own aggregate, and ordering between sweeps is carried by `mark` in the
	// payload rather than by partitioning.
	//
	// The alternative was to derive a stable aggregate identifier from the consumer name so a
	// consumer's sweeps shared a partition. That buys per-consumer ordering this design does not
	// need: a sweep is a complete set at a stated position, so a consumer accepts the highest mark
	// it has seen and discards an older one — the same rule that already governs every Membership
	// and Tenant event here. Ordering that nothing relies on is a constraint to maintain, not a
	// guarantee to gain.
	aggregate, err := r.newID()
	if err != nil {
		return fmt.Errorf("projection: mint sweep identifier: %w", err)
	}

	envelope, err := event.New(system.Source, eventType, result.RunAt, result)
	if err != nil {
		return fmt.Errorf("projection: build reconciled envelope: %w", err)
	}

	return db.WithProviderScope(ctx, r.pool,
		"publish reconciliation for "+result.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			if err := outbox.Append(ctx, tx, aggregate, envelope); err != nil {
				return fmt.Errorf("projection: append reconciled event: %w", err)
			}
			return nil
		})
}
