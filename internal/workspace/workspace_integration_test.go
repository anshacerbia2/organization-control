package workspace

// Against a real engine as the tenant-scoped runtime role, because that is the role this service
// runs as and the one the policy constrains. On an owning connection the cross-Tenant assertion
// below would pass while proving nothing.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

const fixedNowText = "2026-08-27T13:14:15Z"

type recorder struct{}

func (recorder) RecordProviderAccess(context.Context, db.ProviderAccess) error { return nil }

type fixture struct {
	service     *Service
	provider    *db.ProviderPool
	providerCtx context.Context
	scopeFor    func(id.UUID) context.Context
	fixed       time.Time
}

func hostFrom(t *testing.T) string {
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
	return rest
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	host := hostFrom(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	runtimePool, err := fdb.Open(ctx, fdb.Config{
		Name:     "workspace-runtime",
		DSN:      fmt.Sprintf("postgres://organization_app:%s@%s", os.Getenv("TEST_RUNTIME_PASSWORD"), host),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	t.Cleanup(runtimePool.Close)

	// The provider pool is the fixture's, not the service's. Seeding a Tenant and reading across
	// Tenants is provider work; the service under test never gets it.
	providerPool, err := fdb.Open(ctx, fdb.Config{
		Name:     "workspace-provider",
		DSN:      fmt.Sprintf("postgres://organization_provider_app:%s@%s", os.Getenv("TEST_PROVIDER_PASSWORD"), host),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("open provider pool: %v", err)
	}
	t.Cleanup(providerPool.Close)

	tenantPool, err := db.NewTenantPool(runtimePool)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	provider, err := db.NewProviderPool(providerPool, recorder{})
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	service, err := New(tenantPool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	service.now = func() time.Time { return fixed }

	actor, correlation := mustID(t), mustID(t)
	providerScope, err := db.ProviderScope(actor, correlation)
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	return &fixture{
		service: service, provider: provider,
		providerCtx: db.WithScope(ctx, providerScope),
		scopeFor: func(tenantID id.UUID) context.Context {
			scope, err := db.TenantScope(tenantID, actor, correlation)
			if err != nil {
				t.Fatalf("TenantScope: %v", err)
			}
			return db.WithScope(ctx, scope)
		},
		fixed: fixed,
	}
}

func mustID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value
}

func (f *fixture) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if err := db.WithProviderScope(f.providerCtx, f.provider, "workspace suite fixture",
		func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
		t.Fatalf("fixture statement: %v", err)
	}
}

func (f *fixture) seedTenant(t *testing.T) id.UUID {
	t.Helper()
	organizationID, tenantID := mustID(t), mustID(t)
	f.exec(t, `INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'workspace suite', 'customer', 'active')`, organizationID.String())
	f.exec(t, `INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, 'workspace suite', 'active', 'pooled')`, tenantID.String(), organizationID.String())
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id IN (
		    SELECT workspace_id FROM workspace.workspace WHERE tenant_id = $1)`, tenantID.String())
		f.exec(t, `DELETE FROM membership.membership WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM workspace.workspace WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})
	return tenantID
}

func (f *fixture) create(t *testing.T, tenantID id.UUID) Workspace {
	t.Helper()
	record, err := f.service.Create(f.scopeFor(tenantID), CreateRequest{
		DisplayName: "suite workspace", Type: "team",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return record
}

// TestArchiveIsReversibleAndRetirementIsNot is the ordering the lifecycle exists for.
//
// Archiving is reversible and immediate, retirement is neither. Collapsing them would make every
// mistaken retirement irreversible, so retirement is reachable only from archived.
func TestArchiveIsReversibleAndRetirementIsNot(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t)
	ctx := f.scopeFor(tenantID)
	record := f.create(t, tenantID)

	if record.Status != StateActive || record.TenantID != tenantID {
		t.Fatalf("created = %+v", record)
	}

	// Retirement straight from active is refused.
	if _, err := f.service.Retire(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: record.Version,
	}); !errors.Is(err, ErrTransitionRefused) {
		t.Fatalf("retiring an active Workspace returned %v, want ErrTransitionRefused", err)
	}

	archived, err := f.service.Archive(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: record.Version,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if archived.Status != StateArchived {
		t.Errorf("status = %s, want archived", archived.Status)
	}

	restored, err := f.service.Restore(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: archived.Version,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Status != StateActive {
		t.Errorf("status = %s, want active", restored.Status)
	}

	again, err := f.service.Archive(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: restored.Version,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	retired, err := f.service.Retire(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: again.Version,
	})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if retired.Status != StateRetired {
		t.Errorf("status = %s, want retired", retired.Status)
	}

	// Terminal, and it says so.
	if _, err := f.service.Restore(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: retired.Version,
	}); !errors.Is(err, ErrRetired) {
		t.Errorf("restoring a retired Workspace returned %v, want ErrRetired", err)
	}

	// The version increased at every transition.
	if retired.Version <= again.Version || again.Version <= restored.Version ||
		restored.Version <= archived.Version || archived.Version <= record.Version {
		t.Error("a transition did not increase the version")
	}
}

// TestRetirementIsRefusedWhileAMembershipStillReferencesIt.
//
// The composite foreign key would refuse a delete, but nothing deletes here — the row is retired in
// place, so the constraint never fires. Without this gate a retired Workspace would leave every
// Membership scoped to a context that no longer exists.
func TestRetirementIsRefusedWhileAMembershipStillReferencesIt(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t)
	ctx := f.scopeFor(tenantID)
	record := f.create(t, tenantID)

	membershipID := mustID(t)
	f.exec(t, `INSERT INTO membership.membership
	    (membership_id, principal_id, tenant_id, workspace_id, subject_type, status, membership_version, valid_from, provenance)
	    VALUES ($1, $2, $3, $4, 'human', 'active', 1, now(), 'workspace suite')`,
		membershipID.String(), mustID(t).String(), tenantID.String(), record.WorkspaceID.String())

	archived, err := f.service.Archive(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: record.Version,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Archiving is permitted with Memberships present: it is reversible and stops nothing.
	referencing, err := f.service.ReferencingMemberships(ctx, record.WorkspaceID)
	if err != nil {
		t.Fatalf("ReferencingMemberships: %v", err)
	}
	if len(referencing) != 1 {
		t.Fatalf("referencing = %v, want one", referencing)
	}

	_, err = f.service.Retire(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: archived.Version,
	})
	if !errors.Is(err, ErrMembershipsPresent) {
		t.Fatalf("error = %v, want ErrMembershipsPresent", err)
	}
	if !strings.Contains(err.Error(), membershipID.String()) {
		t.Errorf("the refusal does not name the Membership: %v", err)
	}
	if !strings.Contains(err.Error(), referencing[0]) {
		t.Errorf("the refusal and the reader disagree: %q vs %v", referencing[0], err)
	}

	// A revoked Membership is a historical record that grants nothing, so it stops blocking.
	// Otherwise a Workspace becomes unretireable forever once anybody's access there was withdrawn.
	f.exec(t, `UPDATE membership.membership SET status = 'revoked' WHERE membership_id = $1`,
		membershipID.String())
	if remaining, err := f.service.ReferencingMemberships(ctx, record.WorkspaceID); err != nil {
		t.Fatalf("ReferencingMemberships: %v", err)
	} else if len(remaining) != 0 {
		t.Errorf("referencing = %v after the Membership was revoked", remaining)
	}
	if _, err := f.service.Retire(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: archived.Version,
	}); err != nil {
		t.Fatalf("Retire after the Membership was revoked: %v", err)
	}
}

