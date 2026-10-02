// Package authority reads who a caller is to this service, and makes the first provider grant.
//
// ADR-ORG-001 §5.11 puts provider and consumer authority in records this platform holds, checked for
// each request by the token's principal_id, rather than in a role the token carries. A token says who
// the caller is; this package says what this service has recorded about that Principal
// (TDD-organization-control-001 §Caller Authority).
//
// # Why the reads take the raw provider connections
//
// The two tables it reads carry no tenant column and no Row-Level Security, so there is no scope to
// bind, and the scope wrapper would write a privileged-access record for every request before the
// request's own scope was even known. The frontier reader and the claim store take a raw transactor
// for the same reason. tools/grantcheck lists Reader's methods as declared boundaries on
// organization_provider_rt, because that is what the composition root hands it.
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

// Scope is the one registered provider scope this service checks (STD-IAM-002 §3.1.1). The table's
// check constraint admits it and nothing else.
const Scope = "provider:organization-control"

// Reader reads provider grants and consumer registrations.
type Reader struct {
	tx db.Transactor
}

// NewReader builds the reader on the provider connections.
func NewReader(tx db.Transactor) (*Reader, error) {
	if tx == nil {
		return nil, errors.New("authority: a transaction source is required")
	}
	return &Reader{tx: tx}, nil
}

// Standing is what the records say about a Principal's provider authority over this service
// (ADR-ORG-002): whether it holds an unrevoked grant, and whether authority is in force, by an
// emergency grant or by an approved activation that has not ended.
type Standing struct {
	// Holder is an unrevoked grant, eligible or emergency.
	Holder bool
	// InForce is authority now: an emergency grant, or an activation in force.
	InForce bool
	// Emergency is authority from an emergency grant, which every request reports.
	Emergency bool
}

// standingStatement is one row always: three booleans over the Principal's unrevoked grants and
// their activations.
const standingStatement = `SELECT
    EXISTS (SELECT 1 FROM organization.provider_grant g
            WHERE g.principal_id = $1 AND g.scope = $2 AND g.revoked_at IS NULL),
    EXISTS (SELECT 1 FROM organization.provider_grant g
            WHERE g.principal_id = $1 AND g.scope = $2 AND g.revoked_at IS NULL AND g.kind = 'emergency'),
    EXISTS (SELECT 1 FROM organization.provider_activation a
            JOIN organization.provider_grant g ON g.grant_id = a.grant_id
            WHERE a.principal_id = $1 AND a.scope = $2 AND g.revoked_at IS NULL
              AND a.decision = 'approved' AND a.ended_at IS NULL AND now() < a.ends_at)`

// ProviderStanding reads the Principal's provider authority over this service. A revoked grant, an
// activation past its end, and one ended early confer nothing from the next request on.
func (r *Reader) ProviderStanding(ctx context.Context, principal id.UUID) (Standing, error) {
	if principal.IsNil() {
		return Standing{}, errors.New("authority: a principal is required")
	}
	var (
		standing  Standing
		activated bool
	)
	if err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, standingStatement, principal.String(), Scope).
			Scan(&standing.Holder, &standing.Emergency, &activated)
	}); err != nil {
		return Standing{}, fmt.Errorf("authority: read provider standing: %w", err)
	}
	standing.InForce = standing.Emergency || activated
	return standing, nil
}

// consumerStatement is one row always: coalesce turns "none registered" into an empty name, so the
// expected case is not an error this package would need a no-rows sentinel to tell from a failure.
const consumerStatement = `SELECT coalesce((
    SELECT consumer_id FROM projection.consumer WHERE principal_id = $1 AND retired_at IS NULL), '')`

// ConsumerFor names the active projection consumer registered with the Principal, or "" when none is.
// A retired consumer names nobody: retiring it is how its authority ends.
func (r *Reader) ConsumerFor(ctx context.Context, principal id.UUID) (string, error) {
	if principal.IsNil() {
		return "", errors.New("authority: a principal is required")
	}
	var consumer string
	if err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, consumerStatement, principal.String()).Scan(&consumer)
	}); err != nil {
		return "", fmt.Errorf("authority: read consumer registration: %w", err)
	}
	return consumer, nil
}

// Bootstrap is the request to make the first provider grant.
type Bootstrap struct {
	// Principal is an existing Principal, minted by the Identity Control API's ceremony. The
	// bootstrap creates no identity.
	Principal id.UUID

	// Operator is the person running the command, as they give their name.
	Operator string

	// Reason is why, recorded with the grant.
	Reason string
}

// Grant is a provider grant as the bootstrap recorded it.
type Grant struct {
	ID        id.UUID
	Principal id.UUID
	Scope     string
	Operator  string
	GrantedAt time.Time

	// Existing is set when the bootstrap had already granted this Principal and wrote nothing.
	Existing bool
}

