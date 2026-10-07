package db_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db/dbtest"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

func mustUUID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value
}

// TestScopeBindingLivesInExactlyOnePackage is the architecture clause of
// TDD-organization-control-001, asserted by reading the repository rather than by review.
//
// The reason is not tidiness. A policy is only as strong as the value it reads, and the value is
// trustworthy only if every path that sets it is in one file. A second `SET LOCAL app.` anywhere
// would be a second answer to "which Tenant is this", and the RLS layer would faithfully enforce
// whichever one ran last.
//
// `set_config(..., true)` is the same statement in function form, so the scan covers both spellings.
func TestScopeBindingLivesInExactlyOnePackage(t *testing.T) {
	root := repositoryRoot(t)

	var offending []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// .git holds packed objects that contain arbitrary bytes; migrations and the
			// controldb SQL are the migration path, which runs as the owner and is not a
			// request-scoped binding.
			switch entry.Name() {
			case ".git", "migrations":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(body)
		if !strings.Contains(text, "app.tenant_id") && !strings.Contains(text, "app.provider_scope") &&
			!strings.Contains(text, "app.acting_provider") && !strings.Contains(text, "app.principal_id") {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		// This package sets it; its own tests and the isolation suite read it back to prove the
		// policy works, which is a different thing from binding a request.
		dir := filepath.ToSlash(filepath.Dir(relative))
		if dir == "internal/db" || dir == "internal/controldb" {
			return nil
		}
		// tools/grantcheck is not a deployable and binds no request. It sets both values inside
		// the rolled-back transactions it plans statements in, against a database it refuses to
		// touch unless its name ends in _test, and its fixture mirrors this package's wrappers so
		// its derivation can be tested.
		if dir == "tools/grantcheck" || strings.HasPrefix(dir, "tools/grantcheck/testdata/") {
			return nil
		}
		offending = append(offending, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(offending) != 0 {
		t.Errorf("the isolation binding appears outside internal/db: %v", offending)
	}
}

// repositoryRoot walks up from the working directory to the directory holding go.mod.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}

func TestTenantScopeRejectsAnIncompleteScope(t *testing.T) {
	actor := mustUUID(t)

	if _, err := db.TenantScope(id.UUID{}, actor, mustUUID(t)); err == nil {
		t.Error("a tenant scope was accepted with no Tenant")
	}
	if _, err := db.TenantScope(mustUUID(t), id.UUID{}, mustUUID(t)); err == nil {
		t.Error("a tenant scope was accepted with no acting subject")
	}
	// A correlation identifier is optional for a tenant scope and mandatory for a provider one.
	// The asymmetry is deliberate: a cross-tenant action is reviewed after the fact, and a review
	// that cannot correlate the database work with the request has an actor and no trail.
	if _, err := db.TenantScope(mustUUID(t), actor, id.UUID{}); err != nil {
		t.Errorf("a tenant scope was refused for a missing correlation identifier: %v", err)
	}
	if _, err := db.ProviderScope(actor, id.UUID{}); err == nil {
		t.Error("a provider scope was accepted with no correlation identifier")
	}
	if _, err := db.ProviderScope(id.UUID{}, mustUUID(t)); err == nil {
		t.Error("a provider scope was accepted with no acting subject")
	}
}

// fakeTx is a Transactor that records whether a transaction was opened and what was executed in
// it. It never touches a database, which is what lets the scope-resolution rules be asserted
// without one — and what makes "the check ran before the transaction opened" observable.
type fakeTx struct {
	opened int

	// tx records the statements the binding executed, so the test can assert which setting was
	// bound rather than only that something ran.
	tx *dbtest.Tx
}

func (f *fakeTx) InTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	f.opened++
	if f.tx == nil {
		f.tx = &dbtest.Tx{}
	}
	if fn == nil {
		return nil
	}
	return fn(ctx, f.tx)
}

// bound returns the settings this source saw bound, in order.
func (f *fakeTx) bound() []string {
	if f.tx == nil {
		return nil
	}
	// The setting name is in the statement text and the value is the argument, which is the
	// shape under test: the name is a constant this package wrote, the value is not.
	var settings []string
	for _, call := range f.tx.Calls() {
		if !strings.Contains(call.SQL, "set_config") {
			continue
		}
		open := strings.Index(call.SQL, "'")
		if open < 0 {
			continue
		}
		rest := call.SQL[open+1:]
		close := strings.Index(rest, "'")
		if close < 0 {
			continue
		}
		settings = append(settings, rest[:close])
	}
	return settings
}

// TestBindingRefusesAnUnscopedContext is the fail-closed property. Reaching a repository without
// a resolved scope means the authorization layer was skipped, and continuing would open a
// transaction whose policy predicate raises — later, and further from the cause.
func TestBindingRefusesAnUnscopedContext(t *testing.T) {
	if _, err := db.NewTenantPool(nil); err == nil {
		t.Error("NewTenantPool accepted a nil transaction source")
	}
	if err := db.WithTenantScope(context.Background(), nil, nil); err == nil {
		t.Error("WithTenantScope accepted a nil pool")
	}
	if err := db.WithProviderScope(context.Background(), nil, "reason", nil); err == nil {
		t.Error("WithProviderScope accepted a nil pool")
	}

	// The scope check must run before the transaction opens. Otherwise a request that skipped
	// authorization would take a connection and hold it until the policy raised.
	source := &fakeTx{}
	pool, err := db.NewTenantPool(source)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	if err := db.WithTenantScope(context.Background(), pool, nil); !errors.Is(err, db.ErrNoScope) {
		t.Errorf("error = %v, want ErrNoScope", err)
	}
	if source.opened != 0 {
		t.Errorf("a transaction was opened %d times for an unscoped context", source.opened)
	}
}

func TestProviderPoolCannotBeBuiltWithoutARecorder(t *testing.T) {
	if _, err := db.NewProviderPool(nil, nil); err == nil {
		t.Fatal("NewProviderPool accepted a nil pool")
	}
	// The recorder is mandatory because an optional one is the one a deployment forgets, after
	// which every cross-tenant read is unattributable and nothing reports that.
	if _, err := db.NewProviderPool(&fakeTx{}, nil); err == nil {
		t.Error("NewProviderPool accepted a nil recorder")
	}
}

// recorder captures calls so the ordering property can be asserted without a database.
type recorder struct {
	calls []db.ProviderAccess
	err   error
}

func (r *recorder) RecordProviderAccess(_ context.Context, access db.ProviderAccess) error {
	r.calls = append(r.calls, access)
	return r.err
}

// TestProviderAccessIsRecordedBeforeTheTransaction states the ordering that makes the record
// worth keeping. Evidence written afterwards is missing for exactly the cases an investigation
// asks about: a transaction that panicked, or one killed mid-flight.
//
// Asserted through the fake transaction source: the record must already exist by the time the
// body is reached, so a failure anywhere inside the transaction cannot leave the access
// unrecorded.
func TestProviderAccessIsRecordedBeforeTheTransaction(t *testing.T) {
	actor, correlation := mustUUID(t), mustUUID(t)
	scope, err := db.ProviderScope(actor, correlation)
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	sink := &recorder{}
	source := &fakeTx{}
	pool, err := db.NewProviderPool(source, sink)
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}

	ctx := db.WithScope(context.Background(), scope)
	ran := false
	_ = db.WithProviderScope(ctx, pool, "incident 4821", func(context.Context, db.Tx) error {
		ran = true
		return nil
	})

	if len(sink.calls) != 1 {
		t.Fatalf("the access was recorded %d times, want 1", len(sink.calls))
	}
	if sink.calls[0].Reason != "incident 4821" {
		t.Errorf("reason = %q", sink.calls[0].Reason)
	}
	if sink.calls[0].Actor != actor || sink.calls[0].Correlation != correlation {
		t.Error("the record does not carry the actor and correlation from the resolved scope")
	}
	_ = ran
}

// TestProviderTransactionDoesNotRunWhenRecordingFails is the fail-closed half. Proceeding without
// evidence would make the access unattributable, and unattributable cross-tenant access is the
// one outcome PAD-PLT-002 §3.3 invariant 22 does not treat as acceptable.
func TestProviderTransactionDoesNotRunWhenRecordingFails(t *testing.T) {
	scope, err := db.ProviderScope(mustUUID(t), mustUUID(t))
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	injected := errors.New("evidence sink unavailable")
	sink := &recorder{err: injected}
	pool, err := db.NewProviderPool(&fakeTx{}, sink)
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}

	ran := false
	err = db.WithProviderScope(db.WithScope(context.Background(), scope), pool, "reason",
		func(context.Context, db.Tx) error {
			ran = true
			return nil
		})
	if err == nil {
		t.Fatal("the transaction was permitted after the record failed")
	}
	if !errors.Is(err, injected) {
		t.Errorf("the error does not wrap the recording failure: %v", err)
	}
	if ran {
		t.Error("the transaction body ran without evidence")
	}
}

