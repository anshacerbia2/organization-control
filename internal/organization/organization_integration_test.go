package organization

// Against a real engine as the provider runtime role. `organization.organization` carries no
// tenant column and no policy, so the grant is the only boundary — and `organization_rt` holds
// nothing on it, which the controldb suite asserts separately.

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

const fixedNowText = "2026-08-27T12:13:14Z"

type recorder struct{ calls int }

func (r *recorder) RecordProviderAccess(context.Context, db.ProviderAccess) error {
	r.calls++
	return nil
}

type fixture struct {
	service  *Service
	provider *db.ProviderPool
	ctx      context.Context
	fixed    time.Time
}

func newFixture(t *testing.T) *fixture {
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := fdb.Open(ctx, fdb.Config{
		Name:     "organization-test",
		DSN:      fmt.Sprintf("postgres://organization_provider_app:%s@%s", os.Getenv("TEST_PROVIDER_PASSWORD"), rest),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	provider, err := db.NewProviderPool(pool, &recorder{})
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	service, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	service.now = func() time.Time { return fixed }

	scope, err := db.ProviderScope(mustID(t), mustID(t))
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	return &fixture{service: service, provider: provider, ctx: db.WithScope(ctx, scope), fixed: fixed}
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
	if err := db.WithProviderScope(f.ctx, f.provider, "organization suite fixture",
		func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
		t.Fatalf("fixture statement: %v", err)
	}
}

func (f *fixture) register(t *testing.T, classification Classification, parent *id.UUID) Organization {
	t.Helper()
	record, err := f.service.Register(f.ctx, RegisterRequest{
		DisplayName:    "organization suite " + mustID(t).String(),
		Classification: classification, ParentID: parent,
		Reason: "asserted by the organization suite",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id = $1`, record.OrganizationID.String())
		f.exec(t, `DELETE FROM tenant.tenant WHERE organization_id = $1`, record.OrganizationID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE parent_id = $1`, record.OrganizationID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`, record.OrganizationID.String())
	})
	return record
}

func (f *fixture) seedTenant(t *testing.T, organizationID id.UUID, status string) id.UUID {
	t.Helper()
	tenantID := mustID(t)
	f.exec(t, `INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, 'organization suite tenant', $3, 'pooled')`,
		tenantID.String(), organizationID.String(), status)
	return tenantID
}

func (f *fixture) events(t *testing.T, organizationID id.UUID) []string {
	t.Helper()
	var types []string
	if err := db.WithProviderScope(f.ctx, f.provider, "organization suite read",
		func(ctx context.Context, tx db.Tx) error {
			rows, err := tx.Query(ctx, `SELECT event_type FROM platform.outbox
			    WHERE aggregate_id = $1 ORDER BY sequence`, organizationID.String())
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var t string
				if err := rows.Scan(&t); err != nil {
					return err
				}
				types = append(types, t)
			}
			return rows.Err()
		}); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return types
}

// TestRetiringAnOrganizationDoesNotRetireItsTenants is the refusal that replaces a cascade.
//
// A cascade would take an irreversible action on isolation boundaries as a side effect of a registry
// change, which is the shape of an accidental mass outage. The refusal costs an operator one extra
// deliberate step and names exactly which Tenants to take it on.
func TestRetiringAnOrganizationDoesNotRetireItsTenants(t *testing.T) {
	f := newFixture(t)
	record := f.register(t, ClassificationCustomer, nil)

	active := f.seedTenant(t, record.OrganizationID, "active")
	suspended := f.seedTenant(t, record.OrganizationID, "suspended")

	_, err := f.service.Retire(f.ctx, Command{
		OrganizationID: record.OrganizationID, Reason: "winding down",
		ExpectedVersion: record.Version,
	})
	if !errors.Is(err, ErrTenantsNotRetired) {
		t.Fatalf("error = %v, want ErrTenantsNotRetired", err)
	}
	for _, tenantID := range []id.UUID{active, suspended} {
		if !strings.Contains(err.Error(), tenantID.String()) {
			t.Errorf("the refusal does not name Tenant %s: %v", tenantID, err)
		}
	}

	// The Tenants were not touched, which is the whole point.
	var stillLive int
	if err := db.WithProviderScope(f.ctx, f.provider, "organization suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant
			    WHERE organization_id = $1 AND status <> 'retired'`,
				record.OrganizationID.String()).Scan(&stillLive)
		}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stillLive != 2 {
		t.Errorf("live Tenants = %d, want 2 — the refusal changed something", stillLive)
	}

	// Retiring them deliberately clears the way.
	f.exec(t, `UPDATE tenant.tenant SET status = 'retired' WHERE organization_id = $1`,
		record.OrganizationID.String())
	retired, err := f.service.Retire(f.ctx, Command{
		OrganizationID: record.OrganizationID, Reason: "winding down",
		ExpectedVersion: record.Version,
	})
	if err != nil {
		t.Fatalf("Retire after the Tenants were retired: %v", err)
	}
	if retired.Status != StateRetired {
		t.Errorf("status = %s, want retired", retired.Status)
	}
}

// TestSponsoredTenantsNotRetiredAgreesWithTheGate.
//
// Same query as the gate. A separate one would eventually disagree with the one that actually
// blocks, and then the screen and the refusal would say different things.
func TestSponsoredTenantsNotRetiredAgreesWithTheGate(t *testing.T) {
	f := newFixture(t)
	record := f.register(t, ClassificationCustomer, nil)
	f.seedTenant(t, record.OrganizationID, "active")

	live, err := f.service.SponsoredTenantsNotRetired(f.ctx, record.OrganizationID)
	if err != nil {
		t.Fatalf("SponsoredTenantsNotRetired: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live = %v, want one Tenant", live)
	}

	_, refusal := f.service.Retire(f.ctx, Command{
		OrganizationID: record.OrganizationID, Reason: "winding down",
		ExpectedVersion: record.Version,
	})
	if !errors.Is(refusal, ErrTenantsNotRetired) {
		t.Fatalf("Retire returned %v", refusal)
	}
	if !strings.Contains(refusal.Error(), live[0]) {
		t.Errorf("the refusal does not name %q that the reader reported", live[0])
	}

	// A retired Tenant drops off both.
	f.exec(t, `UPDATE tenant.tenant SET status = 'retired' WHERE organization_id = $1`,
		record.OrganizationID.String())
	if remaining, err := f.service.SponsoredTenantsNotRetired(f.ctx, record.OrganizationID); err != nil {
		t.Fatalf("SponsoredTenantsNotRetired: %v", err)
	} else if len(remaining) != 0 {
		t.Errorf("live = %v after every Tenant retired", remaining)
	}
}

// TestTheRegistryMachineIsWalkedWholeAndRefusesEverythingElse.
func TestTheRegistryMachineIsWalkedWholeAndRefusesEverythingElse(t *testing.T) {
	states := []State{StateActive, StateSuspended, StateRetired}
	permitted := map[string]State{
		"suspend|active":    StateSuspended,
		"restore|suspended": StateActive,
		"retire|active":     StateRetired,
		"retire|suspended":  StateRetired,
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

	// Retired is terminal and says so.
	for _, action := range Actions() {
		if _, err := Resolve(action, StateRetired); !errors.Is(err, ErrRetired) {
			t.Errorf("%s from retired returned %v, want ErrRetired", action, err)
		}
	}
	if _, err := Resolve(Action("merge"), StateActive); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("an action outside the machine returned %v", err)
	}
	if _, err := EventType(Action("merge")); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("EventType for an unknown action returned %v", err)
	}
}