// TestAWorkspaceInAnotherTenantIsNotFound.
//
// Under Row-Level Security the row is simply absent, which is the correct answer: reporting that it
// exists elsewhere would disclose a row this caller may not read.
func TestAWorkspaceInAnotherTenantIsNotFound(t *testing.T) {
	f := newFixture(t)
	ownerTenant := f.seedTenant(t)
	otherTenant := f.seedTenant(t)
	record := f.create(t, ownerTenant)

	otherCtx := f.scopeFor(otherTenant)
	if _, err := f.service.Get(otherCtx, record.WorkspaceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if _, err := f.service.Archive(otherCtx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: record.Version,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if err := errors.Unwrap(fmt.Errorf("x: %w", ErrNotFound)); err == nil {
		t.Error("the sentinel does not unwrap")
	}

	// Still active for its own Tenant: the refused attempt changed nothing.
	current, err := f.service.Get(f.scopeFor(ownerTenant), record.WorkspaceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if current.Status != StateActive {
		t.Errorf("status = %s after a cross-tenant attempt", current.Status)
	}
}

// TestCreateRefusesATenantOtherThanTheBoundOne is SAD-004 §8.3 at the service layer: a Tenant
// identifier arriving with a request is a *requested* scope, refused before any statement runs.
func TestCreateRefusesATenantOtherThanTheBoundOne(t *testing.T) {
	f := newFixture(t)
	bound := f.seedTenant(t)
	other := f.seedTenant(t)

	if _, err := f.service.Create(f.scopeFor(bound), CreateRequest{
		DisplayName: "elsewhere", Type: "team", TenantID: other,
	}); err == nil {
		t.Fatal("a create naming another Tenant was accepted")
	}

	// Naming the bound Tenant explicitly is accepted, so the check is the mismatch and not the
	// presence of the field.
	if _, err := f.service.Create(f.scopeFor(bound), CreateRequest{
		DisplayName: "here", Type: "team", TenantID: bound,
	}); err != nil {
		t.Fatalf("naming the bound Tenant was refused: %v", err)
	}
}

// TestTheMachineIsWalkedWholeAndRefusesEverythingElse.
func TestTheMachineIsWalkedWholeAndRefusesEverythingElse(t *testing.T) {
	states := []State{StateActive, StateArchived, StateRetired}
	permitted := map[string]State{
		"archive|active":   StateArchived,
		"restore|archived": StateActive,
		"retire|archived":  StateRetired,
	}

	covered := map[string]bool{}
	for _, action := range Actions() {
		for _, from := range states {
			key := string(action) + "|" + string(from)
			next, err := Resolve(action, from)
			want, allowed := permitted[key]
			if allowed {
				covered[key] = true
				if err != nil {
					t.Errorf("%s from %s was refused: %v", action, from, err)
				}
				if next != want {
					t.Errorf("%s from %s went to %s, want %s", action, from, next, want)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s from %s was permitted and went to %s", action, from, next)
			}
		}
	}
	for key := range permitted {
		if !covered[key] {
			t.Errorf("%q is expected to be permitted and is not in the machine", key)
		}
	}

	for _, state := range states {
		if !state.Valid() {
			t.Errorf("%s is in the machine and Valid() rejects it", state)
		}
	}
	if _, err := Resolve(Action("move"), StateActive); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("an action outside the machine returned %v", err)
	}

	// Every action publishes, and every type is valid under the platform grammar.
	for _, action := range Actions() {
		eventType, err := EventType(action)
		if err != nil {
			t.Errorf("EventType(%s): %v", action, err)
			continue
		}
		if !strings.Contains(string(eventType), ".workspace.lifecycle.") {
			t.Errorf("%s publishes %q, which is not a Workspace lifecycle type", action, eventType)
		}
	}
}

// TestEveryChangePublishesWithItsTenant. A consumer keying a Workspace without its Tenant holds an
// identifier it cannot scope, and every Membership referencing it is scoped by the pair.
func TestEveryChangePublishesWithItsTenant(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t)
	ctx := f.scopeFor(tenantID)
	record := f.create(t, tenantID)
	if _, err := f.service.Archive(ctx, Command{
		WorkspaceID: record.WorkspaceID, ExpectedVersion: record.Version,
	}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	type row struct {
		eventType string
		tenant    string
		priority  int16
	}
	var rows []row
	if err := db.WithProviderScope(f.providerCtx, f.provider, "workspace suite read",
		func(ctx context.Context, tx db.Tx) error {
			result, err := tx.Query(ctx, `SELECT event_type, envelope->'data'->>'tenant_id', priority
			    FROM platform.outbox WHERE aggregate_id = $1 ORDER BY sequence`,
				record.WorkspaceID.String())
			if err != nil {
				return err
			}
			defer result.Close()
			for result.Next() {
				var next row
				if err := result.Scan(&next.eventType, &next.tenant, &next.priority); err != nil {
					return err
				}
				rows = append(rows, next)
			}
			return result.Err()
		}); err != nil {
		t.Fatalf("read events: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("events = %+v, want created and archived", rows)
	}
	for i, r := range rows {
		if r.tenant != tenantID.String() {
			t.Errorf("event %d carries tenant %q, want %s", i, r.tenant, tenantID)
		}
		if r.priority == 0 {
			t.Errorf("event %d took the reserved dispatch lane; archiving withdraws no Membership", i)
		}
	}
}

// TestCreateRefusesAnIncompleteWorkspace.
func TestCreateRefusesAnIncompleteWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := f.scopeFor(f.seedTenant(t))

	for name, req := range map[string]CreateRequest{
		"no name": {Type: "team"},
		"no type": {DisplayName: "x"},
	} {
		if _, err := f.service.Create(ctx, req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// And an unbound context is refused rather than defaulting to some Tenant.
	if _, err := f.service.Create(context.Background(), CreateRequest{
		DisplayName: "x", Type: "team",
	}); !errors.Is(err, db.ErrNoScope) {
		t.Errorf("an unbound create returned %v, want ErrNoScope", err)
	}
}
