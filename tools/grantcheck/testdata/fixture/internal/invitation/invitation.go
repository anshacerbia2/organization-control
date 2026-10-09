// Package invitation mirrors the scheduled invitation expiry: a declared boundary on the provider
// role, a raw transaction on the provider connections.
package invitation

import (
	"context"

	"github.com/anshacerbia2/organization-control/internal/db"
)

type ScheduledExpiry struct{ tx db.Transactor }

func (e *ScheduledExpiry) ExpireLapsed(ctx context.Context) error {
	return e.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operation.invitation_expiry SET state = 'expired' WHERE invitation_id = $1`)
		return err
	})
}
