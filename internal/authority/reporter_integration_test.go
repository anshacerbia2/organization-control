package authority

// The reporter registry, against the real engine as the provider role (ADR-ORG-001 §5.11, amended
// 2026-10-20).

import (
	"context"
	"errors"
	"testing"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

func reporters(t *testing.T) (*Reporters, *Reader, context.Context, id.UUID) {
	t.Helper()
	provider, owner, ctx := pools(t)
	pool, err := db.NewProviderPool(provider, noEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewReporters(pool)
	if err != nil {
		t.Fatal(err)
	}
	records, err := NewReader(provider)
	if err != nil {
		t.Fatal(err)
	}
	actor := newID(t)
	t.Cleanup(func() {
		if err := owner.InTx(context.Background(), func(ctx context.Context, tx fdb.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM organization.reporter WHERE registered_by = $1`, actor.String())
			return err
		}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return registry, records, actingAs(t, ctx, actor), actor
}

func TestAReporterIsRegisteredReadAndRetired(t *testing.T) {
	registry, records, ctx, actor := reporters(t)
	workload := newID(t)

	reporter, err := registry.Register(ctx, ReporterRegistration{Principal: workload, Reports: ReportsProvisioning,
		Purpose: "the development server's provisioning stand-in", Reason: "wiring the development server"})
	if err != nil || reporter.Principal != workload || reporter.RegisteredBy != actor || reporter.Version != 1 ||
		!reporter.Active() {
		t.Fatalf("Register answered %+v, %v", reporter, err)
	}
	if found, reports, err := records.ReporterFor(ctx, workload); err != nil || found != reporter.ID ||
		!reports.Provisioning() {
		t.Fatalf("ReporterFor answered %s %q %v; want the registration", found, reports, err)
	}
	if _, err := registry.Register(ctx, ReporterRegistration{Principal: workload, Reports: "obligation:billing",
		Purpose: "again", Reason: "again"}); !errors.Is(err, ErrReporterRegistered) {
		t.Errorf("a second registration of the workload answered %v, want ErrReporterRegistered", err)
	}

	page, err := registry.List(ctx, ReporterQuery{Reports: ReportsProvisioning, State: "active", Limit: 100}, "reading")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	listed := false
	for _, item := range page.Reporters {
		listed = listed || item.ID == reporter.ID
	}
	if !listed {
		t.Error("the active provisioning reporter is not listed")
	}

	if _, err := registry.Retire(ctx, reporter.ID, 2, "stale view"); !errors.Is(err, ErrReporterVersion) {
		t.Errorf("a stale retirement answered %v, want ErrReporterVersion", err)
	}
	retired, err := registry.Retire(ctx, reporter.ID, 1, "the stand-in is replaced")
	if err != nil || retired.Active() || retired.RetiredBy == nil || *retired.RetiredBy != actor || retired.Version != 2 {
		t.Fatalf("Retire answered %+v, %v", retired, err)
	}
	if found, _, err := records.ReporterFor(ctx, workload); err != nil || !found.IsNil() {
		t.Errorf("a retired reporter still admits its workload: %s, %v", found, err)
	}
	if _, err := registry.Retire(ctx, reporter.ID, 2, "again"); !errors.Is(err, ErrReporterRetired) {
		t.Errorf("a second retirement answered %v, want ErrReporterRetired", err)
	}
	if _, err := registry.Get(ctx, newID(t), "reading"); !errors.Is(err, ErrReporterNotFound) {
		t.Errorf("an unknown reporter answered %v, want ErrReporterNotFound", err)
	}
}

func TestAReporterRegistrationIsChecked(t *testing.T) {
	registry, _, ctx, _ := reporters(t)
	for name, req := range map[string]ReporterRegistration{
		"no principal":     {Reports: ReportsProvisioning, Purpose: "p", Reason: "r"},
		"unknown report":   {Principal: newID(t), Reports: "billing", Purpose: "p", Reason: "r"},
		"domain with caps": {Principal: newID(t), Reports: "obligation:Billing", Purpose: "p", Reason: "r"},
		"empty domain":     {Principal: newID(t), Reports: "obligation:", Purpose: "p", Reason: "r"},
		"no purpose":       {Principal: newID(t), Reports: ReportsProvisioning, Reason: "r"},
		"no reason":        {Principal: newID(t), Reports: ReportsProvisioning, Purpose: "p"},
	} {
		if _, err := registry.Register(ctx, req); !errors.Is(err, ErrReporterInvalid) {
			t.Errorf("%s: answered %v, want ErrReporterInvalid", name, err)
		}
	}
}

// TestTheProviderRoleCannotRewriteAReporter: who was registered, by whom and why stays as written.
func TestTheProviderRoleCannotRewriteAReporter(t *testing.T) {
	registry, _, ctx, _ := reporters(t)
	reporter, err := registry.Register(ctx, ReporterRegistration{Principal: newID(t), Reports: "obligation:billing",
		Purpose: "billing's obligations", Reason: "wiring"})
	if err != nil {
		t.Fatal(err)
	}
	provider, _, _ := pools(t)
	for _, statement := range []string{
		`UPDATE organization.reporter SET reports = 'provisioning' WHERE reporter_id = $1`,
		`UPDATE organization.reporter SET principal_id = gen_random_uuid() WHERE reporter_id = $1`,
		`DELETE FROM organization.reporter WHERE reporter_id = $1`,
	} {
		if err := provider.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			_, err := tx.Exec(ctx, statement, reporter.ID.String())
			return err
		}); err == nil {
			t.Errorf("the provider role ran %q", statement)
		}
	}
}
