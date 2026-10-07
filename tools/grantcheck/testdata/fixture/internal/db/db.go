// Package db mirrors the real scope wrappers' shape: the pool type fixes the role, the body is a
// function value, and the shared helpers forward it as a parameter.
package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type Tx = pgx.Tx

type Body func(context.Context, Tx) error

type Transactor interface {
	InTx(ctx context.Context, fn func(context.Context, Tx) error) error
}

type PrivilegedRecorder interface {
	RecordProviderAccess(ctx context.Context) error
}

type TenantPool struct{ tx Transactor }
type ProviderPool struct {
	tx       Transactor
	recorder PrivilegedRecorder
}
type ResolutionPool struct {
	tx       Transactor
	recorder PrivilegedRecorder
}
type ConsumerPool struct {
	tx       Transactor
	recorder PrivilegedRecorder
}

// NewConsumerPool lets the fixture's composition root hand the pool a concrete recorder, as the real
// one does, so the recorder's type flows into the interface the wrappers call.
func NewConsumerPool(tx Transactor, recorder PrivilegedRecorder) *ConsumerPool {
	return &ConsumerPool{tx: tx, recorder: recorder}
}

func WithConsumerScope(ctx context.Context, pool *ConsumerPool, reason string, fn Body) error {
	return withRecordedScope(ctx, pool.tx, pool.recorder, fn)
}

func WithConsumerSnapshot(ctx context.Context, pool *ConsumerPool, reason string, fn Body) error {
	return withRecordedScope(ctx, pool.tx, pool.recorder, fn)
}

type SelfPool struct{ tx Transactor }

func WithSelfRead(ctx context.Context, pool *SelfPool, fn Body) error {
	return pool.tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, ""); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func WithTenantScope(ctx context.Context, pool *TenantPool, fn Body) error {
	return pool.tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, ""); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func WithTenantRead(ctx context.Context, pool *TenantPool, fn Body) error {
	return pool.tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, ""); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

// WithProviderInTenant records the access on the provider pool's recorder and runs the body on the
// tenant pool, as the real one does.
func WithProviderInTenant(ctx context.Context, provider *ProviderPool, tenants *TenantPool, reason string, fn Body) error {
	if err := provider.recorder.RecordProviderAccess(ctx); err != nil {
		return err
	}
	return tenants.tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, ""); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func WithProviderScope(ctx context.Context, pool *ProviderPool, reason string, fn Body) error {
	return withProviderScope(ctx, pool, reason, false, fn)
}

func WithProviderSnapshot(ctx context.Context, pool *ProviderPool, reason string, fn Body) error {
	return withProviderScope(ctx, pool, reason, true, fn)
}

func WithResolutionScope(ctx context.Context, pool *ResolutionPool, reason string, fn Body) error {
	return withRecordedScope(ctx, pool.tx, pool.recorder, fn)
}

func withProviderScope(ctx context.Context, pool *ProviderPool, reason string, snapshot bool, fn Body) error {
	return withRecordedScope(ctx, pool.tx, pool.recorder, fn)
}

func withRecordedScope(ctx context.Context, tx Transactor, recorder PrivilegedRecorder, fn Body) error {
	if err := recorder.RecordProviderAccess(ctx); err != nil {
		return err
	}
	return tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.provider_scope', 'true', true)`); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

// RecordAccessInTx writes evidence on the caller's transaction, so under the caller's role.
func RecordAccessInTx(ctx context.Context, tx Tx) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit.privileged_access (access_id) VALUES ($1)`, "")
	return err
}

type ClaimStore struct{ tx Transactor }

func (s *ClaimStore) Complete(ctx context.Context) error {
	return s.tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE platform.idempotency_key SET completed_at = now() WHERE key = $1`, "")
		return err
	})
}
