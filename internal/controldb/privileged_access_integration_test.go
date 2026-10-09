package controldb_test

// The privileged-access record's readers, proven as the runtime roles (ADR-ORG-002 §5.6,
// TDD-organization-control-001 §Privileged Access Review).
//
// A provider in force reads every row; a Tenant administrator reads the provider rows that name its
// own Tenant, through audit.tenant_provider_access and nothing else; no runtime role changes or
// deletes a row of the record or of its reviews. Every assertion runs on a login role inheriting the
// role production traffic uses, for the reason the isolation suite does.

import (
	"context"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"
)

// seedAccess writes one row as the provider role, the way the recorder does.
func seedAccess(t *testing.T, ctx context.Context, pool *db.Pool, correlation, authority string, tenant any) {
	t.Helper()
	activation := any(nil)
	if authority == "activation" {
		activation = newID(t)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit.privileged_access
		    (access_id, actor_id, correlation_id, reason, authority, activation_id, tenant_id, operation)
		    VALUES ($1, $2, $3, 'seed', $4, $5::uuid, $6::uuid, 'GET /v1/tenants/{tenant_id}')`,
			newID(t), newID(t), correlation, authority, activation, tenant)
		return err
	}); err != nil {
		t.Fatalf("seed %s access: %v", authority, err)
	}
}

// TestATenantReadsOnlyTheProviderAccessThatNamedIt is the view's predicate, as the tenant role: its
// own Tenant's provider rows, and neither another Tenant's, a consumer's, nor one that names no
// Tenant.
func TestATenantReadsOnlyTheProviderAccessThatNamedIt(t *testing.T) {
	provider, ctx := providerPool(t)
	tenant, _ := tenantPool(t)

	correlation := newID(t)
	seedAccess(t, ctx, provider, correlation, "emergency", tenantA)
	seedAccess(t, ctx, provider, correlation, "activation", tenantA)
	seedAccess(t, ctx, provider, correlation, "consumer", tenantA)
	seedAccess(t, ctx, provider, correlation, "emergency", tenantB)
	seedAccess(t, ctx, provider, correlation, "emergency", nil)

	if err := bound(ctx, tenant, tenantA, func(ctx context.Context, tx db.Tx) error {
		if got := scanInt(t, ctx, tx,
			`SELECT count(*) FROM audit.tenant_provider_access WHERE correlation_id = $1`, correlation); got != 2 {
			t.Errorf("Tenant A reads %d rows of the correlation, want its 2 provider rows", got)
		}
		if got := scanInt(t, ctx, tx,
			`SELECT count(*) FROM audit.tenant_provider_access WHERE correlation_id = $1 AND tenant_id = $2`,
			correlation, tenantB); got != 0 {
			t.Errorf("Tenant A reads %d of Tenant B's rows, want 0", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("read as Tenant A: %v", err)
	}
}

// TestTheTenantRoleReachesTheRecordOnlyThroughTheView: no SELECT on the table, no write through the
// view (a single-table view is updatable, and the owner's privileges would apply), and an unbound
// read raises rather than answering nothing.
func TestTheTenantRoleReachesTheRecordOnlyThroughTheView(t *testing.T) {
	tenant, ctx := tenantPool(t)

	refused := map[string]string{
		"select the table":   `SELECT count(*) FROM audit.privileged_access`,
		"select the reviews": `SELECT count(*) FROM audit.privileged_access_review`,
		"insert through the view": `INSERT INTO audit.tenant_provider_access
		    (access_id, actor_id, authority, correlation_id, reason, tenant_id)
		    VALUES (gen_random_uuid(), gen_random_uuid(), 'emergency', gen_random_uuid(), 'forged', '` + tenantA + `')`,
		"delete through the view": `DELETE FROM audit.tenant_provider_access`,
	}
	for name, statement := range refused {
		t.Run(name, func(t *testing.T) {
			if err := bound(ctx, tenant, tenantA, func(ctx context.Context, tx db.Tx) error {
				_, err := tx.Exec(ctx, statement)
				return err
			}); err == nil {
				t.Error("the tenant role was allowed")
			}
		})
	}

	t.Run("an unbound read", func(t *testing.T) {
		if err := tenant.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, `SELECT count(*) FROM audit.tenant_provider_access`)
			return err
		}); err == nil {
			t.Error("an unbound connection read the view; missing_ok must be false")
		}
	})
}

// TestTheReviewIsInsertOnlyAndNeverTheSubjects is the review table's half: the separation and period
// checks hold for any writer, and the provider role can neither change nor delete a review.
func TestTheReviewIsInsertOnlyAndNeverTheSubjects(t *testing.T) {
	provider, ctx := providerPool(t)

	actor, reviewer, correlation := newID(t), newID(t), newID(t)
	insert := `INSERT INTO audit.privileged_access_review
	    (review_id, actor_id, period_from, period_to, outcome, statement, accesses, emergency_accesses,
	     reviewed_by, correlation_id)
	    VALUES ($1, $2, now() - interval '7 days', $3::timestamptz, 'appropriate', 'weekly review', 1, 0, $4, $5)`

	// A period_to of 'now' is the transaction's start, which the period check admits.
	reviewID := newID(t)
	if err := provider.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, insert, reviewID, actor, "now", reviewer, correlation)
		return err
	}); err != nil {
		t.Fatalf("a review by another provider was refused: %v", err)
	}

	refused := map[string][]any{
		"a review by its own subject":   {newID(t), actor, "now", actor, correlation},
		"a period ending in the future": {newID(t), actor, "infinity", reviewer, correlation},
	}
	for name, args := range refused {
		t.Run(name, func(t *testing.T) {
			if err := provider.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
				_, err := tx.Exec(ctx, insert, args...)
				return err
			}); err == nil {
				t.Error("the table accepted it")
			}
		})
	}

	for name, statement := range map[string]string{
		"update": `UPDATE audit.privileged_access_review SET outcome = 'escalated' WHERE review_id = $1`,
		"delete": `DELETE FROM audit.privileged_access_review WHERE review_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := provider.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
				_, err := tx.Exec(ctx, statement, reviewID)
				return err
			}); err == nil {
				t.Errorf("the provider role could %s a review", name)
			}
		})
	}
}
