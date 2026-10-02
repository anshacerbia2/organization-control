// Package delivery mirrors the in-process dispatchers' registration check: a raw transaction on the
// dispatch pool, which grantcheck lists out of scope rather than attributing to a role it verifies.
package delivery

import (
	"context"

	"github.com/anshacerbia2/organization-control/internal/db"
)

func checkRegistered(ctx context.Context, tx db.Transactor) error {
	return tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `SELECT EXISTS (SELECT 1 FROM projection.consumer WHERE consumer_id = $1 AND retired_at IS NULL)`)
		return err
	})
}

// Run is what the composition root starts.
func Run(ctx context.Context, tx db.Transactor) error { return checkRegistered(ctx, tx) }
