// Package tenant mirrors the scheduled provisioning sweep: a declared boundary on the provider role,
// a raw transaction on the provider connections.
package tenant

import (
	"context"

	"github.com/anshacerbia2/organization-control/internal/db"
)

type ScheduledSweep struct{ tx db.Transactor }

func (s *ScheduledSweep) SweepUnresolved(ctx context.Context) error {
	return s.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operation.provisioning_sweep SET state = 'unresolved' WHERE request_id = $1`)
		return err
	})
}
