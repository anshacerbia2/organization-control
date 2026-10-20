package authority

// The reporter registry (ADR-ORG-001 §5.11, amended 2026-10-20; TDD-organization-control-001 1.27.0
// §Reporters).
//
// A reporter is a workload Principal this platform registered to report one kind of outcome: the
// provisioning system reports the three provisioning outcomes, and an obligation-owning domain
// resolves the obligations raised against it. It is admitted from this record, never from a role or
// a scope in its token: "Access should also be granted with the least privileges needed to complete
// the task" (NIST SP 800-207 §2.1), and a record here is retired here, without a change to the
// kernel.
//
// Registering and retiring run in the provider scope, so the access record names the acting
// provider and the reason before the work runs. ReporterFor runs on the raw provider connections, as
// ConsumerFor does, because authentication reads it before the request's own scope is known.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

var (
	// ErrReporterInvalid means a registration, a retirement or a list query is incomplete.
	ErrReporterInvalid = errors.New("authority: invalid reporter request")

	// ErrReporterRegistered means the workload Principal is already registered, active or retired. A
	// workload is one reporter for good, as it is one consumer for good: its record is what its
	// owner's review reads.
	ErrReporterRegistered = errors.New("authority: the workload Principal is already registered as a reporter")

	// ErrReporterNotFound means no reporter has the identifier.
	ErrReporterNotFound = errors.New("authority: no reporter has this identifier")

	// ErrReporterRetired means the reporter was retired before.
	ErrReporterRetired = errors.New("authority: the reporter is already retired")

	// ErrReporterVersion means the reporter changed since the caller read it.
	ErrReporterVersion = errors.New("authority: the reporter changed since it was read")
)

// ReportsProvisioning is what the provisioning system reports: POST /v1/provisioning/realized,
// POST /v1/provisioning/failed and POST /v1/offboardings/{offboarding_id}/deprovisioning.
const ReportsProvisioning = "provisioning"

// obligationPrefix starts what an obligation-owning domain reports: obligation:<domain>.
const obligationPrefix = "obligation:"

// domainPattern is a domain as an obligation names one: lowercase, digits and hyphens, as the
// domains raised today are (product, hcm, billing, audit).
var domainPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Reports is what a reporter may report: ReportsProvisioning, or obligation:<domain>.
type Reports string

// Valid reports whether the value is one a reporter may hold.
func (r Reports) Valid() bool {
	if r == ReportsProvisioning {
		return true
	}
	domain, ok := strings.CutPrefix(string(r), obligationPrefix)
	return ok && domainPattern.MatchString(domain)
}

// Provisioning reports whether the reporter reports provisioning outcomes.
func (r Reports) Provisioning() bool { return r == ReportsProvisioning }

// Domain is the obligation domain an obligation reporter resolves for, or "" for any other.
func (r Reports) Domain() string {
	domain, ok := strings.CutPrefix(string(r), obligationPrefix)
	if !ok {
		return ""
	}
	return domain
}

// Reporter is one registration, active or retired.
type Reporter struct {
	ID           id.UUID
	Principal    id.UUID
	Reports      Reports
	Purpose      string
	Version      int64
	RegisteredAt time.Time
	RegisteredBy id.UUID
	RetiredAt    *time.Time
	RetiredBy    *id.UUID
}

// Active reports whether the reporter is admitted.
func (r Reporter) Active() bool { return r.RetiredAt == nil }

// ReporterRegistration asks for a reporter.
type ReporterRegistration struct {
	Principal id.UUID
	Reports   Reports
	Purpose   string
	Reason    string
}

// ReporterQuery is one page of the registry: after a reporter_id, at most Limit, optionally of one
// Reports value and one state, active or retired.
type ReporterQuery struct {
	After   id.UUID
	Limit   int
	Reports string
	State   string
}

// ReporterPage is a page and the cursor of the next, nil on the last.
type ReporterPage struct {
	Reporters []Reporter
	Next      *id.UUID
}

// Reporters registers, retires and lists reporters on the provider pool.
type Reporters struct {
	pool *db.ProviderPool
	id   func() (id.UUID, error)
}

// NewReporters builds the registry on the provider pool.
func NewReporters(pool *db.ProviderPool) (*Reporters, error) {
	if pool == nil {
		return nil, errors.New("authority: a provider pool is required")
	}
	return &Reporters{pool: pool, id: id.NewV7}, nil
}

const reporterColumns = `SELECT reporter_id::text, principal_id::text, reports, purpose, version, registered_at,
       registered_by::text, retired_at, coalesce(retired_by::text, '')
FROM organization.reporter`

const oneReporterStatement = reporterColumns + `
WHERE reporter_id = $1`