// TestEveryTransitionPublishesOnTheStandardLane. A registry change stops no access, so putting one
// on the reserved lane would let a bulk import delay a live revocation.
func TestEveryTransitionPublishesOnTheStandardLane(t *testing.T) {
	f := newFixture(t)
	record := f.register(t, ClassificationPartner, nil)

	suspended, err := f.service.Suspend(f.ctx, Command{
		OrganizationID: record.OrganizationID, Reason: "under review",
		ExpectedVersion: record.Version,
	})
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if _, err := f.service.Restore(f.ctx, Command{
		OrganizationID: record.OrganizationID, Reason: "review closed",
		ExpectedVersion: suspended.Version,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	want := []string{
		"com.scnehaux.organization.organization.registry.created",
		"com.scnehaux.organization.organization.registry.suspended",
		"com.scnehaux.organization.organization.registry.restored",
	}
	got := f.events(t, record.OrganizationID)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}

	var priorities []int16
	if err := db.WithProviderScope(f.ctx, f.provider, "organization suite read",
		func(ctx context.Context, tx db.Tx) error {
			rows, err := tx.Query(ctx, `SELECT priority FROM platform.outbox
			    WHERE aggregate_id = $1`, record.OrganizationID.String())
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p int16
				if err := rows.Scan(&p); err != nil {
					return err
				}
				priorities = append(priorities, p)
			}
			return rows.Err()
		}); err != nil {
		t.Fatalf("read priorities: %v", err)
	}
	for i, p := range priorities {
		if p == 0 {
			t.Errorf("registry event %d took the reserved dispatch lane", i)
		}
	}
}

