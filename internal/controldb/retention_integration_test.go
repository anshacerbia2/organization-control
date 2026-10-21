package controldb_test

// The two retention purges the maintenance stage gained on 2026-10-20, against the migrated database.
//
// Completed idempotency keys are kept 7 days (TDD-organization-control-003 §Completed Keys Are Kept
// 7 Days, Then Purged), and only the migration role purges them. Audit records are kept 7 years and
// purged after (STD-GLB-007 1.1.0, TDD-organization-control-001 §Privileged Access Review): the purge
// deletes only older records, and the database refuses the delete of a younger one, whoever asks.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"

	"github.com/anshacerbia2/organization-control/internal/controldb"
)

// runtimeRoles is every role a deployable runs as. None is the migration role.
var runtimeRoles = []string{
	"organization_rt", "organization_provider_rt", "organization_consumer_rt",
	"organization_resolution_rt", "organization_dispatch_rt", "organization_self_rt",
	"organization_invitation_resolver_rt",
}

// TestNoRuntimeRoleCanPurgeIdempotencyKeys: the purge is the maintenance stage's, as the table's
// owner. A request path able to delete a key could make a retry of a completed command run it again.
func TestNoRuntimeRoleCanPurgeIdempotencyKeys(t *testing.T) {
	pool, ctx := openAdmin(t)

	for _, role := range runtimeRoles {
		var deletes, executes bool
		if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT has_table_privilege($1, 'platform.idempotency_key', 'DELETE'),
				       has_function_privilege($1, 'platform.purge_idempotency_keys(interval)', 'EXECUTE')`,
				role).Scan(&deletes, &executes)
		}); err != nil {
			t.Fatalf("reading %s's privileges: %v", role, err)
		}
		if deletes {
			t.Errorf("%s holds DELETE on platform.idempotency_key", role)
		}
		if executes {
			t.Errorf("%s holds EXECUTE on platform.purge_idempotency_keys", role)
		}
	}
}

// TestMaintenancePurgesCompletedIdempotencyKeysAfterSevenDays: a key completed 8 days ago goes, one
// completed 6 days ago stays, and a key claimed 30 days ago and never completed stays, because its
// effect may have committed without its response.
func TestMaintenancePurgesCompletedIdempotencyKeysAfterSevenDays(t *testing.T) {
	pool, ctx := openAdmin(t)

	scope := "maintenance-suite:" + newUUID(t)
	t.Cleanup(func() {
		execAdmin(t, context.Background(), pool, `DELETE FROM platform.idempotency_key WHERE scope = $1`, scope)
	})
	for _, key := range []struct {
		name, claimed, completed string
	}{
		{"completed-8-days", "8 days", "8 days"},
		{"completed-6-days", "6 days", "6 days"},
		{"uncompleted-30-days", "30 days", ""},
	} {
		execAdmin(t, ctx, pool, `
			INSERT INTO platform.idempotency_key
			    (scope, key, request_digest, response_status, response_body, claimed_at, completed_at)
			VALUES ($1, $2, 'digest',
			        CASE WHEN $4 = '' THEN NULL ELSE 201 END,
			        CASE WHEN $4 = '' THEN NULL ELSE '{}'::jsonb END,
			        now() - $3::interval,
			        now() - NULLIF($4, '')::interval)`,
			scope, key.name, key.claimed, key.completed)
	}

	report, err := controldb.RunMaintenance(ctx, pool, controldb.DefaultMaintenance)
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	if report.IdempotencyKeysPurged < 1 {
		t.Errorf("the stage purged %d keys, want at least the one completed 8 days ago", report.IdempotencyKeysPurged)
	}

	kept := map[string]bool{}
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT key FROM platform.idempotency_key WHERE scope = $1`, scope)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			kept[key] = true
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("reading the keys: %v", err)
	}
	if kept["completed-8-days"] {
		t.Error("a key completed 8 days ago survived; the published window is 7 days")
	}
	if !kept["completed-6-days"] {
		t.Error("a key completed 6 days ago was purged inside the published window")
	}
	if !kept["uncompleted-30-days"] {
		t.Error("an uncompleted key was purged; a retry would execute its command twice")
	}
}

// auditSeed is one 8-year-old and one 6-year-old record in each audit table.
type auditSeed struct {
	oldAccess, youngAccess, oldReview, youngReview, oldGrant, youngGrant string
}

