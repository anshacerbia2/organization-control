package access

import (
	"context"

	"github.com/anshacerbia2/organization-control/internal/db"
)

type Recorder struct{ tx db.Transactor }

func (r *Recorder) RecordProviderAccess(ctx context.Context) error {
	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return db.RecordAccessInTx(ctx, tx)
	})
}