const listReportersStatement = reporterColumns + `
WHERE ($1::uuid IS NULL OR reporter_id > $1::uuid)
  AND ($2::text = '' OR reports = $2::text)
  AND ($3::text = '' OR ($3::text = 'active') = (retired_at IS NULL))
ORDER BY reporter_id
LIMIT $4`

// registerReporterStatement inserts the registration unless the Principal is registered already. The
// unique index on principal_id is the guard, against two concurrent registrations as well.
const registerReporterStatement = `WITH inserted AS (
    INSERT INTO organization.reporter (reporter_id, principal_id, reports, purpose, registered_by, register_reason)
    VALUES ($1, $2, $3, $4, $5, $6)
    ON CONFLICT (principal_id) DO NOTHING
    RETURNING reporter_id)
SELECT count(*) FROM inserted`

const retireReporterStatement = `UPDATE organization.reporter
SET retired_at = now(), retired_by = $2, retire_reason = $3, version = version + 1
WHERE reporter_id = $1 AND retired_at IS NULL AND version = $4`

// Register records a reporter for the workload Principal, naming the calling provider and the
// reason. The Principal is not checked against the Identity Control API: this service reaches no
// other domain for authority (ADR-ORG-001 §5.4), and a registration naming a principal_id no
// workload token carries admits nobody.
func (r *Reporters) Register(ctx context.Context, req ReporterRegistration) (Reporter, error) {
	scope, ok := db.ScopeFrom(ctx)
	purpose, reason := strings.TrimSpace(req.Purpose), strings.TrimSpace(req.Reason)
	switch {
	case !ok:
		return Reporter{}, db.ErrNoScope
	case req.Principal.IsNil():
		return Reporter{}, fmt.Errorf("%w: a principal_id is required", ErrReporterInvalid)
	case !req.Reports.Valid():
		return Reporter{}, fmt.Errorf("%w: reports is provisioning or obligation:<domain>, the domain lowercase letters, digits and hyphens",
			ErrReporterInvalid)
	case purpose == "" || len(purpose) > 500:
		return Reporter{}, fmt.Errorf("%w: a purpose of 1 to 500 characters is required", ErrReporterInvalid)
	case reason == "":
		return Reporter{}, fmt.Errorf("%w: a reason is required", ErrReporterInvalid)
	}
	reporterID, err := r.id()
	if err != nil {
		return Reporter{}, fmt.Errorf("authority: mint reporter identifier: %w", err)
	}

	var reporter Reporter
	err = db.WithProviderScope(ctx, r.pool, reason, func(ctx context.Context, tx db.Tx) error {
		var inserted int
		if err := tx.QueryRow(ctx, registerReporterStatement, reporterID.String(), req.Principal.String(),
			string(req.Reports), purpose, scope.Actor().String(), reason).Scan(&inserted); err != nil {
			return err
		}
		if inserted == 0 {
			return ErrReporterRegistered
		}
		if reporter, err = scanReporter(tx.QueryRow(ctx, oneReporterStatement, reporterID.String())); err != nil {
			return err
		}
		return db.Respond(ctx, tx, reporter)
	})
	if err != nil {
		if errors.Is(err, ErrReporterRegistered) {
			return Reporter{}, err
		}
		return Reporter{}, fmt.Errorf("authority: register reporter: %w", err)
	}
	return reporter, nil
}

// Retire ends a reporter's admission from its next request, naming the calling provider and the
// reason. expectedVersion is the version the caller was shown.
func (r *Reporters) Retire(ctx context.Context, reporterID id.UUID, expectedVersion int64, reason string) (Reporter, error) {
	scope, ok := db.ScopeFrom(ctx)
	reason = strings.TrimSpace(reason)
	switch {
	case !ok:
		return Reporter{}, db.ErrNoScope
	case reporterID.IsNil():
		return Reporter{}, fmt.Errorf("%w: a reporter identifier is required", ErrReporterInvalid)
	case expectedVersion < 1:
		return Reporter{}, fmt.Errorf("%w: expected_version is required", ErrReporterInvalid)
	case reason == "":
		return Reporter{}, fmt.Errorf("%w: a reason is required", ErrReporterInvalid)
	}

	var reporter Reporter
	err := db.WithProviderScope(ctx, r.pool, reason, func(ctx context.Context, tx db.Tx) error {
		current, err := scanReporter(tx.QueryRow(ctx, oneReporterStatement+` FOR UPDATE`, reporterID.String()))
		if err != nil {
			return ErrReporterNotFound
		}
		switch {
		case !current.Active():
			return ErrReporterRetired
		case current.Version != expectedVersion:
			return fmt.Errorf("%w: expected %d, stored %d", ErrReporterVersion, expectedVersion, current.Version)
		}
		if _, err := tx.Exec(ctx, retireReporterStatement, reporterID.String(), scope.Actor().String(), reason,
			expectedVersion); err != nil {
			return err
		}
		if reporter, err = scanReporter(tx.QueryRow(ctx, oneReporterStatement, reporterID.String())); err != nil {
			return err
		}
		return db.Respond(ctx, tx, reporter)
	})
	if err != nil {
		for _, sentinel := range []error{ErrReporterNotFound, ErrReporterRetired, ErrReporterVersion} {
			if errors.Is(err, sentinel) {
				return Reporter{}, err
			}
		}
		return Reporter{}, fmt.Errorf("authority: retire reporter: %w", err)
	}
	return reporter, nil
}

