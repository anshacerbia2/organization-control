package authority

// The caller records and the bootstrap, against the real engine as the real provider login role.
//
// As the provider role rather than the owner, because the privileges are half the contract: that the
// role reads and inserts a grant and cannot rewrite one is a property of the role, and an owning
// connection would pass for the wrong reason. The owner is used only to clear and restore the grant
// table around the bootstrap, which needs it empty, and to arrange consumer rows.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
)

func pools(t *testing.T) (provider, owner *fdb.Pool, ctx context.Context) {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL is unset")
	}
	rest := base
	if index := strings.Index(base, "://"); index >= 0 {
		rest = base[index+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	var err error
	provider, err = fdb.Open(ctx, fdb.Config{Name: "authority-test-provider", MaxConns: 4,
		DSN: fmt.Sprintf("postgres://organization_provider_app:%s@%s", os.Getenv("TEST_PROVIDER_PASSWORD"), rest)})
	if err != nil {
		t.Fatalf("open the provider pool: %v", err)
	}
	t.Cleanup(provider.Close)
	owner, err = fdb.Open(ctx, fdb.Config{Name: "authority-test-owner", DSN: base, MaxConns: 2})
	if err != nil {
		t.Fatalf("open the owner pool: %v", err)
	}
	t.Cleanup(owner.Close)
	return provider, owner, ctx
}

func exec(t *testing.T, ctx context.Context, pool *fdb.Pool, statement string, args ...any) {
	t.Helper()
	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		_, err := tx.Exec(ctx, statement, args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func newID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return value
}

// emptyGrants clears the grant table for one test and restores what was there, the fixture's dev
// provider among it. Packages run one at a time (-p 1), and nothing else in this suite reads it. The
// saved rows are held here rather than in a scratch table, so a failed run leaves no table behind.
func emptyGrants(t *testing.T, ctx context.Context, owner *fdb.Pool) {
	t.Helper()
	type row struct {
		grantID, principal, scope, reason string
		grantedBy, operator               *string
		grantedAt                         time.Time
	}
	var saved []row
	if err := owner.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		rows, err := tx.Query(ctx, `SELECT grant_id::text, principal_id::text, scope, reason,
		    granted_by::text, bootstrap_operator, granted_at FROM organization.provider_grant`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.grantID, &r.principal, &r.scope, &r.reason, &r.grantedBy, &r.operator, &r.grantedAt); err != nil {
				return err
			}
			saved = append(saved, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("save the grants: %v", err)
	}
	exec(t, ctx, owner, `DELETE FROM organization.provider_grant`)
	t.Cleanup(func() {
		exec(t, context.Background(), owner, `DELETE FROM organization.provider_grant`)
		for _, r := range saved {
			exec(t, context.Background(), owner, `INSERT INTO organization.provider_grant
			    (grant_id, principal_id, scope, reason, granted_by, bootstrap_operator, granted_at)
			    VALUES ($1, $2, $3, $4, $5::uuid, $6, $7)`,
				r.grantID, r.principal, r.scope, r.reason, r.grantedBy, r.operator, r.grantedAt)
		}
	})
}

func TestTheBootstrapMakesTheFirstGrantOnce(t *testing.T) {
	provider, owner, ctx := pools(t)
	emptyGrants(t, ctx, owner)
	grants, err := NewGrants(provider)
	if err != nil {
		t.Fatal(err)
	}
	records, err := NewReader(provider)
	if err != nil {
		t.Fatal(err)
	}
	first, other := newID(t), newID(t)

	grant, err := grants.Bootstrap(ctx, Bootstrap{Principal: first, Operator: "Ada Operator", Reason: "first provider"})
	if err != nil || grant.Existing || grant.Principal != first || grant.Scope != Scope {
		t.Fatalf("the first bootstrap answered %+v, %v", grant, err)
	}
	if granted, err := records.ProviderGrant(ctx, first); err != nil || !granted {
		t.Errorf("the bootstrapped Principal reads as granted=%t, %v", granted, err)
	}
	if granted, err := records.ProviderGrant(ctx, other); err != nil || granted {
		t.Errorf("another Principal reads as granted=%t, %v", granted, err)
	}

	// A rerun for the same Principal reports the grant and cannot rewrite who is on record.
	again, err := grants.Bootstrap(ctx, Bootstrap{Principal: first, Operator: "Someone Else", Reason: "again"})
	if err != nil || !again.Existing || again.ID != grant.ID || again.Operator != "Ada Operator" {
		t.Errorf("the rerun answered %+v, %v; want the existing grant, operator unchanged", again, err)
	}

	if _, err := grants.Bootstrap(ctx, Bootstrap{Principal: other, Operator: "Ada Operator", Reason: "second"}); !errors.Is(err, errGrantsExist) {
		t.Errorf("a bootstrap for another Principal answered %v, want errGrantsExist", err)
	}

	for name, statement := range map[string]string{
		"rewrite a grant": `UPDATE organization.provider_grant SET principal_id = gen_random_uuid()`,
		"delete a grant":  `DELETE FROM organization.provider_grant`,
	} {
		err := provider.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			_, err := tx.Exec(ctx, statement)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("the provider role could %s: %v", name, err)
		}
	}
}

func TestTwoConcurrentBootstrapsLeaveOneGrant(t *testing.T) {
	provider, owner, ctx := pools(t)
	emptyGrants(t, ctx, owner)
	grants, err := NewGrants(provider)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	principals := []id.UUID{newID(t), newID(t)}
	results := make([]error, len(principals))
	for i, principal := range principals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = grants.Bootstrap(ctx, Bootstrap{Principal: principal, Operator: "Ada Operator", Reason: "race"})
		}()
	}
	wg.Wait()

	var rows int
	if err := owner.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM organization.provider_grant`).Scan(&rows)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || (results[0] == nil) == (results[1] == nil) {
		t.Errorf("two bootstraps left %d rows with results %v; want one row and one refusal", rows, results)
	}
}

func TestTheBootstrapRefusesAnIncompleteRequest(t *testing.T) {
	provider, _, ctx := pools(t)
	grants, err := NewGrants(provider)
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]Bootstrap{
		"no principal": {Operator: "Ada", Reason: "r"},
		"no operator":  {Principal: newID(t), Reason: "r"},
		"no reason":    {Principal: newID(t), Operator: "Ada"},
	} {
		if _, err := grants.Bootstrap(ctx, req); !errors.Is(err, errInvalid) {
			t.Errorf("%s: answered %v, want errInvalid", name, err)
		}
	}
}

func TestAConsumerIsFoundByItsPrincipalWhileActive(t *testing.T) {
	provider, owner, ctx := pools(t)
	records, err := NewReader(provider)
	if err != nil {
		t.Fatal(err)
	}
	workload, consumerID := newID(t), "authority-test-"+newID(t).String()
	exec(t, ctx, owner, `INSERT INTO projection.consumer
	    (consumer_id, principal_id, projection_version, max_accepted_age, stale_behavior)
	    VALUES ($1, $2, 'v1', interval '30 seconds', 'fail_closed')`, consumerID, workload.String())
	t.Cleanup(func() {
		exec(t, context.Background(), owner, `DELETE FROM projection.consumer WHERE consumer_id = $1`, consumerID)
	})

	if found, err := records.ConsumerFor(ctx, workload); err != nil || found != consumerID {
		t.Errorf("the registered workload reads as %q, %v; want %q", found, err, consumerID)
	}
	if found, err := records.ConsumerFor(ctx, newID(t)); err != nil || found != "" {
		t.Errorf("an unregistered workload reads as %q, %v", found, err)
	}

	exec(t, ctx, owner, `UPDATE projection.consumer SET retired_at = now() WHERE consumer_id = $1`, consumerID)
	if found, err := records.ConsumerFor(ctx, workload); err != nil || found != "" {
		t.Errorf("a retired consumer reads as %q, %v; retiring it ends its authority", found, err)
	}
}
