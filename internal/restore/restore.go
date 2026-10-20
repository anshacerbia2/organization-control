// Package restore moves every Tenant and provider grant version past what consumers hold, after a
// restore to an older point and before the service serves.
package restore

// The restore bump (ADR-ORG-003 §5.5, amended 2026-10-20; TDD-organization-control-002 §After a
// Restore to an Older Point).
//
// A restore to an older point gives back Tenant versions and provider grant versions that consumers
// have already seen superseded, and a consumer discards a lower version as already applied. Before
// the service serves, this stage moves every one of them past anything a consumer can hold, as etcd
// restores its revision: "In order to ensure the revisions are never decreasing after a restore, you
// can supply the --bump-revision option" (etcd v3.6, Disaster recovery). In one transaction, with no
// actor, on the restore's own connection:
//
//  1. every Tenant's version and security version, and every provider grant's version, gain
//     Margin. identity-control orders a Tenant by tenant_version and decides a token's
//     staleness by tenant_security_version, so both move;
//  2. every Tenant and published-scope grant that withholds access is published at its new version;
//  3. no active one is: publishing it could grant again what a lost withdrawal took away;
//  4. every active consumer is asked to reconcile: reconciliation_requested_at is set, its alert
//     fires, and its next recorded reconciliation clears it (ADR-ORG-001 §5.7).
//
// Memberships are not bumped here. Each is advanced from the consumer's own report
// (POST /v1/projections/advance-versions), which knows how far a consumer holds it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"

	"github.com/anshacerbia2/organization-control/internal/authority"
	"github.com/anshacerbia2/organization-control/internal/tenant"
)

// Margin is the margin every version gains: 10^9, etcd's own for a week-old snapshot ("you may
// cover a week old snapshot with bumping by 1'000'000'000 assuming that etcd runs with less than 1500
// writes per second"). No row here changes 10^9 times within a backup window, and the versions are
// bigint.
const Margin int64 = 1_000_000_000

// Report is what one bump did.
type Report struct {
	ObservedAt         time.Time
	Tenants            int64
	Grants             int64
	TenantsPublished   int
	GrantsPublished    int
	ConsumersRequested int64
}

const bumpTenants = `UPDATE tenant.tenant
SET version = version + $1, tenant_security_version = tenant_security_version + $1`

const bumpGrants = `UPDATE organization.provider_grant
SET grant_version = grant_version + $1`

const requestReconciliation = `UPDATE projection.consumer
SET reconciliation_requested_at = $1
WHERE retired_at IS NULL`

// Bump runs the bump on pool, which is the restore's connection: the cluster's superuser in
// deploy/dev/restore.sh, which reaches every Tenant before any is bound, as the restore's pause
// does. It is not idempotent by design: run twice, the versions gain the margin twice, which is
// harmless and still never decreases one.
func Bump(ctx context.Context, pool *db.Pool) (Report, error) {
	if pool == nil {
		return Report{}, errors.New("restore: a pool is required")
	}
	var report Report
	err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		report, err = BumpWithin(ctx, tx)
		return err
	})
	if err != nil {
		return Report{}, fmt.Errorf("restore: restore bump: %w", err)
	}
	return report, nil
}

// BumpWithin is the bump inside the caller's transaction, so a test can roll it back.
func BumpWithin(ctx context.Context, tx db.Tx) (Report, error) {
	var report Report
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&report.ObservedAt); err != nil {
		return Report{}, fmt.Errorf("reading the database clock: %w", err)
	}
	tag, err := tx.Exec(ctx, bumpTenants, Margin)
	if err != nil {
		return Report{}, fmt.Errorf("bumping the Tenant versions: %w", err)
	}
	report.Tenants = tag.RowsAffected()
	if tag, err = tx.Exec(ctx, bumpGrants, Margin); err != nil {
		return Report{}, fmt.Errorf("bumping the provider grant versions: %w", err)
	}
	report.Grants = tag.RowsAffected()
	if report.TenantsPublished, err = tenant.PublishWithdrawn(ctx, tx, report.ObservedAt.UTC()); err != nil {
		return Report{}, err
	}
	if report.GrantsPublished, err = authority.PublishWithdrawnGrants(ctx, tx); err != nil {
		return Report{}, err
	}
	if tag, err = tx.Exec(ctx, requestReconciliation, report.ObservedAt); err != nil {
		return Report{}, fmt.Errorf("asking every consumer to reconcile: %w", err)
	}
	report.ConsumersRequested = tag.RowsAffected()
	return report, nil
}