// Unexported, unlike the domain packages' sentinels: they belong to a command an operator runs, never
// reach an HTTP caller, and so have no problem type for internal/httpapi's registry to map.
var (
	// errInvalid means the bootstrap request is incomplete.
	errInvalid = errors.New("authority: invalid bootstrap")

	// errGrantsExist means a provider grant already exists for someone else, so this is not the
	// first grant and the bootstrap is not the way to make it.
	errGrantsExist = errors.New("authority: a provider grant already exists; the bootstrap makes only the first")
)

// Grants makes the first provider grant.
type Grants struct {
	tx db.Transactor
	id func() (id.UUID, error)
}

// NewGrants builds the bootstrap on the provider connections.
func NewGrants(tx db.Transactor) (*Grants, error) {
	if tx == nil {
		return nil, errors.New("authority: a transaction source is required")
	}
	return &Grants{tx: tx, id: id.NewV7}, nil
}

// existingGrantsStatement reads at most two grants: enough to tell "none", "only the bootstrap" and
// "more than that" apart without reading the table.
const existingGrantsStatement = `SELECT grant_id::text, principal_id::text, coalesce(bootstrap_operator, ''),
       granted_by IS NULL, granted_at
FROM organization.provider_grant
ORDER BY granted_at
LIMIT 2`

// The bootstrap grant is an emergency grant: before a second provider exists, no one could approve
// its activation (ADR-ORG-002 §5.2).
const insertBootstrapStatement = `INSERT INTO organization.provider_grant
    (grant_id, principal_id, scope, bootstrap_operator, reason, kind)
VALUES ($1, $2, $3, $4, $5, 'emergency')
RETURNING granted_at`

// Bootstrap makes the first provider grant, following ADR-ORG-001 §5.11.
//
// It refuses when any grant exists, except that a rerun naming the Principal the bootstrap already
// granted reports that grant and writes nothing, so a run whose answer was lost can be repeated
// without a second row. A rerun cannot rewrite the operator or the reason on record. Two concurrent
// runs both reading an empty table are settled by provider_grant_single_bootstrap: the second insert
// fails on it.
func (g *Grants) Bootstrap(ctx context.Context, req Bootstrap) (Grant, error) {
	operator, reason := strings.TrimSpace(req.Operator), strings.TrimSpace(req.Reason)
	switch {
	case req.Principal.IsNil():
		return Grant{}, fmt.Errorf("%w: a principal_id is required", errInvalid)
	case operator == "":
		return Grant{}, fmt.Errorf("%w: the operator running the bootstrap must be named", errInvalid)
	case reason == "":
		return Grant{}, fmt.Errorf("%w: a reason is required", errInvalid)
	}

	var grant Grant
	err := g.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, existingGrantsStatement)
		if err != nil {
			return fmt.Errorf("read existing grants: %w", err)
		}
		var existing []Grant
		var bootstrap []bool
		for rows.Next() {
			var (
				grantID, principal string
				row                Grant
				isBootstrap        bool
			)
			if err := rows.Scan(&grantID, &principal, &row.Operator, &isBootstrap, &row.GrantedAt); err != nil {
				rows.Close()
				return fmt.Errorf("read existing grants: %w", err)
			}
			if row.ID, err = id.Parse(grantID); err != nil {
				rows.Close()
				return fmt.Errorf("read existing grants: %w", err)
			}
			if row.Principal, err = id.Parse(principal); err != nil {
				rows.Close()
				return fmt.Errorf("read existing grants: %w", err)
			}
			row.Scope = Scope
			existing = append(existing, row)
			bootstrap = append(bootstrap, isBootstrap)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read existing grants: %w", err)
		}

		switch {
		case len(existing) == 1 && bootstrap[0] && existing[0].Principal == req.Principal:
			grant = existing[0]
			grant.Existing = true
			return nil
		case len(existing) > 0:
			return errGrantsExist
		}

		grantID, err := g.id()
		if err != nil {
			return fmt.Errorf("mint grant identifier: %w", err)
		}
		grant = Grant{ID: grantID, Principal: req.Principal, Scope: Scope, Operator: operator}
		return tx.QueryRow(ctx, insertBootstrapStatement,
			grantID.String(), req.Principal.String(), Scope, operator, reason).Scan(&grant.GrantedAt)
	})
	if err != nil {
		if errors.Is(err, errGrantsExist) {
			return Grant{}, err
		}
		return Grant{}, fmt.Errorf("authority: bootstrap: %w", err)
	}
	return grant, nil
}

const emergencyCountStatement = `SELECT count(*) FROM organization.provider_grant
WHERE scope = $1 AND kind = 'emergency' AND revoked_at IS NULL`

// EmergencyGrants counts the unrevoked emergency grants over this service. A production deployment
// with fewer than two is reported (ADR-ORG-002 §5.2).
func (r *Reader) EmergencyGrants(ctx context.Context) (int, error) {
	var count int
	if err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, emergencyCountStatement, Scope).Scan(&count)
	}); err != nil {
		return 0, fmt.Errorf("authority: count emergency grants: %w", err)
	}
	return count, nil
}
