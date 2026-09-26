// Package projection holds the cases grantcheck must attribute correctly.
package projection

import (
	"context"
	"fmt"

	"github.com/anshacerbia2/organization-control/internal/db"
)

type FrontierReader struct{ tx db.Transactor }

func (f *FrontierReader) FrontierFor(ctx context.Context) error {
	return f.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, frontierStatement)
		return err
	})
}

const frontierStatement = `SELECT max(sequence) FROM platform.outbox`

const (
	closeStatement    = `UPDATE platform.dead_letter SET resolved_at = now() WHERE event_id = $1`
	evidenceStatement = `SELECT 1 FROM platform.delivery_receipt WHERE event_id = $1`
	loadByID          = `SELECT invitation_id FROM invitation.invitation WHERE invitation_id = $1`
	loadByHash        = `SELECT invitation_id FROM invitation.invitation WHERE token_hash = $1`
	grantStatement    = `INSERT INTO membership.membership (membership_id) VALUES ($1)`
	tenantStatement   = `INSERT INTO tenant.tenant (tenant_id) VALUES ($1)`
)

type evidenceFinder func(ctx context.Context, tx db.Tx) error

func findEvidence(ctx context.Context, tx db.Tx) error {
	_, err := tx.Exec(ctx, evidenceStatement, "")
	return err
}

// Resolve reaches its evidence through a function value captured by the body, as the real
// resolver does.
func Resolve(ctx context.Context, pool *db.ResolutionPool) error {
	return closeIncident(ctx, pool, findEvidence)
}

func closeIncident(ctx context.Context, pool *db.ResolutionPool, find evidenceFinder) error {
	return db.WithResolutionScope(ctx, pool, "resolve", func(ctx context.Context, tx db.Tx) error {
		if err := find(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, closeStatement, ""); err != nil {
			return err
		}
		return db.RecordAccessInTx(ctx, tx)
	})
}

// load takes its statement as a parameter, and every caller passes a constant.
func load(ctx context.Context, tx db.Tx, statement, key string) error {
	return tx.QueryRow(ctx, statement, key).Scan()
}

// Accept runs under the tenant role, then opens a provider scope of its own. The provider
// statement must not be attributed to the tenant role.
func Accept(ctx context.Context, tenant *db.TenantPool, provider *db.ProviderPool) error {
	return db.WithTenantScope(ctx, tenant, func(ctx context.Context, tx db.Tx) error {
		if err := load(ctx, tx, loadByID, ""); err != nil {
			return err
		}
		if err := load(ctx, tx, loadByHash, ""); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, grantStatement, ""); err != nil {
			return err
		}
		return provision(ctx, provider)
	})
}

func provision(ctx context.Context, provider *db.ProviderPool) error {
	return db.WithProviderScope(ctx, provider, "provision", func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, tenantStatement, "")
		return err
	})
}

// Dynamic builds its statement at runtime: a problem, not a guess.
func Dynamic(ctx context.Context, pool *db.TenantPool, table string) error {
	return db.WithTenantScope(ctx, pool, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s", table))
		return err
	})
}

// Orphan runs SQL on a transaction no wrapper opened: no role is known for it.
func Orphan(ctx context.Context, tx db.Tx) error {
	_, err := tx.Exec(ctx, `DELETE FROM workspace.workspace WHERE workspace_id = $1`, "")
	return err
}

// Rogue opens a raw transaction and is not a declared boundary.
type Rogue struct{ tx db.Transactor }

func (r *Rogue) Do(ctx context.Context) error {
	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE organization.organization SET status = 'retired'`)
		return err
	})
}

// Freshness reads the frontier from inside a tenant body. The frontier reader owns its own
// connection, so its statement belongs to its declared role, not to the tenant role around it.
func Freshness(ctx context.Context, tenant *db.TenantPool, reader *FrontierReader) error {
	return db.WithTenantScope(ctx, tenant, func(ctx context.Context, tx db.Tx) error {
		return reader.FrontierFor(ctx)
	})
}
