package httpapi

import (
	stdcontext "context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/observability"

	occontext "github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

// Prober reports whether a dependency can be reached.
//
// An interface rather than a pool so readiness can be tested without a database, and so this
// package cannot reach past the one method it needs.
type Prober interface {
	Ping(ctx stdcontext.Context) error
}

// Services is every authority this surface routes to.
//
// All are required. An optional service would produce a deployment where some routes answer 404
// because a field was nil, and nothing would report which — a surface that looks complete and is
// not. `Routes` refuses to build instead.
type Services struct {
	Memberships *membership.Service
	Tenants     *tenant.Service

	// Provisioning owns the three transitions between `requested` and `provisioning`, which
	// `Tenants` deliberately does not expose. Separate here because it is a separate component in
	// TDD-organization-control-003 §"Component Design", and because the routes it serves are driven
	// by the provisioning system rather than by an operator.
	Provisioning *tenant.Coordinator

	Organizations *organization.Service
	Workspaces    *workspace.Service
	Invitations   *invitation.Service
	Offboardings  *offboarding.Service
	Registry      *projection.Registry
	Publisher     *projection.Publisher
	Reconciler    *projection.Reconciler
	Contexts      *occontext.Service
}

// RoutesConfig supplies what the surface needs.
type RoutesConfig struct {
	Services  Services
	Database  Prober
	Telemetry *observability.Telemetry

	// ReadinessTimeout bounds the dependency check. It sits well below any orchestrator probe
	// interval so a slow database produces a failed probe rather than a hung one.
	ReadinessTimeout time.Duration
}

// Surface is the deployable's HTTP surface, split by what a request is allowed to carry.
//
// Three muxes rather than one with an exemption list. An exemption list is edited by whoever adds a
// route, and the failure mode of forgetting is an unauthenticated mutation; here a route is
// unauthenticated only if its author writes it into `Anonymous`, and it escapes scope resolution
// only if written into `Probes` or `Anonymous`. identity-control learned the first half of this the
// hard way: one mux meant the authentication middleware also wrapped `/readyz`, every probe
// answered 401, and no replica entered service.
type Surface struct {
	// Probes is liveness and readiness. It carries no authentication and never will.
	Probes http.Handler

	// Anonymous is the routes an unauthenticated caller may reach.
	//
	// It holds exactly one route: the invitation lookup, which SAD-004 §5.5 requires to answer
	// identically for an absent, expired, revoked, accepted, and valid token. Because the answer
	// derives from nothing in the record, the handler reads nothing — which is what keeps an
	// anonymous request off a Row-Level-Security-protected table it could bind no scope for.
	Anonymous http.Handler

	// API is every route that acts on behalf of a caller. It requires an authenticated caller and
	// runs behind ResolveScope.
	API http.Handler
}

// Routes builds the HTTP surface.
//
// It returns bare muxes. The middleware chain is applied by the composition root, so ordering stays
// in one place: TDD-foundation-platform-002 fixes recovery, correlation, logging, timeout, and
// shedding in that order, and a package that wrapped its own routes could quietly reorder them.
func Routes(cfg RoutesConfig) (Surface, error) {
	if err := cfg.Services.validate(); err != nil {
		return Surface{}, err
	}
	if cfg.Database == nil {
		return Surface{}, errors.New("httpapi: a database prober is required")
	}
	if cfg.ReadinessTimeout <= 0 {
		cfg.ReadinessTimeout = 2 * time.Second
	}

	h := &handlers{services: cfg.Services}

	probes := http.NewServeMux()

	// Liveness touches no dependency on purpose. A probe that fails during a database outage makes
	// the orchestrator restart every replica for a fault a restart cannot fix, which converts a
	// degradation into an outage.
	probes.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	probes.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := stdcontext.WithTimeout(r.Context(), cfg.ReadinessTimeout)
		defer cancel()
		if err := cfg.Database.Ping(ctx); err != nil {
			if cfg.Telemetry != nil {
				cfg.Telemetry.Logger(r.Context()).WarnContext(r.Context(), "readiness failed",
					slog.String("error", err.Error()))
			}
			platform.Problem(w, r, platform.DependencyUnavailable, "The control database is unreachable")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	anonymous := http.NewServeMux()
	anonymous.HandleFunc("POST /v1/invitations/lookup", h.lookupInvitation)

	api := http.NewServeMux()

	// Tenant-scoped. None of these paths names a Tenant, so there is no client-supplied Tenant for
	// a handler to mistake for the authoritative one.
	api.HandleFunc("POST /v1/memberships", h.grantMembership)
	api.HandleFunc("POST /v1/memberships/{membership_id}/suspend", h.suspendMembership)
	api.HandleFunc("POST /v1/memberships/{membership_id}/restore", h.restoreMembership)
	api.HandleFunc("POST /v1/memberships/{membership_id}/revoke", h.revokeMembership)

	api.HandleFunc("POST /v1/workspaces", h.createWorkspace)
	api.HandleFunc("GET /v1/workspaces/{workspace_id}", h.getWorkspace)
	api.HandleFunc("POST /v1/workspaces/{workspace_id}/archive", h.archiveWorkspace)
	api.HandleFunc("POST /v1/workspaces/{workspace_id}/restore", h.restoreWorkspace)
	api.HandleFunc("POST /v1/workspaces/{workspace_id}/retire", h.retireWorkspace)

	api.HandleFunc("POST /v1/invitations", h.issueInvitation)
	api.HandleFunc("GET /v1/invitations/{invitation_id}", h.getInvitation)
	api.HandleFunc("POST /v1/invitations/{invitation_id}/revoke", h.revokeInvitation)
	api.HandleFunc("POST /v1/invitations/accept", h.acceptInvitation)

	// Provider-scoped.
	api.HandleFunc("POST /v1/invitations/verify-identity", h.recordVerifiedIdentity)
	api.HandleFunc("POST /v1/invitations/expire-lapsed", h.expireLapsedInvitations)

	api.HandleFunc("POST /v1/organizations", h.registerOrganization)
	api.HandleFunc("GET /v1/organizations/{organization_id}", h.getOrganization)
	api.HandleFunc("POST /v1/organizations/{organization_id}/suspend", h.suspendOrganization)
	api.HandleFunc("POST /v1/organizations/{organization_id}/restore", h.restoreOrganization)
	api.HandleFunc("POST /v1/organizations/{organization_id}/retire", h.retireOrganization)

	api.HandleFunc("POST /v1/tenants", h.requestTenant)
	api.HandleFunc("GET /v1/tenants/{tenant_id}", h.getTenant)
	api.HandleFunc("POST /v1/tenants/{tenant_id}/activate", h.activateTenant)
	api.HandleFunc("POST /v1/tenants/{tenant_id}/suspend", h.suspendTenant)
	api.HandleFunc("POST /v1/tenants/{tenant_id}/restore", h.restoreTenant)

	// The provisioning correlation surface.
	//
	// Two of these are driven by the external system that owns the isolation boundary rather than by
	// a person, and they are on the authenticated provider surface all the same. A callback route
	// exempted from authentication would let anyone who learned a correlation identifier declare a
	// Tenant's boundary built, and activation reads exactly that statement.
	//
	// The design's API list names none of these, which is an omission rather than a prohibition: it
	// mandates realized-status correlation, gives this service no inbound transport but HTTP, and
	// `POST /v1/offboardings/{id}/deprovisioning` already reports the other direction's outcome the
	// same way. These mirror it.
	api.HandleFunc("POST /v1/tenants/{tenant_id}/provisioning", h.provisionTenant)
	api.HandleFunc("POST /v1/provisioning/realized", h.realizeProvisioning)
	api.HandleFunc("POST /v1/provisioning/failed", h.failProvisioning)
	api.HandleFunc("POST /v1/provisioning/sweep-unresolved", h.sweepProvisioning)

	api.HandleFunc("POST /v1/offboardings", h.beginOffboarding)
	api.HandleFunc("GET /v1/offboardings/{offboarding_id}", h.getOffboarding)
	api.HandleFunc("POST /v1/offboardings/{offboarding_id}/freeze", h.freezeOffboarding)
	api.HandleFunc("POST /v1/offboardings/{offboarding_id}/complete-freeze", h.completeFreeze)
	api.HandleFunc("POST /v1/offboardings/{offboarding_id}/release", h.releaseOffboarding)
	api.HandleFunc("POST /v1/offboardings/{offboarding_id}/retire", h.retireOffboarding)
	api.HandleFunc("POST /v1/offboardings/{offboarding_id}/legal-hold", h.setLegalHold)
	api.HandleFunc("POST /v1/offboardings/{offboarding_id}/obligations", h.raiseObligation)
	api.HandleFunc("GET /v1/offboardings/{offboarding_id}/obligations", h.outstandingObligations)
	api.HandleFunc("POST /v1/offboardings/{offboarding_id}/deprovisioning", h.recordDeprovisioning)
	api.HandleFunc("POST /v1/obligations/{obligation_id}/resolve", h.resolveObligation)

	api.HandleFunc("POST /v1/projections/consumers", h.registerConsumer)
	api.HandleFunc("GET /v1/projections/consumers/{consumer_id}", h.getConsumer)
	api.HandleFunc("POST /v1/projections/consumers/{consumer_id}/progress", h.recordProgress)
	api.HandleFunc("POST /v1/projections/consumers/{consumer_id}/bootstrap", h.bootstrapConsumer)
	api.HandleFunc("POST /v1/projections/snapshot", h.snapshot)
	api.HandleFunc("POST /v1/projections/reconcile", h.reconcile)

	api.HandleFunc("POST /v1/context/verify", h.verifyContext)
	api.HandleFunc("POST /v1/context/switch-eligible", h.switchEligible)
	api.HandleFunc("POST /v1/context/rate", h.recordRate)
	api.HandleFunc("GET /v1/context/rate/over-threshold", h.ratesOverThreshold)

	return Surface{Probes: probes, Anonymous: anonymous, API: api}, nil
}

func (s Services) validate() error {
	switch {
	case s.Memberships == nil:
		return errors.New("httpapi: the membership service is required")
	case s.Tenants == nil:
		return errors.New("httpapi: the tenant service is required")
	case s.Provisioning == nil:
		return errors.New("httpapi: the provisioning coordinator is required")
	case s.Organizations == nil:
		return errors.New("httpapi: the organization service is required")
	case s.Workspaces == nil:
		return errors.New("httpapi: the workspace service is required")
	case s.Invitations == nil:
		return errors.New("httpapi: the invitation service is required")
	case s.Offboardings == nil:
		return errors.New("httpapi: the offboarding service is required")
	case s.Registry == nil:
		return errors.New("httpapi: the projection registry is required")
	case s.Publisher == nil:
		return errors.New("httpapi: the projection publisher is required")
	case s.Reconciler == nil:
		return errors.New("httpapi: the projection reconciler is required")
	case s.Contexts == nil:
		return errors.New("httpapi: the context service is required")
	}
	return nil
}

// handlers holds the services the routes call. Unexported: the routes are the surface's contract,
// not the methods.
type handlers struct{ services Services }

// Mount joins the three halves onto one root mux, each behind the chain its half requires.
//
// The probe and anonymous patterns are literal and method-qualified, so Go's mux precedence gives
// them priority over the catch-all without either half being able to shadow the other.
func (s Surface) Mount(probeChain, anonymousChain, apiChain func(http.Handler) http.Handler) http.Handler {
	root := http.NewServeMux()
	root.Handle("GET /healthz", probeChain(s.Probes))
	root.Handle("GET /readyz", probeChain(s.Probes))
	root.Handle("POST /v1/invitations/lookup", anonymousChain(s.Anonymous))
	root.Handle("/", apiChain(s.API))
	return root
}