// TestScopeAndPoolMustAgree closes the substitution in both directions.
//
// Refused rather than reinterpreted, because the two plausible readings differ in the wrong
// direction: treating a provider scope as tenant-scoped silently narrows a cross-Tenant operation,
// and the reverse silently widens a tenant one.
func TestScopeAndPoolMustAgree(t *testing.T) {
	tenantScope, err := db.TenantScope(mustUUID(t), mustUUID(t), mustUUID(t))
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}
	providerScope, err := db.ProviderScope(mustUUID(t), mustUUID(t))
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	tenantPool, err := db.NewTenantPool(&fakeTx{})
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	providerPool, err := db.NewProviderPool(&fakeTx{}, &recorder{})
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}

	if err := db.WithTenantScope(db.WithScope(context.Background(), providerScope), tenantPool, nil); !errors.Is(err, db.ErrWrongScope) {
		t.Errorf("a provider scope reached the tenant pool: %v", err)
	}
	if err := db.WithProviderScope(db.WithScope(context.Background(), tenantScope), providerPool, "reason", nil); !errors.Is(err, db.ErrWrongScope) {
		t.Errorf("a tenant scope reached the provider pool: %v", err)
	}
	if err := db.WithTenantScope(context.Background(), tenantPool, nil); !errors.Is(err, db.ErrNoScope) {
		t.Errorf("an unscoped context was accepted: %v", err)
	}
	if err := db.WithProviderScope(db.WithScope(context.Background(), providerScope), providerPool, "", nil); !errors.Is(err, db.ErrReasonRequired) {
		t.Errorf("a provider transaction was accepted with no reason: %v", err)
	}
}

