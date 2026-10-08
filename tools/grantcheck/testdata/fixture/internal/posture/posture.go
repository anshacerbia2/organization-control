// Package posture mirrors the isolation posture check the serving process runs at startup and behind
// readiness: a raw transaction on the tenant connections, declared as a boundary.
package posture

import (
	"context"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// AssertIsolation reads the catalog.
func AssertIsolation(ctx context.Context, pool db.Transactor) error {
	return pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `SELECT c.relname, c.relforcerowsecurity FROM pg_class c WHERE c.relkind = 'r'`)
		return err
	})
}
