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
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/anshacerbia2/foundation-platform/clientauth"
	fdb "github.com/anshacerbia2/foundation-platform/db"
	fhttp "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/observability"
	"github.com/anshacerbia2/foundation-platform/verify"

	"github.com/anshacerbia2/organization-control/internal/access"
	"github.com/anshacerbia2/organization-control/internal/authority"
	"github.com/anshacerbia2/organization-control/internal/config"
	occontext "github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/delivery"
	"github.com/anshacerbia2/organization-control/internal/httpapi"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/posture"
	"github.com/anshacerbia2/organization-control/internal/projection"
	enforcement "github.com/anshacerbia2/organization-control/internal/telemetry"
	"github.com/anshacerbia2/organization-control/internal/tenant"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

func main() {
	// One subcommand: the bootstrap that makes the first provider grant (ADR-ORG-001 §5.11). A
	// command on the deployable rather than an endpoint, for the reason the first Principal's
	// ceremony is one: an endpoint needs a caller, and the first grant is what makes the first one.
	if len(os.Args) > 1 && os.Args[1] == bootstrapCommand {
		if err := bootstrapProvider(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "organization-control %s: %v\n", bootstrapCommand, err)
			os.Exit(1)
		}
		return
	}
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
	// A batch execution's heartbeat older than membership.BatchLease is read as its request having
	// ended, and the execution is resumed by the next execute (TDD-organization-control-002 §Resuming
	// an execution). That reading is true only while no request outlives the lease.
	if cfg.HTTPRequestTimeout >= membership.BatchLease {
		return fmt.Errorf("configuration: HTTP_REQUEST_TIMEOUT (%s) must be shorter than the batch "+
			"execution lease (%s), or a live execution could be read as abandoned",
			cfg.HTTPRequestTimeout, membership.BatchLease)
	}

	logger := newLogger(cfg.LogLevel)

	// Signals are wired before anything is acquired. A process that takes a database connection
	// before it can be interrupted is a process that ignores the first SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The Collector, when one is configured. Without it every metric and span goes to the no-op
	// providers, which is said once here rather than discovered as an empty dashboard.
	var exported *observability.Exported
	if cfg.OTLPEndpoint != "" {
		exported, err = observability.Export(ctx, observability.ExportConfig{
			Endpoint: cfg.OTLPEndpoint, Deployable: cfg.Deployable, System: cfg.System,
		})
		if err != nil {
			return fmt.Errorf("telemetry export: %w", err)
		}
		defer func() {
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := exported.Shutdown(flush); err != nil {
				logger.Warn("telemetry flush on shutdown", slog.String("error", err.Error()))
			}
		}()
	} else {
		logger.Warn("OTEL_EXPORTER_OTLP_ENDPOINT is unset: no metric or trace leaves this process")
	}
	telemetryConfig := observability.Config{Deployable: cfg.Deployable, System: cfg.System, Logger: logger}
	if exported != nil {
		telemetryConfig.MeterProvider, telemetryConfig.TracerProvider = exported.MeterProvider, exported.TracerProvider
	}
	telemetry, err := observability.New(telemetryConfig)
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

	// The isolation posture, read from the catalog as the tenant login role before anything is
	// served (TDD-organization-control-001 §Verifying the Posture at Runtime). The migration job
	// asserts it after each deploy; this covers the time between deploys, when a superuser action
	// during an incident could drop FORCE from one table and the next signal would be a cross-Tenant
	// read. A problem stops the start: EAD-006 §8 has a security-control failure fail closed.
	report, err := posture.Startup(ctx, tenantConns, startupPostureTimeout)
	if err != nil {
		for _, problem := range report.Problems {
			logger.Error("isolation posture", slog.String("problem", problem))
		}
		return err
	}
	logger.Info("tenant isolation verified",
		slog.Int("protected_tables", len(report.Tables)),
		slog.Any("schemas", posture.RLSSchemas))
	// The same check backs GET /readyz, so a replica whose database loses a policy after start
	// leaves the load balancer rather than serving.
	readiness, err := posture.NewProbe(tenantConns)
	if err != nil {
		return fmt.Errorf("readiness probe: %w", err)
	}

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

	// The claim store writes `platform.idempotency_key` and nothing else, so it takes the raw
	// transactor rather than a scoped pool: that table carries no tenant_id and no Row-Level
	// Security policy — its `scope` column is its isolation — so there is no binding for the write
	// to need, and routing it through the provider scope would file a privileged-access record for a
	// bookkeeping update.
	//
	// The tenant connections, because that is the pool ordinary traffic already uses and both
	// runtime roles hold the same privileges on the table.
	claims, err := db.NewClaimStore(tenantConns)
	if err != nil {
		return fmt.Errorf("idempotency claim store: %w", err)
	}

	memberships, err := membership.New(tenantPool)
	if err != nil {
		return fmt.Errorf("membership service: %w", err)
	}
	tenants, err := tenant.New(providerPool, tenant.WithDisplayNameMax(cfg.TenantNameMax))
	if err != nil {
		return fmt.Errorf("tenant service: %w", err)
	}
	provisioning, err := tenant.NewCoordinator(providerPool, tenants, cfg.ProvisioningTimeout)
	if err != nil {
		return fmt.Errorf("provisioning coordinator: %w", err)
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
	// Provider-scoped, unlike the frontier below: a replay puts a security event back on the wire,
	// and the access record the scope wrapper writes first is the point of doing it this way.
	replayer, err := projection.NewReplayer(providerPool)
	if err != nil {
		return fmt.Errorf("dead-letter replayer: %w", err)
	}

	// Its own connections, as its own role. Closing an incident is the act that makes every
	// consumer serve again, and it is the only one in this service with a credential of its own.
	resolutionConns, err := fdb.Open(ctx, fdb.Config{Name: "organization-control-resolution", DSN: cfg.ResolutionDSN, MaxConns: 2})
	if err != nil {
		return fmt.Errorf("resolution pool: %w", err)
	}
	defer resolutionConns.Close()
	resolutionPool, err := db.NewResolutionPool(resolutionConns, recorder)
	if err != nil {
		return fmt.Errorf("resolution scope pool: %w", err)
	}
	resolver, err := projection.NewResolver(resolutionPool)
	if err != nil {
		return fmt.Errorf("dead-letter resolver: %w", err)
	}
	// The raw transactor, not the provider pool: the frontier reads platform.outbox aggregates,
	// which carry no tenant_id and no policy, and consumers poll it. Through the provider scope every
	// poll would write a privileged-access record, filling the evidence table with rows about
	// nothing -- the same reasoning as the claim store above.
	frontier, err := projection.NewFrontierReader(providerConns)
	if err != nil {
		return fmt.Errorf("frontier reader: %w", err)
	}

	// The enforcement gauges, read on the raw provider connections on each collection, for the
	// reason the frontier reads them there. Registered only when something will collect them.
	if exported != nil {
		signals, err := projection.NewSignalsReader(providerConns)
		if err != nil {
			return fmt.Errorf("signals reader: %w", err)
		}
		if err := enforcement.Register(exported.MeterProvider, signals, cfg.Deployable, cfg.System); err != nil {
			return fmt.Errorf("enforcement metrics: %w", err)
		}
	}

	contexts, err := occontext.New(providerPool)
	if err != nil {
		return fmt.Errorf("context service: %w", err)
	}
	// A person's own contexts, on the tenant connections as organization_self_rt, which those
	// connections SET ROLE to for the one read and do not inherit (ADR-ORG-005, TDD-001 §Roles).
	selfPool, err := db.NewSelfPool(tenantConns)
	if err != nil {
		return fmt.Errorf("self pool: %w", err)
	}
	contextList, err := occontext.NewContexts(providerPool, selfPool)
	if err != nil {
		return fmt.Errorf("context list: %w", err)
	}
	// Granting and revoking provider authority, in the provider scope like every provider route.
	providerGrants, err := authority.NewAdministration(providerPool)
	if err != nil {
		return fmt.Errorf("provider grant administration: %w", err)
	}
	// A provider acting inside one Tenant: the access recorded on the provider pool, the work on the
	// tenant pool under that Tenant's policy (TDD-organization-control-001 §The Single Binding Path).
	tenantAdministrators, err := authority.NewTenantAdministration(providerPool, tenantPool, memberships)
	if err != nil {
		return fmt.Errorf("tenant administration: %w", err)
	}
	providerActivations, err := authority.NewActivations(providerPool, authority.ActivationPolicy{
		Max: cfg.ProviderActivationMax, ApprovalRequired: cfg.ProviderActivationApproval})
	if err != nil {
		return fmt.Errorf("provider activations: %w", err)
	}

	// A registered consumer's own routes, on its own connections and its own role. Built only when
	// the consumer credential is configured, which is what enables consumer authority: without the
	// pool no caller could be served. The frontier
	// takes the raw consumer connections for the reason the provider's frontier takes the raw
	// provider ones.
	var consumerServices *httpapi.ConsumerServices
	if cfg.ConsumerDSN != "" {
		consumerConns, err := fdb.Open(ctx, fdb.Config{
			Name:            "organization-control-consumer",
			DSN:             cfg.ConsumerDSN,
			MaxConns:        cfg.DBMaxConns,
			MaxConnLifetime: cfg.DBMaxConnLifetime,
			AcquireTimeout:  cfg.DBAcquireTimeout,
		})
		if err != nil {
			return fmt.Errorf("consumer pool: %w", err)
		}
		defer consumerConns.Close()
		consumerPool, err := db.NewConsumerPool(consumerConns, recorder)
		if err != nil {
			return fmt.Errorf("consumer scope pool: %w", err)
		}
		access, err := projection.NewConsumerAccess(consumerPool)
		if err != nil {
			return fmt.Errorf("consumer access: %w", err)
		}
		checks, err := occontext.NewConsumerChecks(consumerPool)
		if err != nil {
			return fmt.Errorf("consumer checks: %w", err)
		}
		consumerFrontier, err := projection.NewFrontierReader(consumerConns)
		if err != nil {
			return fmt.Errorf("consumer frontier reader: %w", err)
		}
		consumerServices = &httpapi.ConsumerServices{Access: access, Checks: checks, Frontier: consumerFrontier}
	}

	// Readiness probes the tenant pool and the isolation posture. One pool is enough to answer
	// whether this replica can serve, and it is the tenant one because that is the pool ordinary
	// traffic uses: a replica whose tenant pool is unreachable can serve almost nothing, while one
	// whose provider pool is unreachable can still serve every tenant-scoped route.
	surface, err := httpapi.Routes(httpapi.RoutesConfig{
		Services: httpapi.Services{
			Memberships: memberships, Tenants: tenants, Provisioning: provisioning,
			Organizations: organizations,
			Workspaces:    workspaces, Invitations: invitations, Offboardings: offboardings,
			Registry: registry, Publisher: publisher, Reconciler: reconciler, Contexts: contexts, ContextList: contextList,
			Replayer: replayer, Resolver: resolver,
			ProviderGrants:       providerGrants,
			ProviderActivations:  providerActivations,
			TenantAdministrators: tenantAdministrators,
			Frontier:             frontier,
			Consumer:             consumerServices,
		},
		Database:         readiness,
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

	// Provider grants and consumer registrations, read for each request on the provider connections
	// (TDD-organization-control-001 §Caller Authority). The raw transactor, for the reason the
	// frontier takes one: the tables carry no policy, and the scope wrapper would file an access
	// record before the request's own scope is known.
	records, err := authority.NewReader(providerConns)
	if err != nil {
		return fmt.Errorf("caller records: %w", err)
	}
	// Fewer than two emergency grants in production is a lockout waiting for one absence: they are
	// how a deployment that requires approval stays administrable (ADR-ORG-002 §5.2).
	// Each scope is its own lockout: the Identity Control API's emergency grants are what keep it
	// administrable through an Organization outage (ADR-ORG-002 §5.3).
	if cfg.Production {
		for _, scope := range authority.Scopes {
			if count, err := records.EmergencyGrants(ctx, scope); err != nil {
				logger.Error("emergency grants could not be counted",
					slog.String("scope", scope), slog.String("error", err.Error()))
			} else if count < 2 {
				logger.Warn("fewer than two emergency provider grants in production; grant another with kind emergency",
					slog.String("scope", scope), slog.Int("emergency_grants", count))
			}
		}
	}
	// A Tenant administrator's records, read on the tenant connections under that Tenant's own policy
	// (ADR-ORG-003 §5.3), beside the provider and consumer records above.
	tenantRecords, err := authority.NewTenantRecords(tenantPool)
	if err != nil {
		return fmt.Errorf("tenant caller records: %w", err)
	}
	authenticationConfig := httpapi.AuthenticationConfig{
		Records:       callerRecords{Reader: records, TenantRecords: tenantRecords},
		Logger:        logger,
		EmergencyUses: records,
		Consumers:     consumerServices != nil,
	}

	// The claim rule is this service's, because STD-IAM-002 §3.5 states it in terms of claims
	// foundation-platform is forbidden from naming. The verifier refuses to build without one.
	verifier, err := verify.New(verify.Config{
		Issuer:                 cfg.TokenIssuer,
		Audience:               cfg.TokenAudience,
		Keys:                   keys,
		Requirement:            httpapi.Requirement(),
		MaxSkew:                cfg.TokenMaxSkew,
		RequireAccessTokenType: cfg.EnforceAccessTokenType,
	})
	if err != nil {
		return fmt.Errorf("token verifier: %w", err)
	}
	var tokens httpapi.TokenVerifier = verifier
	if !cfg.EnforceAccessTokenType {
		tokens = httpapi.ReportTokenType(verifier, logger)
	}

	authentication, err := httpapi.Authenticate(tokens, authenticationConfig)
	if err != nil {
		return fmt.Errorf("authentication middleware: %w", err)
	}

	logger.Info("token verification configured",
		slog.String("issuer", cfg.TokenIssuer),
		slog.String("audience", cfg.TokenAudience),
		slog.String("jwks_url", cfg.JWKSURL),
		slog.Bool("consumer_authority", authenticationConfig.Consumers),
		slog.Duration("max_skew", cfg.TokenMaxSkew))

	// Scope resolution runs inside the chain's innermost position rather than as another Chain
	// option, because Chain's slots are fixed by TDD-foundation-platform-002 and scope resolution
	// is this service's own step. It must come after authentication — it reads the caller
	// authentication established — and before any handler, which is exactly where wrapping the mux
	// puts it.
	// The idempotency middleware sits inside scope resolution, and the nesting is forced rather
	// than stylistic. It builds a claim scoped per authenticated caller, so it must run after
	// authentication has established one; and the claim it attaches is made by the service's own
	// transaction, so it must run before the handler. Between ResolveScope and the mux is the only
	// position that satisfies both.
	idempotent, err := httpapi.Idempotent(claims, telemetry)
	if err != nil {
		return fmt.Errorf("idempotency middleware: %w", err)
	}

	apiChain := func(next http.Handler) http.Handler {
		return fhttp.Chain(fhttp.Options{
			Telemetry:      telemetry,
			Timeout:        cfg.HTTPRequestTimeout,
			MaxInFlight:    cfg.HTTPMaxInFlight,
			Authentication: authentication,
		})(httpapi.ResolveScope(idempotent(next)))
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

	// This service's consumers' dispatchers (ADR-GLB-018 §5.4), each waiting for its consumer's
	// registration. Started before the listener so a delivery backlog drains while the HTTP surface
	// comes up, and stopped by the same signal.
	deliveryDone := make(chan struct{})
	if targets := cfg.Delivery.Targets; len(targets) > 0 {
		dispatchConns, err := fdb.Open(ctx, fdb.Config{
			Name: "organization-control-dispatch", DSN: cfg.Delivery.DispatchDSN,
			MaxConns: int32(2*len(targets) + 2),
		})
		if err != nil {
			return fmt.Errorf("dispatch database: %w", err)
		}
		defer dispatchConns.Close()
		key, err := clientauth.LoadKey(cfg.Delivery.WorkloadKeyFile)
		if err != nil {
			return fmt.Errorf("workload key: %w", err)
		}
		tokens, err := clientauth.NewTokens(clientauth.Config{
			TokenURL: cfg.Delivery.WorkloadTokenURL, Audience: cfg.TokenIssuer,
			ClientID: cfg.Delivery.WorkloadClientID, Key: key,
		})
		if err != nil {
			return fmt.Errorf("workload token source: %w", err)
		}
		deliveryTargets := make([]delivery.Target, len(targets))
		for i, target := range targets {
			deliveryTargets[i] = delivery.Target{Consumer: target.Consumer, Endpoint: target.Endpoint}
		}
		go func() {
			defer close(deliveryDone)
			_ = delivery.Run(ctx, delivery.Config{
				Targets: deliveryTargets, Pool: dispatchConns, Tokens: tokens,
				Timeout: cfg.Delivery.Timeout, RetryInterval: cfg.Delivery.RetryInterval,
				Telemetry: telemetry, Logger: logger,
			})
		}()
	} else {
		close(deliveryDone)
		logger.Info("no delivery targets are configured; this process runs no dispatcher")
	}
	// The dispatchers stop on the signal and release their leases; wait for them, bounded, so a
	// lease is released rather than left to expire.
	defer func() {
		select {
		case <-deliveryDone:
		case <-time.After(cfg.HTTPShutdownGrace):
			logger.Warn("the dispatchers did not stop within the shutdown grace")
		}
	}()

	// Bind here rather than inside the goroutine, so "listening" is logged after the port is
	// actually held. ListenAndServe binds and serves in one call, so the log line preceded the bind
	// and a port already in use produced "listening on 127.0.0.1:8099" followed by the bind failure
	// — a startup log that says the service is up when it never came up. An orchestrator reading
	// logs to decide readiness would believe the first line.
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("bind %s: %w", cfg.ListenAddress, err)
	}
	logger.Info("listening", slog.String("address", listener.Addr().String()))

	serveErr := make(chan error, 1)
	go func() {
		if listenErr := server.Serve(listener); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
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

// startupPostureTimeout bounds the isolation check at startup. Two catalog queries take
// milliseconds; a database that does not answer them in this time stops the start.
const startupPostureTimeout = 10 * time.Second

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

// callerRecords is the three callers' records in one: a provider's and a consumer's on the provider
// connections, a Tenant administrator's on the tenant connections.
type callerRecords struct {
	*authority.Reader
	*authority.TenantRecords
}