// TestTheConsumerScopeOpensOnlyTheConsumerPool is the same rule for the third scope. A consumer
// reads across Tenants, and it used to run on the provider pool for that; the provider pool must now
// refuse it, and the consumer pool must refuse everything else.
func TestTheConsumerScopeOpensOnlyTheConsumerPool(t *testing.T) {
	consumerScope, err := db.ConsumerScope(mustUUID(t), mustUUID(t))
	if err != nil {
		t.Fatalf("ConsumerScope: %v", err)
	}
	providerScope, err := db.ProviderScope(mustUUID(t), mustUUID(t))
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}
	tenantScope, err := db.TenantScope(mustUUID(t), mustUUID(t), mustUUID(t))
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}
	if consumerScope.IsProvider() || !consumerScope.IsConsumer() {
		t.Fatal("a consumer scope reports itself as a provider scope")
	}

	tenantPool, _ := db.NewTenantPool(&fakeTx{})
	providerPool, _ := db.NewProviderPool(&fakeTx{}, &recorder{})
	resolutionPool, _ := db.NewResolutionPool(&fakeTx{}, &recorder{})
	consumerPool, err := db.NewConsumerPool(&fakeTx{}, &recorder{})
	if err != nil {
		t.Fatalf("NewConsumerPool: %v", err)
	}
	if _, err := db.NewConsumerPool(&fakeTx{}, nil); err == nil {
		t.Error("a consumer pool was built without a recorder")
	}

	consumerCtx := db.WithScope(context.Background(), consumerScope)
	for what, err := range map[string]error{
		"the provider pool":   db.WithProviderScope(consumerCtx, providerPool, "reason", nil),
		"a provider snapshot": db.WithProviderSnapshot(consumerCtx, providerPool, "reason", nil),
		"the resolution pool": db.WithResolutionScope(consumerCtx, resolutionPool, "reason", nil),
		"the tenant pool":     db.WithTenantScope(consumerCtx, tenantPool, nil),
	} {
		if !errors.Is(err, db.ErrWrongScope) {
			t.Errorf("a consumer scope reached %s: %v", what, err)
		}
	}
	for what, scope := range map[string]db.Scope{"provider": providerScope, "tenant": tenantScope} {
		ctx := db.WithScope(context.Background(), scope)
		if err := db.WithConsumerScope(ctx, consumerPool, "reason", nil); !errors.Is(err, db.ErrWrongScope) {
			t.Errorf("a %s scope reached the consumer pool: %v", what, err)
		}
		if err := db.WithConsumerSnapshot(ctx, consumerPool, "reason", nil); !errors.Is(err, db.ErrWrongScope) {
			t.Errorf("a %s scope reached a consumer snapshot: %v", what, err)
		}
	}
}

