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

	// State is the authoritative Membership the consumer repairs toward, in the shape of a
	// Membership event's payload, so a consumer applies it by the rule it applies that event with: a
	// higher version replaces a lower one. Null when authority holds no Membership by this identifier,
	// which tells the consumer to remove the row.
	//
	// It is what TDD-organization-control-002 §Reconciliation calls republishing. The findings
	// carried versions alone until this, which a consumer cannot apply, so every sweep that found
	// something dead-lettered at the consumer as poison.
	State *RepairedState `json:"state"`
}

// RepairedState is one Membership as authority holds it.
type RepairedState struct {
	MembershipID          id.UUID  `json:"membership_id"`
	PrincipalID           id.UUID  `json:"principal_id"`
	TenantID              id.UUID  `json:"tenant_id"`
	WorkspaceID           *id.UUID `json:"workspace_id"`
	MembershipStatus      string   `json:"membership_status"`
	MembershipVersion     int64    `json:"membership_version"`
	TenantSecurityVersion int64    `json:"tenant_security_version"`
}

// ReportedRow is one context a consumer says it is projecting.
//
// Tagged like every other field on this surface. Untagged, the reconcile route accepted only the Go
// field names, so a report written as membership_id matched nothing and was refused as an unknown field.
type ReportedRow struct {
	MembershipID      id.UUID `json:"membership_id"`
	MembershipVersion int64   `json:"membership_version"`
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
       coalesce(m.workspace_id::text, ''),
       m.status,
       m.membership_version,
       t.tenant_security_version
FROM membership.membership m
JOIN tenant.tenant t ON t.tenant_id = m.tenant_id
WHERE m.status = 'active'`

// withdrawnStatement reads, for identifiers a consumer projects and authority does not hold as
// active, whatever authority does hold: a suspended or revoked Membership, whose state the consumer
// then applies. An identifier absent here is one authority never granted, and the consumer removes it.
const withdrawnStatement = `SELECT m.membership_id::text,
       m.principal_id::text,
       m.tenant_id::text,
       coalesce(m.workspace_id::text, ''),
       m.status,
       m.membership_version,
       t.tenant_security_version
FROM membership.membership m
JOIN tenant.tenant t ON t.tenant_id = m.tenant_id
WHERE m.membership_id::text = ANY ($1::text[])`

// Reconcile compares authority against a report and returns the differences, most severe first.
//
// It repairs in one direction. The result says what authority holds; nothing here writes a
// consumer's value into authority, on any classification, including `extra`.
func (r *Reconciler) Reconcile(ctx context.Context, report Report) (Result, error) {
	if report.ConsumerID == "" {
		return Result{}, fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	}
	if report.Mark <= 0 {
		return Result{}, ErrReportMarkRequired
	}

	result := Result{ConsumerID: report.ConsumerID, Mark: report.Mark, RunAt: r.now().UTC()}

	projected := make(map[id.UUID]int64, len(report.Rows))
	for _, row := range report.Rows {
		projected[row.MembershipID] = row.MembershipVersion
	}

	// authority is the active set; withdrawn is what authority holds for a projected identifier it
	// does not hold as active. Both are read in the one snapshot, so a repair describes one instant.
	authority := map[id.UUID]RepairedState{}
	withdrawn := map[id.UUID]RepairedState{}

	if err := db.WithProviderSnapshot(ctx, r.pool,
		"reconcile projection for "+report.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			var consumer Consumer
			if err := load(ctx, tx, report.ConsumerID, &consumer); err != nil {
				return err
			}
			if err := readStates(ctx, tx, authoritativeStatement, authority); err != nil {
				return fmt.Errorf("projection: read authoritative set: %w", err)
			}

			var unheld []string
			for membershipID := range projected {
				if _, held := authority[membershipID]; !held {
					unheld = append(unheld, membershipID.String())
				}
			}
			if len(unheld) == 0 {
				return nil
			}
			if err := readStates(ctx, tx, withdrawnStatement, withdrawn, unheld); err != nil {
				return fmt.Errorf("projection: read withdrawn Memberships: %w", err)
			}
			return nil
		}); err != nil {
		return Result{}, err
	}

	for membershipID, auth := range authority {
		state := auth
		reportedVersion, present := projected[membershipID]
		switch {
		case !present:
			result.Findings = append(result.Findings, Finding{
				Classification: ClassMissing, MembershipID: membershipID,
				TenantID: auth.TenantID, PrincipalID: auth.PrincipalID,
				AuthoritativeVersion: auth.MembershipVersion, State: &state,
			})
		case reportedVersion != auth.MembershipVersion:
			result.Findings = append(result.Findings, Finding{
				Classification: ClassMismatch, MembershipID: membershipID,
				TenantID: auth.TenantID, PrincipalID: auth.PrincipalID,
				AuthoritativeVersion: auth.MembershipVersion, ProjectedVersion: reportedVersion, State: &state,
			})
		}
	}

	for membershipID, reportedVersion := range projected {
		if _, granted := authority[membershipID]; granted {
			continue
		}
		finding := Finding{Classification: ClassExtra, MembershipID: membershipID, ProjectedVersion: reportedVersion}
		// Suspended or revoked in authority: the consumer applies the withdrawal. Unknown to
		// authority: State stays nil, and the consumer removes the row.
		if held, ok := withdrawn[membershipID]; ok {
			state := held
			finding.TenantID, finding.PrincipalID = held.TenantID, held.PrincipalID
			finding.AuthoritativeVersion, finding.State = held.MembershipVersion, &state
		}
		result.Findings = append(result.Findings, finding)
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

// readStates reads Membership rows in authoritativeStatement's column order into into.
func readStates(ctx context.Context, tx db.Tx, statement string, into map[id.UUID]RepairedState, args ...any) error {
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var rawMembership, rawPrincipal, rawTenant, rawWorkspace string
		var state RepairedState
		if err := rows.Scan(&rawMembership, &rawPrincipal, &rawTenant, &rawWorkspace,
			&state.MembershipStatus, &state.MembershipVersion, &state.TenantSecurityVersion); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if state.MembershipID, err = id.Parse(rawMembership); err != nil {
			return fmt.Errorf("stored membership id %q: %w", rawMembership, err)
		}
		if state.PrincipalID, err = id.Parse(rawPrincipal); err != nil {
			return fmt.Errorf("stored principal id %q: %w", rawPrincipal, err)
		}
		if state.TenantID, err = id.Parse(rawTenant); err != nil {
			return fmt.Errorf("stored tenant id %q: %w", rawTenant, err)
		}
		if rawWorkspace != "" {
			workspace, err := id.Parse(rawWorkspace)
			if err != nil {
				return fmt.Errorf("stored workspace id %q: %w", rawWorkspace, err)
			}
			state.WorkspaceID = &workspace
		}
		into[state.MembershipID] = state
	}
	return rows.Err()
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

// recordReconciliation stamps the consumer with the run, whatever it found, so its reconciliation
// age is a fact rather than an inference from the event stream. A retired consumer matches nothing:
// its record stays as it was when it was retired.
const recordReconciliation = `UPDATE projection.consumer
SET last_reconciled_at = $2, last_reconciled_mark = $3, last_reconciled_findings = $4,
    last_reconciled_extra_findings = $5
WHERE consumer_id = $1 AND retired_at IS NULL`

// PublishReconciled records the sweep against its consumer and, when it found something, appends
// the repair event, in one transaction.
//
// Recorded on every sweep, clean ones included: a clean sweep is the evidence that the consumer's
// copy was compared and agreed, and it is what the projection health screen reads as the consumer's
// reconciliation age (TDD-organization-control-002 1.13.0 §Reconciliation). Until 1.13.0 nothing
// recorded a run, so the age could not be served.
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
		return db.WithProviderScope(ctx, r.pool,
			"record reconciliation for "+result.ConsumerID,
			func(ctx context.Context, tx db.Tx) error {
				return recordRun(ctx, tx, result)
			})
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
			if err := recordRun(ctx, tx, result); err != nil {
				return err
			}
			if err := outbox.Append(ctx, tx, aggregate, envelope); err != nil {
				return fmt.Errorf("projection: append reconciled event: %w", err)
			}
			return nil
		})
}

func recordRun(ctx context.Context, tx db.Tx, result Result) error {
	if _, err := tx.Exec(ctx, recordReconciliation, result.ConsumerID, result.RunAt, result.Mark,
		len(result.Findings), len(result.SecurityFindings())); err != nil {
		return fmt.Errorf("projection: record reconciliation: %w", err)
	}
	return nil
}
