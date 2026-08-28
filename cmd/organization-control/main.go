// Command organization-control is the Organization Control Service deployable.
//
// This file is the composition root and the only place in the repository that constructs anything.
// Every dependency is built here and passed down explicitly: no package-level singleton, no init()
// side effect, and nothing started by the act of being linked, per STD-GLB-BE-001 rule 10.
//
// # Two pools, two credentials
//
// The service opens two connection pools authenticating as two PostgreSQL login roles:
// `organization_rt` for tenant-scoped traffic and `organization_provider_rt` for deliberately
// cross-Tenant traffic. That split is the isolation boundary, and it is a runtime fact rather than a
// Go one — `db.TenantPool` and `db.ProviderPool` make a mix-up a compile error, but only if the two
// are built from two different credentials. `config.Load` refuses identical DSNs for that reason.
//
// # The service reaches no other domain
//
// ADR-ORG-001 §5.4 gives this process neither the Keycloak administration credential nor a network
// route to the kernel. It serves HTTP and calls nothing over it: `arch.json` excepts this package
// from the `net/http` denial for serving, and `internal/httpapi/outbound_test.go` walks this file for
// any client construction.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	fhttp "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/observability"
	"github.com/anshacerbia2/foundation-platform/verify"

	"github.com/anshacerbia2/organization-control/internal/access"
	"github.com/anshacerbia2/organization-control/internal/config"
	occontext "github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/httpapi"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when configuration fails, so this one write goes to stderr
		// directly rather than through a dependency that might be the failure.
		fmt.Fprintf(os.Stderr, "organization-control: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	logger := newLogger(cfg.LogLevel)

	// Signals are wired before anything is acquired. A process that takes a database connection
	// before it can be interrupted is a process that ignores the first SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telemetry, err := observability.New(observability.Config{
		Deployable: cfg.Deployable,
		System:     cfg.System,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	// No SessionBinder is supplied to either pool, and the omission is a statement. The binder
	// exists so foundation-platform can issue the scope binding without naming a tenant; this
	// repository binds its own, in internal/db and nowhere else, because
	// TDD-organization-control-001 requires the binding to appear in exactly one package and
	// binding_test.go asserts that by walking the repository. A binder here would be a second path.
	tenantConns, err := fdb.Open(ctx, fdb.Config{
		Name:            "organization-control-tenant",
		DSN:             cfg.TenantDSN,
		MaxConns:        cfg.DBMaxConns,
		MaxConnLifetime: cfg.DBMaxConnLifetime,
		AcquireTimeout:  cfg.DBAcquireTimeout,
	})
	if err != nil {
		return fmt.Errorf("tenant pool: %w", err)
	}
	defer tenantConns.Close()

	providerConns, err := fdb.Open(ctx, fdb.Config{
		Name:            "organization-control-provider",
		DSN:             cfg.ProviderDSN,
		MaxConns:        cfg.DBMaxConns,
		MaxConnLifetime: cfg.DBMaxConnLifetime,
		AcquireTimeout:  cfg.DBAcquireTimeout,
	})
	if err != nil {
		return fmt.Errorf("provider pool: %w", err)
	}
	defer providerConns.Close()

	logger.Info("control database connected",
		slog.String("tenant_pool", tenantConns.Name()),
		slog.String("provider_pool", providerConns.Name()),
		slog.Int("max_conns_each", int(cfg.DBMaxConns)))

	// The recorder is built on the provider connections rather than the tenant ones because the
	// evidence table is revoked from the tenant role. It writes in its own transaction, so an
	// access whose work later rolls back is still recorded — see internal/access.
	recorder, err := access.New(providerConns)
	if err != nil {
		return fmt.Errorf("privileged-access recorder: %w", err)
	}

	tenantPool, err := db.NewTenantPool(tenantConns)
	if err != nil {
		return fmt.Errorf("tenant scope pool: %w", err)
	}
	providerPool, err := db.NewProviderPool(providerConns, recorder)
	if err != nil {
		return fmt.Errorf("provider scope pool: %w", err)
	}

	memberships, err := membership.New(tenantPool)
	if err != nil {
		return fmt.Errorf("membership service: %w", err)
	}
	tenants, err := tenant.New(providerPool)
	if err != nil {
		return fmt.Errorf("tenant service: %w", err)
	}
	organizations, err := organization.New(providerPool)
	if err != nil {
		return fmt.Errorf("organization service: %w", err)
	}
	workspaces, err := workspace.New(tenantPool)
	if err != nil {
		return fmt.Errorf("workspace service: %w", err)
	}
	invitations, err := invitation.New(tenantPool, providerPool, memberships)
	if err != nil {
		return fmt.Errorf("invitation service: %w", err)
	}
	offboardings, err := offboarding.New(providerPool, tenantPool, tenants, memberships)
	if err != nil {
		return fmt.Errorf("offboarding service: %w", err)
	}
	registry, err := projection.NewRegistry(providerPool)
	if err != nil {
		return fmt.Errorf("projection registry: %w", err)
	}
	publisher, err := projection.NewPublisher(providerPool, registry)
	if err != nil {
		return fmt.Errorf("projection publisher: %w", err)
	}
	reconciler, err := projection.NewReconciler(providerPool)
	if err != nil {
		return fmt.Errorf("projection reconciler: %w", err)
	}
	contexts, err := occontext.New(providerPool)
	if err != nil {
		return fmt.Errorf("context service: %w", err)
	}

	// Readiness probes the tenant pool. One of the two is enough to answer whether this replica can
	// serve, and it is the tenant one because that is the pool ordinary traffic uses: a replica
	// whose tenant pool is unreachable can serve almost nothing, while one whose provider pool is
	// unreachable can still serve every tenant-scoped route.
	surface, err := httpapi.Routes(httpapi.RoutesConfig{
		Services: httpapi.Services{
			Memberships: memberships, Tenants: tenants, Organizations: organizations,
			Workspaces: workspaces, Invitations: invitations, Offboardings: offboardings,
			Registry: registry, Publisher: publisher, Reconciler: reconciler, Contexts: contexts,
		},
		Database:         tenantConns,
		Telemetry:        telemetry,
		ReadinessTimeout: cfg.ReadinessTimeout,
	})
	if err != nil {
		return fmt.Errorf("routes: %w", err)
	}

	// The key source performs no fetch here. A cold replica loads the key set on its first
	// verification, and NewJWKS deliberately touches no network so the composition root decides
	// when that happens rather than the linker.
	keys, err := verify.NewJWKS(verify.JWKSConfig{URL: cfg.JWKSURL})
	if err != nil {
		return fmt.Errorf("jwks source: %w", err)
	}

	authenticationConfig := httpapi.AuthenticationConfig{
		TenantClaim:  cfg.TenantClaim,
		ProviderRole: cfg.ProviderRole,
	}

	// The claim rule is this service's, because STD-IAM-002 §3.5 states it in terms of claims
	// foundation-platform is forbidden from naming. The verifier refuses to build without one.
	verifier, err := verify.New(verify.Config{
		Issuer:      cfg.TokenIssuer,
		Audience:    cfg.TokenAudience,
		Keys:        keys,
		Requirement: httpapi.Requirement(authenticationConfig),
		MaxSkew:     cfg.TokenMaxSkew,
	})
	if err != nil {
		return fmt.Errorf("token verifier: %w", err)
	}

	authentication, err := httpapi.Authenticate(verifier, authenticationConfig)
	if err != nil {
		return fmt.Errorf("authentication middleware: %w", err)
	}

	logger.Info("token verification configured",
		slog.String("issuer", cfg.TokenIssuer),
		slog.String("audience", cfg.TokenAudience),
		slog.String("jwks_url", cfg.JWKSURL),
		slog.String("tenant_claim", cfg.TenantClaim),
		slog.String("provider_role", cfg.ProviderRole),
		slog.Duration("max_skew", cfg.TokenMaxSkew))

	// Scope resolution runs inside the chain's innermost position rather than as another Chain
	// option, because Chain's slots are fixed by TDD-foundation-platform-002 and scope resolution
	// is this service's own step. It must come after authentication — it reads the caller
	// authentication established — and before any handler, which is exactly where wrapping the mux
	// puts it.
	apiChain := func(next http.Handler) http.Handler {
		return fhttp.Chain(fhttp.Options{
			Telemetry:      telemetry,
			Timeout:        cfg.HTTPRequestTimeout,
			MaxInFlight:    cfg.HTTPMaxInFlight,
			Authentication: authentication,
		})(httpapi.ResolveScope(next))
	}

	// The anonymous chain carries observability, timeout, and shedding, and neither authentication
	// nor scope resolution. It holds one route — the invitation lookup, which SAD-004 §5.5 requires
	// to answer identically for every token and which therefore reads nothing. Load shedding is
	// kept precisely because it is the one unauthenticated route: it is the only path an
	// unauthenticated caller can put load on.
	anonymousChain := fhttp.Chain(fhttp.Options{
		Telemetry:   telemetry,
		Timeout:     cfg.HTTPRequestTimeout,
		MaxInFlight: cfg.HTTPMaxInFlight,
	})

	// Probes get the same observability and timeout and neither authentication nor the API's
	// in-flight budget. Both omissions are decisions: a probe cannot present a credential, and a
	// readiness check shed by an overloaded API would remove a replica that is still healthy —
	// which is how load shedding turns overload into an outage.
	probeChain := fhttp.Chain(fhttp.Options{
		Telemetry: telemetry,
		Timeout:   cfg.HTTPRequestTimeout,
	})

	server, err := fhttp.NewServer(cfg.ListenAddress,
		surface.Mount(probeChain, anonymousChain, apiChain), fhttp.ServerConfig{
			ReadTimeout:  cfg.HTTPReadTimeout,
			WriteTimeout: cfg.HTTPWriteTimeout,
		})
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("address", cfg.ListenAddress))
		if listenErr := server.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			serveErr <- listenErr
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signalled", slog.Duration("grace", cfg.HTTPShutdownGrace))
	}

	// Shutdown uses a fresh context. Reusing the cancelled one would abort the drain at the instant
	// it began, which is indistinguishable from having no grace period.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTPShutdownGrace)
	defer cancel()
	if err := fhttp.Shutdown(shutdownCtx, server, cfg.HTTPShutdownGrace); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("stopped")
	return nil
}

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	// JSON to stdout with no vendor agent, per STD-GLB-003. Credential redaction is enforced inside
	// foundation-platform's serializer rather than here, so a caller cannot forget it.
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parsed}))
}