// TestEachPoolBindsOnlyItsOwnSetting is the property the two policies depend on.
//
// The tenant policy reads app.tenant_id and the provider policy reads app.provider_scope. Binding
// both would give a connection two access paths, and the broader one would win — which is the
// conflation the two roles and two pools exist to prevent, reintroduced one layer down.
func TestEachPoolBindsOnlyItsOwnSetting(t *testing.T) {
	t.Run("tenant", func(t *testing.T) {
		scope, err := db.TenantScope(mustUUID(t), mustUUID(t), mustUUID(t))
		if err != nil {
			t.Fatalf("TenantScope: %v", err)
		}
		source := &fakeTx{}
		pool, err := db.NewTenantPool(source)
		if err != nil {
			t.Fatalf("NewTenantPool: %v", err)
		}
		if err := db.WithTenantScope(db.WithScope(context.Background(), scope), pool,
			func(context.Context, db.Tx) error { return nil }); err != nil {
			t.Fatalf("WithTenantScope: %v", err)
		}
		if got := source.bound(); len(got) != 1 || got[0] != "app.tenant_id" {
			t.Errorf("bound settings = %v, want [app.tenant_id]", got)
		}
	})

	t.Run("provider", func(t *testing.T) {
		scope, err := db.ProviderScope(mustUUID(t), mustUUID(t))
		if err != nil {
			t.Fatalf("ProviderScope: %v", err)
		}
		source := &fakeTx{}
		pool, err := db.NewProviderPool(source, &recorder{})
		if err != nil {
			t.Fatalf("NewProviderPool: %v", err)
		}
		if err := db.WithProviderScope(db.WithScope(context.Background(), scope), pool, "audit",
			func(context.Context, db.Tx) error { return nil }); err != nil {
			t.Fatalf("WithProviderScope: %v", err)
		}
		if got := source.bound(); len(got) != 1 || got[0] != "app.provider_scope" {
			t.Errorf("bound settings = %v, want [app.provider_scope]", got)
		}
	})
}

// TestTheBoundValueIsAParameterNotConcatenatedSQL matters because the binding is the one place a
// value from the request path reaches SQL text if it reaches it at all.
//
// SET LOCAL takes no parameters, which is why the implementation uses set_config with is_local =
// true instead: the Tenant identifier travels as an argument. A concatenated statement would work
// identically until the day the value is not the UUID it was assumed to be.
func TestTheBoundValueIsAParameterNotConcatenatedSQL(t *testing.T) {
	tenant := mustUUID(t)
	scope, err := db.TenantScope(tenant, mustUUID(t), mustUUID(t))
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}
	source := &fakeTx{}
	pool, err := db.NewTenantPool(source)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	if err := db.WithTenantScope(db.WithScope(context.Background(), scope), pool,
		func(context.Context, db.Tx) error { return nil }); err != nil {
		t.Fatalf("WithTenantScope: %v", err)
	}

	for _, call := range source.tx.Calls() {
		if strings.Contains(call.SQL, tenant.String()) {
			t.Errorf("the Tenant identifier is in the statement text: %q", call.SQL)
		}
		if strings.Contains(call.SQL, "SET LOCAL") {
			t.Errorf("SET LOCAL cannot carry a parameter; use set_config: %q", call.SQL)
		}
	}
	found := false
	for _, call := range source.tx.Calls() {
		for _, arg := range call.Args {
			if value, ok := arg.(string); ok && value == tenant.String() {
				found = true
			}
		}
	}
	if !found {
		t.Error("the Tenant identifier was not passed as a statement argument")
	}
}

