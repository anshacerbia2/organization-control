// Package authority mirrors the caller records and the bootstrap: declared boundaries on the
// provider role, each a raw transaction on the provider connections.
package authority

import (
	"context"

	"github.com/anshacerbia2/organization-control/internal/db"
)

type Reader struct{ tx db.Transactor }

func (r *Reader) ProviderStanding(ctx context.Context) error {
	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `SELECT EXISTS (SELECT 1 FROM organization.provider_grant WHERE principal_id = $1)`)
		return err
	})
}

func (r *Reader) EmergencyGrants(ctx context.Context) error {
	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM organization.provider_grant WHERE kind = 'emergency'`)
		return err
	})
}

func (r *Reader) ConsumerFor(ctx context.Context) error {
	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `SELECT consumer_id FROM projection.consumer WHERE principal_id = $1`)
		return err
	})
}

func (r *Reader) RecordEmergencyUse(ctx context.Context) error {
	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE organization.emergency_grant_use SET uses = uses + 1 WHERE grant_id = $1`)
		return err
	})
}

func (r *Reader) EmergencyValidation(ctx context.Context) error {
	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `SELECT last_used_at FROM organization.emergency_grant_use`)
		return err
	})
}

func (r *Reader) UnreviewedAccess(ctx context.Context) error {
	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `SELECT actor_id FROM audit.privileged_access_review`)
		return err
	})
}

type Grants struct{ tx db.Transactor }

func (g *Grants) Bootstrap(ctx context.Context) error {
	return g.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO organization.provider_grant (grant_id) VALUES ($1)`)
		return err
	})
}