// seedAudit inserts the records as the owner, with explicit times, and removes them afterwards. The
// removal is the one delete the retention trigger is meant to stop, so it turns triggers off for its
// own transaction: session_replication_role, which only a superuser sets, on the disposable test
// database. The provider grants go after their use records, with the triggers on again, so the
// foreign key is checked as usual.
func seedAudit(t *testing.T, ctx context.Context, pool *db.Pool) auditSeed {
	t.Helper()
	s := auditSeed{newUUID(t), newUUID(t), newUUID(t), newUUID(t), newUUID(t), newUUID(t)}
	t.Cleanup(func() {
		execAdmin(t, context.Background(), pool, `SET LOCAL session_replication_role = replica;
			DELETE FROM audit.privileged_access WHERE access_id IN ('`+s.oldAccess+`', '`+s.youngAccess+`');
			DELETE FROM audit.privileged_access_review WHERE review_id IN ('`+s.oldReview+`', '`+s.youngReview+`');
			DELETE FROM organization.emergency_grant_use WHERE grant_id IN ('`+s.oldGrant+`', '`+s.youngGrant+`')`)
		execAdmin(t, context.Background(), pool,
			`DELETE FROM organization.provider_grant WHERE grant_id = ANY ($1::uuid[])`, []string{s.oldGrant, s.youngGrant})
	})
	for _, record := range []struct{ access, review, grant, age string }{
		{s.oldAccess, s.oldReview, s.oldGrant, "8 years"},
		{s.youngAccess, s.youngReview, s.youngGrant, "6 years"},
	} {
		execAdmin(t, ctx, pool, `
			INSERT INTO audit.privileged_access (access_id, actor_id, correlation_id, reason, authority, occurred_at)
			VALUES ($1::uuid, gen_random_uuid(), gen_random_uuid(), 'retention suite', 'emergency', now() - $2::interval)`,
			record.access, record.age)
		execAdmin(t, ctx, pool, `
			INSERT INTO audit.privileged_access_review
			    (review_id, actor_id, period_from, period_to, outcome, statement, accesses, emergency_accesses,
			     reviewed_by, correlation_id, reviewed_at)
			VALUES ($1::uuid, gen_random_uuid(), now() - $2::interval - interval '7 days', now() - $2::interval,
			        'appropriate', 'retention suite', 0, 0, gen_random_uuid(), gen_random_uuid(), now() - $2::interval)`,
			record.review, record.age)
		// A revoked emergency grant, so no report or authority read of a live grant sees it.
		execAdmin(t, ctx, pool, `
			INSERT INTO organization.provider_grant
			    (grant_id, principal_id, scope, reason, kind, granted_by, granted_at, revoked_at, revoked_by, revoke_reason)
			VALUES ($1::uuid, gen_random_uuid(), 'provider:identity-control', 'retention suite', 'emergency',
			        gen_random_uuid(), now() - $2::interval - interval '1 year', now() - $2::interval,
			        gen_random_uuid(), 'retention suite')`,
			record.grant, record.age)
		execAdmin(t, ctx, pool, `
			INSERT INTO organization.emergency_grant_use (grant_id, first_used_at, last_used_at, uses)
			VALUES ($1::uuid, now() - $2::interval - interval '1 day', now() - $2::interval, 2)`,
			record.grant, record.age)
	}
	return s
}

// auditKept reads which of the seeded records exist.
func auditKept(t *testing.T, ctx context.Context, tx db.Tx, s auditSeed) map[string]bool {
	t.Helper()
	kept := map[string]bool{}
	for name, probe := range map[string][2]string{
		"8-year-old access":    {`SELECT EXISTS (SELECT 1 FROM audit.privileged_access WHERE access_id = $1::uuid)`, s.oldAccess},
		"6-year-old access":    {`SELECT EXISTS (SELECT 1 FROM audit.privileged_access WHERE access_id = $1::uuid)`, s.youngAccess},
		"8-year-old review":    {`SELECT EXISTS (SELECT 1 FROM audit.privileged_access_review WHERE review_id = $1::uuid)`, s.oldReview},
		"6-year-old review":    {`SELECT EXISTS (SELECT 1 FROM audit.privileged_access_review WHERE review_id = $1::uuid)`, s.youngReview},
		"8-year-old grant use": {`SELECT EXISTS (SELECT 1 FROM organization.emergency_grant_use WHERE grant_id = $1::uuid)`, s.oldGrant},
		"6-year-old grant use": {`SELECT EXISTS (SELECT 1 FROM organization.emergency_grant_use WHERE grant_id = $1::uuid)`, s.youngGrant},
	} {
		var exists bool
		if err := tx.QueryRow(ctx, probe[0], probe[1]).Scan(&exists); err != nil {
			t.Fatalf("reading the %s: %v", name, err)
		}
		kept[name] = exists
	}
	return kept
}

func assertOnlyOldPurged(t *testing.T, kept map[string]bool) {
	t.Helper()
	for name, exists := range kept {
		old := strings.HasPrefix(name, "8-year-old")
		if old && exists {
			t.Errorf("the %s survived the purge; STD-GLB-007 keeps it 7 years and no longer", name)
		}
		if !old && !exists {
			t.Errorf("the %s was purged; a record younger than 7 years is never deleted", name)
		}
	}
}

// grantMigrator gives organization_migrator, inside one transaction, what owning the audit tables
// gives it in production, and becomes it. Atlas ran as this suite's superuser, so the migration role
// owns nothing here.
const grantMigrator = `GRANT USAGE ON SCHEMA audit, organization TO organization_migrator;
GRANT SELECT, DELETE ON audit.privileged_access, audit.privileged_access_review,
                        organization.emergency_grant_use TO organization_migrator;
SET LOCAL ROLE organization_migrator`

var errRollBack = errors.New("assertions done; roll back")

