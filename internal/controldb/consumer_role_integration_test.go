package controldb_test

// The consumer's role, asserted by connecting as it.
//
// A registered consumer ran as organization_provider_rt, so its credential could read and write
// every table the control plane can. These cases log in as organization_consumer_app and try what
// the consumer's seven routes do, and what they must not be able to do. Every attempt is rolled back.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"
)

var errRolledBack = errors.New("rolled back by the test")

// asConsumer runs statement as the consumer login role in a transaction that is always rolled back,
// optionally with the cross-Tenant binding the consumer scope sets, and returns its error.
func asConsumer(t *testing.T, bound bool, statement string, args ...any) error {
	t.Helper()
	password := os.Getenv("TEST_CONSUMER_PASSWORD")
	if password == "" {
		t.Fatal("TEST_CONSUMER_PASSWORD is empty: the consumer login role exists but its password " +
			"was never exported to the test environment")
	}
	pool, ctx := openAs(t, "organization_consumer_app", password)

	var result error
	err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if bound {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.provider_scope', 'true', true)`); err != nil {
				return err
			}
		}
		_, result = tx.Exec(ctx, statement, args...)
		return errRolledBack
	})
	if err != nil && !errors.Is(err, errRolledBack) {
		t.Fatalf("running as the consumer: %v", err)
	}
	return result
}

func consumerCount(t *testing.T, bound bool, statement string) int {
	t.Helper()
	password := os.Getenv("TEST_CONSUMER_PASSWORD")
	pool, ctx := openAs(t, "organization_consumer_app", password)
	var n int
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if bound {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.provider_scope', 'true', true)`); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, statement).Scan(&n)
	}); err != nil {
		t.Fatalf("counting as the consumer: %v", err)
	}
	return n
}

// What the snapshot and the fresh check read: every Tenant's Memberships, under the binding. An
// unbound consumer connection is refused outright, as an unbound provider one is: the policy reads
// the binding with missing_ok false.
func TestTheConsumerReadsMembershipsAndTenantsOnlyWhenBound(t *testing.T) {
	for _, table := range []string{"membership.membership", "tenant.tenant"} {
		if n := consumerCount(t, true, "SELECT count(*) FROM "+table); n < 2 {
			t.Errorf("the bound consumer sees %d rows of %s, want both fixture Tenants' rows", n, table)
		}
		if err := asConsumer(t, false, "SELECT count(*) FROM "+table); err == nil {
			t.Errorf("an unbound consumer connection read %s", table)
		}
	}
}

// What its routes write: three columns of its registry row, and the meter.
func TestTheConsumerWritesOnlyItsPositionColumns(t *testing.T) {
	for _, statement := range []string{
		`UPDATE projection.consumer SET snapshot_mark = 1 WHERE consumer_id = 'x'`,
		`UPDATE projection.consumer SET last_reported_mark = 1, last_reported_at = now() WHERE consumer_id = 'x'`,
		`UPDATE projection.consumer SET verify_calls_since_report = verify_calls_since_report + 1 WHERE consumer_id = 'x'`,
	} {
		if err := asConsumer(t, true, statement); err != nil {
			t.Errorf("the consumer cannot run what its routes need: %v\n  %s", err, statement)
		}
	}
}

// And everything it held as the provider role and must not hold now.
func TestTheConsumerCannotActAsTheControlPlane(t *testing.T) {
	for what, statement := range map[string]string{
		"change its own declared terms": `UPDATE projection.consumer SET max_accepted_age = interval '1 year' WHERE consumer_id = 'x'`,
		"un-retire itself":              `UPDATE projection.consumer SET retired_at = NULL WHERE consumer_id = 'x'`,
		"register another consumer":     `INSERT INTO projection.consumer (consumer_id, projection_version, max_accepted_age, stale_behavior) VALUES ('y', 1, interval '1 minute', 'fail_closed')`,
		"write a Membership":            `UPDATE membership.membership SET status = 'active'`,
		"write a Tenant":                `UPDATE tenant.tenant SET status = 'active'`,
		"read an invitation":            `SELECT 1 FROM invitation.invitation`,
		"read an Organization":          `SELECT 1 FROM organization.organization`,
		"read a Workspace":              `SELECT 1 FROM workspace.workspace`,
		"read delivery evidence":        `SELECT 1 FROM platform.delivery_receipt`,
		"close a dead letter":           `UPDATE platform.dead_letter SET resolved_at = now()`,
		"append to the outbox":          `INSERT INTO platform.outbox (event_type) VALUES ('x')`,
		"write the audit trail":         `INSERT INTO audit.privileged_access (access_id) VALUES (gen_random_uuid())`,
		"read Membership history":       `SELECT 1 FROM membership.membership_event`,
		// The provider authority snapshot reads what a grant confers, by column; why it was made,
		// who made it, and the requests behind an activation stay with the record.
		"read why a provider grant was made": `SELECT reason, granted_by FROM organization.provider_grant`,
		"read an activation's reason":        `SELECT reason, decided_by FROM organization.provider_activation`,
		"grant provider authority":           `INSERT INTO organization.provider_grant (grant_id) VALUES (gen_random_uuid())`,
		"read provider grant history":        `SELECT 1 FROM organization.provider_grant_event`,
	} {
		if err := asConsumer(t, true, statement); err == nil {
			t.Errorf("the consumer role can %s", what)
		}
	}
}