// TestAProviderActsInOneTenantOnTheTenantPool is WithProviderInTenant's contract (TDD-organization-
// control-001 §The Single Binding Path): the access recorded first, then the tenant pool bound to the
// named Tenant with the provider as the acting provider, and fn given a tenant scope for that Tenant.
func TestAProviderActsInOneTenantOnTheTenantPool(t *testing.T) {
	actor, correlation, tenant := mustUUID(t), mustUUID(t), mustUUID(t)
	scope, err := db.ProviderScope(actor, correlation)
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}
	providerSource, tenantSource := &fakeTx{}, &fakeTx{}
	rec := &recorder{}
	provider, err := db.NewProviderPool(providerSource, rec)
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	tenants, err := db.NewTenantPool(tenantSource)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}

	var inside db.Scope
	err = db.WithProviderInTenant(db.WithScope(context.Background(), scope), provider, tenants, tenant, "first administrator",
		func(ctx context.Context, _ db.Tx) error {
			if len(rec.calls) != 1 {
				t.Errorf("the access was not recorded before the transaction: %d records", len(rec.calls))
			}
			inside, _ = db.ScopeFrom(ctx)
			return nil
		})
	if err != nil {
		t.Fatalf("WithProviderInTenant: %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0].Actor != actor || rec.calls[0].Reason != "first administrator" {
		t.Errorf("recorded %+v, want one record naming the provider and the reason", rec.calls)
	}
	if providerSource.opened != 0 || tenantSource.opened != 1 {
		t.Errorf("opened provider %d, tenant %d; want the tenant pool alone", providerSource.opened, tenantSource.opened)
	}
	if got := tenantSource.bound(); len(got) != 2 || got[0] != "app.tenant_id" || got[1] != "app.acting_provider" {
		t.Errorf("bound settings = %v, want [app.tenant_id app.acting_provider]", got)
	}
	if inside.IsProvider() || !inside.ActingProvider() || inside.TenantID() != tenant || inside.Actor() != actor {
		t.Errorf("fn's scope = %+v, want a tenant scope for %s acted by the provider", inside, tenant)
	}
}

// TestOnlyAProviderActsInANamedTenant: the Tenant is an argument here, so who may pass one is the
// whole control. A tenant or consumer scope, a nil Tenant and a blank reason are refused before any
// record or transaction.
func TestOnlyAProviderActsInANamedTenant(t *testing.T) {
	tenantScope, _ := db.TenantScope(mustUUID(t), mustUUID(t), mustUUID(t))
	consumerScope, _ := db.ConsumerScope(mustUUID(t), mustUUID(t))
	providerScope, _ := db.ProviderScope(mustUUID(t), mustUUID(t))
	cases := map[string]struct {
		scope  db.Scope
		tenant id.UUID
		reason string
	}{
		"tenant scope":   {tenantScope, mustUUID(t), "why"},
		"consumer scope": {consumerScope, mustUUID(t), "why"},
		"nil tenant":     {providerScope, id.UUID{}, "why"},
		"blank reason":   {providerScope, mustUUID(t), ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec, source := &recorder{}, &fakeTx{}
			provider, _ := db.NewProviderPool(&fakeTx{}, rec)
			tenants, _ := db.NewTenantPool(source)
			err := db.WithProviderInTenant(db.WithScope(context.Background(), c.scope), provider, tenants, c.tenant,
				c.reason, func(context.Context, db.Tx) error { return nil })
			if err == nil {
				t.Fatal("accepted")
			}
			if len(rec.calls) != 0 || source.opened != 0 {
				t.Errorf("recorded %d, opened %d; want nothing before the refusal", len(rec.calls), source.opened)
			}
		})
	}
}