// TestTheAuditPurgeDeletesOnlyRecordsOlderThanSevenYears is STD-GLB-007 compliance check 4, first
// half: the purge deletes only records older than 7 years. As the migration role, the role the stage
// runs as, in a transaction rolled back; then the stage itself, for real.
func TestTheAuditPurgeDeletesOnlyRecordsOlderThanSevenYears(t *testing.T) {
	pool, ctx := openAdmin(t)
	s := seedAudit(t, ctx, pool)

	var purged controldb.AuditPurge
	var kept map[string]bool
	err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if _, err := tx.Exec(ctx, grantMigrator); err != nil {
			return err
		}
		var err error
		if purged, err = controldb.PurgeExpiredAuditRecords(ctx, tx, controldb.AuditRetentionFloor); err != nil {
			return err
		}
		kept = auditKept(t, ctx, tx, s)
		return errRollBack
	})
	if !errors.Is(err, errRollBack) {
		t.Fatalf("purge as organization_migrator: %v", err)
	}
	if purged.PrivilegedAccess < 1 || purged.Reviews < 1 || purged.EmergencyUses < 1 {
		t.Errorf("purged %+v, want at least the 8-year-old record of each table", purged)
	}
	assertOnlyOldPurged(t, kept)

	report, err := controldb.RunMaintenance(ctx, pool, controldb.DefaultMaintenance)
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	if report.AuditPurged.PrivilegedAccess < 1 || report.AuditPurged.Reviews < 1 || report.AuditPurged.EmergencyUses < 1 {
		t.Errorf("the stage purged %+v, want at least the 8-year-old record of each table", report.AuditPurged)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		kept = auditKept(t, ctx, tx, s)
		return nil
	}); err != nil {
		t.Fatalf("reading the records: %v", err)
	}
	assertOnlyOldPurged(t, kept)
}

// TestTheDatabaseRefusesToDeleteAYoungAuditRecord: the trigger, not the purge's WHERE clause, is the
// control. A direct DELETE of a 6-year-old record is refused for the migration role and for a
// superuser, which row-level security would not bind, and a TRUNCATE is refused for any record.
func TestTheDatabaseRefusesToDeleteAYoungAuditRecord(t *testing.T) {
	pool, ctx := openAdmin(t)
	s := seedAudit(t, ctx, pool)

	deletes := map[string][2]string{
		"privileged access":   {`DELETE FROM audit.privileged_access WHERE access_id = $1::uuid`, s.youngAccess},
		"privileged review":   {`DELETE FROM audit.privileged_access_review WHERE review_id = $1::uuid`, s.youngReview},
		"emergency grant use": {`DELETE FROM organization.emergency_grant_use WHERE grant_id = $1::uuid`, s.youngGrant},
	}
	for name, statement := range deletes {
		for _, as := range []string{"the migration role", "a superuser"} {
			t.Run(name+" as "+as, func(t *testing.T) {
				err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
					if as == "the migration role" {
						if _, err := tx.Exec(ctx, grantMigrator); err != nil {
							return err
						}
					}
					if _, err := tx.Exec(ctx, statement[0], statement[1]); err != nil {
						return err
					}
					return errRollBack
				})
				if errors.Is(err, errRollBack) || err == nil {
					t.Fatal("a record younger than 7 years was deleted")
				}
				if !strings.Contains(err.Error(), "younger than 7 years") {
					t.Errorf("refused for another reason than the retention trigger: %v", err)
				}
			})
		}
	}

	for _, table := range []string{"audit.privileged_access", "audit.privileged_access_review",
		"organization.emergency_grant_use"} {
		t.Run("truncate "+table, func(t *testing.T) {
			err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
				if _, err := tx.Exec(ctx, `TRUNCATE `+table); err != nil {
					return err
				}
				return errRollBack
			})
			if errors.Is(err, errRollBack) || err == nil {
				t.Fatal("a superuser truncated the audit records")
			}
			if !strings.Contains(err.Error(), "never truncated") {
				t.Errorf("refused for another reason than the retention trigger: %v", err)
			}
		})
	}
}

// TestMaintenanceRefusesAnAuditRetentionBelowSevenYears: the stage and the purge both refuse it,
// before anything runs.
func TestMaintenanceRefusesAnAuditRetentionBelowSevenYears(t *testing.T) {
	pool, ctx := openAdmin(t)

	cfg := controldb.DefaultMaintenance
	cfg.AuditRetentionYears = controldb.AuditRetentionFloor - 1
	if _, err := controldb.RunMaintenance(ctx, pool, cfg); err == nil {
		t.Error("RunMaintenance accepted an audit retention of 6 years")
	}
	cfg = controldb.DefaultMaintenance
	cfg.IdempotencyRetention = 0
	if _, err := controldb.RunMaintenance(ctx, pool, cfg); err == nil {
		t.Error("RunMaintenance accepted a zero idempotency key retention")
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := controldb.PurgeExpiredAuditRecords(ctx, tx, controldb.AuditRetentionFloor-1)
		return err
	}); err == nil {
		t.Error("PurgeExpiredAuditRecords accepted 6 years")
	}
}
