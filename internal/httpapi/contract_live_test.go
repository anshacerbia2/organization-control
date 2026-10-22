package httpapi

// The success exchanges of the contract test (ROADMAP item 62, ADR-GLB-010 §5.1). The route tests in
// routes_test.go stop every request before the database, so none of them produces a success body.
// These run the same handlers on the services production wires, against the integration database
// (TEST_DATABASE_URL), each pool on the login role production uses. Every exchange goes through
// serveChecked, so its request and response are validated against api/openapi.yaml.

import (
	stdcontext "context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/observability"

	"github.com/anshacerbia2/organization-control/internal/authority"
	occontext "github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

// liveReason is the X-Administrative-Reason every live exchange carries.
const liveReason = "contract test"

// matchingAddresses answers Identity Control's address match: every principal holds the address.
type matchingAddresses struct{}

func (matchingAddresses) Match(stdcontext.Context, id.UUID, string) (bool, error) { return true, nil }

type live struct {
	t       *testing.T
	owner   *fdb.Pool
	surface Surface
}

// liveDSN is the integration database as user, with the password the named variable holds.
func liveDSN(t *testing.T, base, user, passwordVariable string) string {
	t.Helper()
	password := os.Getenv(passwordVariable)
	if password == "" {
		t.Fatalf("%s is unset: the live contract test connects as %s", passwordVariable, user)
	}
	rest := base[strings.Index(base, "://")+3:]
	rest = rest[strings.Index(rest, "@")+1:]
	return fmt.Sprintf("postgres://%s:%s@%s", user, password, rest)
}

func liveSurface(t *testing.T) *live {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL is unset")
	}
	ctx := stdcontext.Background()
	open := func(name, dsn string) *fdb.Pool {
		pool, err := fdb.Open(ctx, fdb.Config{Name: name, DSN: dsn, MaxConns: 4})
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	owner := open("contract-owner", base)
	tenantConns := open("contract-tenant", liveDSN(t, base, "organization_app", "TEST_RUNTIME_PASSWORD"))
	providerConns := open("contract-provider", liveDSN(t, base, "organization_provider_app", "TEST_PROVIDER_PASSWORD"))
	resolutionConns := open("contract-resolution", liveDSN(t, base, "organization_resolution_app", "TEST_RESOLUTION_PASSWORD"))
	consumerConns := open("contract-consumer", liveDSN(t, base, "organization_consumer_app", "TEST_CONSUMER_PASSWORD"))

	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// As cmd/organization-control/main.go wires them, but for the privileged-access recorder:
	// internal/access is not an edge of this package (arch.json), and its records are its own suite's.
	recorder := stubRecorder{}
	tenantPool, err := db.NewTenantPool(tenantConns)
	must(err, "tenant pool")
	providerPool, err := db.NewProviderPool(providerConns, recorder)
	must(err, "provider pool")
	memberships, err := membership.New(tenantPool, membership.WithProviderPool(providerPool))
	must(err, "membership service")
	tenants, err := tenant.New(providerPool)
	must(err, "tenant service")
	provisioning, err := tenant.NewCoordinator(providerPool, tenants, 30*time.Minute)
	must(err, "provisioning coordinator")
	organizations, err := organization.New(providerPool)
	must(err, "organization service")
	workspaces, err := workspace.New(tenantPool)
	must(err, "workspace service")
	selfPool, err := db.NewSelfPool(tenantConns)
	must(err, "self pool")
	invitations, err := invitation.New(tenantPool, providerPool, selfPool, memberships, matchingAddresses{})
	must(err, "invitation service")
	resolverPool, err := db.NewInvitationResolverPool(tenantConns, recorder)
	must(err, "invitation resolver pool")
	invitationResolver, err := invitation.NewResolver(resolverPool)
	must(err, "invitation resolver")
	offboardings, err := offboarding.New(providerPool, tenantPool, tenants, memberships)
	must(err, "offboarding service")
	registry, err := projection.NewRegistry(providerPool)
	must(err, "projection registry")
	publisher, err := projection.NewPublisher(providerPool, registry)
	must(err, "projection publisher")
	reconciler, err := projection.NewReconciler(providerPool)
	must(err, "projection reconciler")
	replayer, err := projection.NewReplayer(providerPool)
	must(err, "dead-letter replayer")
	deadLetters, err := projection.NewDeadLetterReader(providerPool)
	must(err, "dead-letter reader")
	resolutionPool, err := db.NewResolutionPool(resolutionConns, recorder)
	must(err, "resolution pool")
	resolver, err := projection.NewResolver(resolutionPool)
	must(err, "dead-letter resolver")
	frontier, err := projection.NewFrontierReader(providerConns)
	must(err, "frontier reader")
	contexts, err := occontext.New(providerPool)
	must(err, "context service")
	contextList, err := occontext.NewContexts(providerPool, selfPool)
	must(err, "context list")
	providerGrants, err := authority.NewAdministration(providerPool)
	must(err, "provider grants")
	tenantAdministrators, err := authority.NewTenantAdministration(providerPool, tenantPool, memberships)
	must(err, "tenant administration")
	providerActivations, err := authority.NewActivations(providerPool, authority.ActivationPolicy{Max: 8 * time.Hour, ApprovalRequired: true})
	must(err, "provider activations")
	accessReview, err := authority.NewAccessReview(providerPool, tenantPool)
	must(err, "privileged-access review")
	reporters, err := authority.NewReporters(providerPool)
	must(err, "reporters")
	consumerPool, err := db.NewConsumerPool(consumerConns, recorder)
	must(err, "consumer pool")
	consumerAccess, err := projection.NewConsumerAccess(consumerPool)
	must(err, "consumer access")
	checks, err := occontext.NewConsumerChecks(consumerPool)
	must(err, "consumer checks")
	consumerFrontier, err := projection.NewFrontierReader(consumerConns)
	must(err, "consumer frontier")

	surface, err := Routes(RoutesConfig{
		Services: Services{
			Memberships: memberships, Tenants: tenants, Provisioning: provisioning,
			Organizations: organizations, Workspaces: workspaces, Invitations: invitations,
			Offboardings: offboardings, InvitationResolver: invitationResolver,
			Registry: registry, Publisher: publisher, Reconciler: reconciler, Contexts: contexts,
			ContextList: contextList, Replayer: replayer, Resolver: resolver, DeadLetters: deadLetters,
			Frontier: frontier, ProviderGrants: providerGrants, ProviderActivations: providerActivations,
			TenantAdministrators: tenantAdministrators, AccessReview: accessReview, Reporters: reporters,
			Consumer: &ConsumerServices{Access: consumerAccess, Checks: checks, Frontier: consumerFrontier},
		},
		Database: okProber{},
	})
	must(err, "routes")
	return &live{t: t, owner: owner, surface: surface}
}

// handler mounts the surface with the caller, as mounted does for the route tests.
func (l *live) handler(caller Caller) http.Handler {
	establish := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := observability.WithCorrelationID(r.Context(), mustID(l.t))
			next.ServeHTTP(w, r.WithContext(WithCaller(ctx, caller)))
		})
	}
	return l.surface.Mount(func(next http.Handler) http.Handler { return next }, establish,
		func(next http.Handler) http.Handler { return establish(ResolveScope(next)) })
}