// Get reads one reporter, recording the read with the caller's reason.
func (r *Reporters) Get(ctx context.Context, reporterID id.UUID, reason string) (Reporter, error) {
	var reporter Reporter
	err := db.WithProviderScope(ctx, r.pool, reason, func(ctx context.Context, tx db.Tx) error {
		var err error
		if reporter, err = scanReporter(tx.QueryRow(ctx, oneReporterStatement, reporterID.String())); err != nil {
			return ErrReporterNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrReporterNotFound) {
			return Reporter{}, err
		}
		return Reporter{}, fmt.Errorf("authority: read reporter: %w", err)
	}
	return reporter, nil
}

// List reads one page of the registry in reporter_id order, recording the read with the reason.
func (r *Reporters) List(ctx context.Context, query ReporterQuery, reason string) (ReporterPage, error) {
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return ReporterPage{}, fmt.Errorf("%w: %w", ErrReporterInvalid, err)
	}
	if query.Reports != "" && !Reports(query.Reports).Valid() {
		return ReporterPage{}, fmt.Errorf("%w: reports is provisioning or obligation:<domain>", ErrReporterInvalid)
	}
	if query.State != "" && query.State != "active" && query.State != "retired" {
		return ReporterPage{}, fmt.Errorf("%w: state is active or retired", ErrReporterInvalid)
	}

	var page ReporterPage
	err = db.WithProviderScope(ctx, r.pool, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, listReportersStatement, db.Keyset(query.After), query.Reports, query.State, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			reporter, err := scanReporter(rows)
			if err != nil {
				return err
			}
			page.Reporters = append(page.Reporters, reporter)
		}
		return rows.Err()
	})
	if err != nil {
		return ReporterPage{}, fmt.Errorf("authority: list reporters: %w", err)
	}
	if len(page.Reporters) > limit {
		page.Reporters = page.Reporters[:limit]
		next := page.Reporters[limit-1].ID
		page.Next = &next
	}
	return page, nil
}

// reporterForStatement is one row always: the active reporter registered with the Principal, or
// empty columns when there is none.
const reporterForStatement = `SELECT coalesce(max(reporter_id::text), ''), coalesce(max(reports), '')
FROM organization.reporter
WHERE principal_id = $1 AND retired_at IS NULL`

// ReporterFor reads the active reporter registered with the Principal: its identifier and what it
// reports, or the nil identifier when none is. A retired reporter admits nobody from the next
// request on.
func (r *Reader) ReporterFor(ctx context.Context, principal id.UUID) (id.UUID, Reports, error) {
	if principal.IsNil() {
		return id.UUID{}, "", errors.New("authority: a principal is required")
	}
	var rawID, reports string
	if err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, reporterForStatement, principal.String()).Scan(&rawID, &reports)
	}); err != nil {
		return id.UUID{}, "", fmt.Errorf("authority: read reporter registration: %w", err)
	}
	if rawID == "" {
		return id.UUID{}, "", nil
	}
	reporterID, err := id.Parse(rawID)
	if err != nil {
		return id.UUID{}, "", fmt.Errorf("authority: read reporter registration: %w", err)
	}
	return reporterID, Reports(reports), nil
}

func scanReporter(row scanner) (Reporter, error) {
	var (
		reporter                                  Reporter
		rawID, principal, registeredBy, retiredBy string
		reports                                   string
	)
	if err := row.Scan(&rawID, &principal, &reports, &reporter.Purpose, &reporter.Version, &reporter.RegisteredAt,
		&registeredBy, &reporter.RetiredAt, &retiredBy); err != nil {
		return Reporter{}, err
	}
	reporter.Reports = Reports(reports)
	var err error
	if reporter.ID, err = id.Parse(rawID); err != nil {
		return Reporter{}, err
	}
	if reporter.Principal, err = id.Parse(principal); err != nil {
		return Reporter{}, err
	}
	if reporter.RegisteredBy, err = id.Parse(registeredBy); err != nil {
		return Reporter{}, err
	}
	if retiredBy != "" {
		parsed, err := id.Parse(retiredBy)
		if err != nil {
			return Reporter{}, err
		}
		reporter.RetiredBy = &parsed
	}
	return reporter, nil
}