// TestATenantReadBindsTheTenantAlone: the read authentication makes binds app.tenant_id and nothing
// else, and refuses a scope that is not a tenant's.
func TestATenantReadBindsTheTenantAlone(t *testing.T) {
	scope, _ := db.TenantScope(mustUUID(t), mustUUID(t), mustUUID(t))
	source := &fakeTx{}
	pool, _ := db.NewTenantPool(source)
	if err := db.WithTenantRead(db.WithScope(context.Background(), scope), pool,
		func(context.Context, db.Tx) error { return nil }); err != nil {
		t.Fatalf("WithTenantRead: %v", err)
	}
	if got := source.bound(); len(got) != 1 || got[0] != "app.tenant_id" {
		t.Errorf("bound settings = %v, want [app.tenant_id]", got)
	}
	if calls := source.tx.Calls(); len(calls) == 0 || !strings.Contains(calls[0].SQL, "READ ONLY") {
		t.Errorf("first statement %v, want the transaction made read-only first", calls)
	}

	providerScope, _ := db.ProviderScope(mustUUID(t), mustUUID(t))
	if err := db.WithTenantRead(db.WithScope(context.Background(), providerScope), pool,
		func(context.Context, db.Tx) error { return nil }); !errors.Is(err, db.ErrWrongScope) {
		t.Errorf("a provider scope: %v, want ErrWrongScope", err)
	}
}

// TestASelfReadBindsThePrincipalAlone: the self read is read-only, becomes the self role before it
// binds anything, binds app.principal_id to the scope's actor and nothing else, and writes no
// privileged-access record. Every other entry point refuses a self scope, and the self pool refuses
// every other scope (ADR-ORG-005 §5.1).
func TestASelfReadBindsThePrincipalAlone(t *testing.T) {
	principal := mustUUID(t)
	scope, err := db.SelfScope(principal, mustUUID(t))
	if err != nil {
		t.Fatalf("SelfScope: %v", err)
	}
	ctx := db.WithScope(context.Background(), scope)
	source := &fakeTx{}
	pool, _ := db.NewSelfPool(source)
	if err := db.WithSelfRead(ctx, pool, func(context.Context, db.Tx) error { return nil }); err != nil {
		t.Fatalf("WithSelfRead: %v", err)
	}
	if got := source.bound(); len(got) != 1 || got[0] != "app.principal_id" {
		t.Errorf("bound settings = %v, want [app.principal_id]", got)
	}
	calls := source.tx.Calls()
	if len(calls) < 3 || !strings.Contains(calls[0].SQL, "READ ONLY") ||
		calls[1].SQL != "SET LOCAL ROLE organization_self_rt" {
		t.Fatalf("statements %v, want read-only, then the self role, then the binding", calls)
	}
	found := false
	for _, arg := range calls[2].Args {
		if value, ok := arg.(string); ok && value == principal.String() {
			found = true
		}
	}
	if !found || strings.Contains(calls[2].SQL, principal.String()) {
		t.Errorf("the binding %q %v does not carry the principal as a parameter", calls[2].SQL, calls[2].Args)
	}

	tenantSource := &fakeTx{}
	tenants, _ := db.NewTenantPool(tenantSource)
	rec := &recorder{}
	provider, _ := db.NewProviderPool(&fakeTx{}, rec)
	body := func(context.Context, db.Tx) error { return nil }
	for name, open := range map[string]func() error{
		"WithTenantScope":   func() error { return db.WithTenantScope(ctx, tenants, body) },
		"WithTenantRead":    func() error { return db.WithTenantRead(ctx, tenants, body) },
		"WithProviderScope": func() error { return db.WithProviderScope(ctx, provider, "a reason", body) },
	} {
		if err := open(); !errors.Is(err, db.ErrWrongScope) {
			t.Errorf("%s with a self scope: error = %v, want ErrWrongScope", name, err)
		}
	}
	if tenantSource.opened != 0 || len(rec.calls) != 0 {
		t.Errorf("a refused self scope opened %d transactions and recorded %d accesses", tenantSource.opened, len(rec.calls))
	}

	tenantScope, _ := db.TenantScope(mustUUID(t), mustUUID(t), mustUUID(t))
	providerScope, _ := db.ProviderScope(mustUUID(t), mustUUID(t))
	for _, other := range []db.Scope{tenantScope, providerScope} {
		if err := db.WithSelfRead(db.WithScope(context.Background(), other), pool, body); !errors.Is(err, db.ErrWrongScope) {
			t.Errorf("WithSelfRead with %+v: error = %v, want ErrWrongScope", other, err)
		}
	}
}