// call sends one exchange as the caller and requires the status. Every request carries an
// Idempotency-Key and a reason; body is marshalled unless it is a string or nil. The decoded JSON
// body is returned.
func (l *live) call(caller Caller, method, path string, body any, want int) map[string]any {
	l.t.Helper()
	var text string
	switch value := body.(type) {
	case nil:
	case string:
		text = value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			l.t.Fatalf("encode the body of %s %s: %v", method, path, err)
		}
		text = string(encoded)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(text))
	if text != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set(IdempotencyHeader, mustID(l.t).String())
	request.Header.Set(ReasonHeader, liveReason)
	recorded := serveChecked(l.t, l.handler(caller), request)
	if recorded.Code != want {
		l.t.Fatalf("%s %s answered %d, want %d: %s", method, path, recorded.Code, want, recorded.Body)
	}
	decoded := map[string]any{}
	if recorded.Body.Len() > 0 {
		_ = json.Unmarshal(recorded.Body.Bytes(), &decoded)
	}
	return decoded
}

// exec runs a seeding statement as the owner, which the fixture is not under test for.
func (l *live) exec(statement string, args ...any) {
	l.t.Helper()
	if err := l.owner.InTx(stdcontext.Background(), func(ctx stdcontext.Context, tx fdb.Tx) error {
		_, err := tx.Exec(ctx, statement, args...)
		return err
	}); err != nil {
		l.t.Fatalf("seed: %v\n%s", err, statement)
	}
}

// fixtureProvider is the principal scripts/ci-fixture.sql gives the bootstrap emergency grant.
const fixtureProvider = "55555555-5555-4555-8555-55555555555a"

// emergencyProvider is a provider caller holding the fixture's emergency grant, as authentication
// would have found it.
func (l *live) emergencyProvider() Caller {
	return Caller{Subject: id.MustParse(fixtureProvider), Provider: true, Emergency: true}
}

// str reads a string field of a decoded body.
func str(t *testing.T, body map[string]any, field string) string {
	t.Helper()
	value, ok := body[field].(string)
	if !ok || value == "" {
		t.Fatalf("the body has no %s: %v", field, body)
	}
	return value
}

// obj reads an object field of a decoded body.
func obj(t *testing.T, body map[string]any, field string) map[string]any {
	t.Helper()
	value, ok := body[field].(map[string]any)
	if !ok {
		t.Fatalf("the body has no object %s: %v", field, body)
	}
	return value
}

// seedTenant records an active Organization and Tenant, and returns the Tenant's identifier.
func (l *live) seedTenant() string {
	l.t.Helper()
	organizationID, tenantID := mustID(l.t).String(), mustID(l.t).String()
	l.exec(`INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, $2, 'customer', 'active')`, organizationID, "Seeded "+organizationID)
	l.exec(`INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, 'Seeded', 'active', 'pooled')`, tenantID, organizationID)
	return tenantID
}
