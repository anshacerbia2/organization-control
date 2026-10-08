package membership_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/membership"
)

// untouchable fails the test if a transaction is opened on it.
type untouchable struct{ t *testing.T }

func (u untouchable) InTx(context.Context, func(context.Context, db.Tx) error) error {
	u.t.Helper()
	u.t.Error("a malformed request opened a transaction before being refused")
	return errors.New("the transactor must not be reached")
}

// TestAMalformedCommandOpensNoTransaction: a request that is wrong on its face is refused before a
// connection is taken, a scope bound or an idempotency key claimed. Validation that depends on
// stored state stays inside the transaction, where the state is read.
func TestAMalformedCommandOpensNoTransaction(t *testing.T) {
	pool, err := db.NewTenantPool(untouchable{t})
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	service, err := membership.New(pool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenant, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	scope, err := db.TenantScope(tenant, tenant, id.UUID{})
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}
	ctx := db.WithScope(context.Background(), scope)

	principal, _ := id.NewV7()
	grants := map[string]membership.GrantRequest{
		"no principal":  {SubjectType: "human", Provenance: "test", ValidFrom: time.Now()},
		"bad subject":   {PrincipalID: principal, SubjectType: "robot", Provenance: "test", ValidFrom: time.Now()},
		"no provenance": {PrincipalID: principal, SubjectType: "human", ValidFrom: time.Now()},
		"no valid_from": {PrincipalID: principal, SubjectType: "human", Provenance: "test"},
		"another tenant": {PrincipalID: principal, TenantID: principal, SubjectType: "human",
			Provenance: "test", ValidFrom: time.Now()},
	}
	for name, req := range grants {
		if _, err := service.Grant(ctx, req); !errors.Is(err, membership.ErrInvalid) {
			t.Errorf("grant, %s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := service.Revoke(ctx, membership.Command{MembershipID: principal, ExpectedVersion: 1}); !errors.Is(err, membership.ErrReasonRequired) {
		t.Errorf("revoke without a reason: %v, want ErrReasonRequired", err)
	}
	if _, err := service.Suspend(ctx, membership.Command{MembershipID: principal}); !errors.Is(err, membership.ErrInvalid) {
		t.Errorf("suspend without a version: %v, want ErrInvalid", err)
	}
}