// TestAStaleVersionIsRefused, and the refused command changes nothing.
func TestAStaleVersionIsRefused(t *testing.T) {
	f := newFixture(t)
	record := f.register(t, ClassificationCustomer, nil)

	if _, err := f.service.Suspend(f.ctx, Command{
		OrganizationID: record.OrganizationID, Reason: "under review",
		ExpectedVersion: record.Version,
	}); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// A second operator still holding the original version.
	_, err := f.service.Restore(f.ctx, Command{
		OrganizationID: record.OrganizationID, Reason: "review closed",
		ExpectedVersion: record.Version,
	})
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("error = %v, want ErrVersionMismatch", err)
	}
	current, err := f.service.Get(f.ctx, record.OrganizationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if current.Status != StateSuspended {
		t.Errorf("status = %s after a refused restore", current.Status)
	}
}

// TestAChildCannotBeAttachedToARetiredParent.
//
// A live record whose ancestor has been wound down is a hierarchy no report can present coherently.
// `parent_id` carries no Tenant implication either — a child does not inherit its parent's Tenants,
// and nothing here traverses it to decide access.
func TestAChildCannotBeAttachedToARetiredParent(t *testing.T) {
	f := newFixture(t)
	parent := f.register(t, ClassificationProvider, nil)

	// A live parent accepts a child.
	child := f.register(t, ClassificationCustomer, &parent.OrganizationID)
	if child.ParentID == nil || *child.ParentID != parent.OrganizationID {
		t.Fatalf("parent = %v, want %s", child.ParentID, parent.OrganizationID)
	}

	retired, err := f.service.Retire(f.ctx, Command{
		OrganizationID: parent.OrganizationID, Reason: "winding down",
		ExpectedVersion: parent.Version,
	})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if retired.Status != StateRetired {
		t.Fatalf("status = %s", retired.Status)
	}

	if _, err := f.service.Register(f.ctx, RegisterRequest{
		DisplayName: "late child", Classification: ClassificationCustomer,
		ParentID: &parent.OrganizationID, Reason: "asserted by the organization suite",
	}); !errors.Is(err, ErrTransitionRefused) {
		t.Fatalf("attaching to a retired parent returned %v, want ErrTransitionRefused", err)
	}

	// An absent parent is not found rather than silently ignored.
	absent := mustID(t)
	if _, err := f.service.Register(f.ctx, RegisterRequest{
		DisplayName: "orphan", Classification: ClassificationCustomer,
		ParentID: &absent, Reason: "asserted by the organization suite",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("attaching to an absent parent returned %v, want ErrNotFound", err)
	}
}

// TestRegistrationRefusesAnUndeclaredParty.
func TestRegistrationRefusesAnUndeclaredParty(t *testing.T) {
	f := newFixture(t)

	cases := map[string]RegisterRequest{
		"no name":                {Classification: ClassificationCustomer, Reason: "r"},
		"unknown classification": {DisplayName: "x", Classification: "reseller", Reason: "r"},
		"no reason":              {DisplayName: "x", Classification: ClassificationCustomer},
	}
	for name, req := range cases {
		if _, err := f.service.Register(f.ctx, req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// Every declared classification is accepted, so the check is the vocabulary and not a typo.
	for _, classification := range []Classification{
		ClassificationProvider, ClassificationCustomer, ClassificationPartner, ClassificationPublisher,
	} {
		if !classification.Valid() {
			t.Errorf("%s is declared and Valid() rejects it", classification)
		}
		f.register(t, classification, nil)
	}
}

// TestAnAbsentOrganizationIsNotFound.
func TestAnAbsentOrganizationIsNotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Get(f.ctx, mustID(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if _, err := f.service.Suspend(f.ctx, Command{
		OrganizationID: mustID(t), Reason: "r", ExpectedVersion: 1,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}
