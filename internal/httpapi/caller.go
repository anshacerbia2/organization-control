package httpapi

import (
	stdcontext "context"
	"errors"
	"net/http"
	"strings"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/observability"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// Caller is the authenticated principal, as the authentication middleware resolved it.
//
// Authentication itself is the composition root's, not this package's: the token format, the issuer,
// and the key rotation are deployment facts, and a transport package that decoded tokens could not
// be tested without one. What this package owns is the step after — turning a caller into a scope —
// because that is the step with a security property worth asserting.
type Caller struct {
	// Subject is the acting Principal, by the token's principal_id, never its sub (STD-IAM-002
	// §3.2). It becomes `db.Scope.Actor`, which is what every lifecycle event and every
	// privileged-access record is attributed to.
	Subject id.UUID

	// Tenant is the Tenant this caller administers. Nil for a provider caller.
	Tenant id.UUID

	// Consumer names the registered projection consumer this caller is, and is empty for every
	// other caller.
	//
	// It comes from the consumer registry, by the token's principal_id, and never from a request
	// body. The fresh check is metered per consumer, so a consumer able to name itself in the body
	// could spend another consumer's budget -- and the meter exists to make an over-eager consumer
	// visible, which it cannot do if the counter it increments is chosen by the caller.
	Consumer string

	// Provider is set when the caller holds cross-Tenant provider authority: a provider grant this
	// service records for the Principal, read when the request was authenticated (ADR-ORG-001 §5.11).
	//
	// A boolean rather than a role list because this package makes no authorization decision: it
	// asks which isolation scope applies. Whether this caller holds provider authority at all was
	// decided before the value was set.
	//
	// It is authority in force: an emergency grant, or an approved activation of an eligible grant
	// that has not ended (ADR-ORG-002).
	Provider bool

	// Eligible is a provider grant holder with no authority in force. It resolves to the provider
	// scope, so the access record names it, and reaches the activation routes alone; every other
	// route refuses it before a transaction opens, and requireProvider refuses it on any.
	Eligible bool

	// Emergency is authority from an emergency grant. Every request it authorizes is reported
	// (ADR-ORG-002 §5.2).
	Emergency bool

	// Self is a person reading their own contexts: a human token on
	// GET /v1/principals/{principal_id}/contexts whose principal_id is the path's (ADR-ORG-005 §5.1).
	// Authentication sets it on that route alone, so no other route ever sees a self caller.
	Self bool
}

type callerKey struct{}

// WithCaller records the authenticated caller.
//
// Exported so the composition root's authentication middleware can call it, and so tests can
// establish a caller without a token. Nothing else should: a handler that set its own caller would
// be choosing its own scope.
func WithCaller(ctx stdcontext.Context, caller Caller) stdcontext.Context {
	return stdcontext.WithValue(ctx, callerKey{}, caller)
}

// CallerFrom reads the authenticated caller.
func CallerFrom(ctx stdcontext.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok
}

// ReasonHeader carries the administrative reason a cross-Tenant action is taken for.
const ReasonHeader = "X-Administrative-Reason"

// There is deliberately no `Idempotency-Key` here.
//
// foundation-platform's `idempotency.Claim` takes a `db.Tx` because the claim has to commit with
// the effect it guards: claimed in one transaction and applied in another, a crash between them
// replays the effect with the key already burnt. The services in this repository open their own
// transactions and accept no claim, so a middleware in this package could only claim outside them.
//
// Accepting the header and not honouring it would be worse than not accepting it: it tells a client
// its retries are safe at exactly the moment they are not. So the header is absent, mutations are
// documented in ROADMAP.md as not yet idempotent, and closing that gap means giving the services a
// seam for the claim — the same shape as `TransitionWithin` and `GrantWithin`.

// ResolveScope converts the authenticated caller into a resolved isolation scope.
//
// This is the one place a scope is created for an inbound request, and it reads exactly two things:
// the caller the authentication middleware resolved, and the correlation identifier the platform
// chain assigned. It does not read the path, the query, the body, or any header naming a Tenant.
//
// SAD-004 §8.3 is the reason. A Tenant identifier arriving from a client is a *requested* scope, and
// treating it as the authoritative one is the substitution that Row-Level Security exists as a
// second line of defence against — a second line that is doing all the work if the first line hands
// it the attacker's value. The tenant-scoped routes in this surface carry no Tenant identifier in
// their paths at all, so on those routes there is no client-supplied value to substitute in the
// first place. `TestScopeIgnoresRequestSuppliedTenant` asserts it for the provider routes, which do
// name a target, by sending a caller bound to one Tenant at a path naming another.
//
// A caller that is absent is refused with 401 rather than treated as anonymous. This handler only
// ever runs behind the API chain, so an absent caller means authentication did not run — and the
// failure of a missing authentication step must be a refusal, never a request that proceeds with no
// scope. The anonymous routes are a separate mux for this reason; they never reach here.
func ResolveScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, ok := CallerFrom(r.Context())
		if !ok {
			platform.Problem(w, r, platform.AuthenticationRequired, "The request carries no authenticated caller")
			return
		}

		// Required rather than generated. The platform chain assigns one to every request, so its
		// absence means this handler is mounted outside that chain — and a scope built with a
		// generated correlation would produce provider evidence that correlates to no request,
		// which is an actor with no trail. Refusing surfaces the wiring defect instead.
		correlation, ok := observability.CorrelationID(r.Context())
		if !ok {
			platform.Problem(w, r, platform.Internal, "The request could not be completed")
			return
		}

		scope, err := resolve(caller, correlation)
		if err != nil {
			writeError(w, r, err)
			return
		}

		next.ServeHTTP(w, r.WithContext(db.WithScope(r.Context(), scope)))
	})
}

// resolve is the caller-to-scope rule, separated so it can be exercised directly.
func resolve(caller Caller, correlation id.UUID) (db.Scope, error) {
	if caller.Self {
		if caller.Provider || caller.Eligible || caller.Consumer != "" || !caller.Tenant.IsNil() {
			return db.Scope{}, errors.New("httpapi: a self caller carries no other authority")
		}
		// The Principal is the caller's own, and the scope opens only the self read: no Tenant, no
		// provider pool, no record of provider access (ADR-ORG-005 §5.1).
		return db.SelfScope(caller.Subject, correlation)
	}
	if caller.Consumer != "" {
		if caller.Provider || !caller.Tenant.IsNil() {
			return db.Scope{}, errors.New("httpapi: a consumer caller must carry neither provider authority nor a Tenant")
		}
		// A consumer reads across Tenants -- it asks about whichever Tenant its caller is acting
		// in -- and its scope opens only the consumer pool, which connects as
		// organization_consumer_rt and holds what the consumer's seven routes need. The provider
		// and tenant pools refuse it, and requireProvider and requireTenant refuse it before
		// either is reached.
		return db.ConsumerScope(caller.Subject, correlation)
	}

	if caller.Provider || caller.Eligible {
		// A provider caller carrying a Tenant is refused rather than narrowed to it. The two
		// readings — cross-Tenant authority, or authority over that one Tenant — differ in the
		// permissive direction, and picking one silently means the caller and the service disagree
		// about what the request did.
		if !caller.Tenant.IsNil() {
			return db.Scope{}, errors.New("httpapi: a provider caller must not also carry a Tenant")
		}
		return db.ProviderScope(caller.Subject, correlation)
	}
	return db.TenantScope(caller.Tenant, caller.Subject, correlation)
}

// reason reads the administrative reason a provider route requires.
//
// Bounded here rather than at the database so an over-long reason is reported as a bad request
// instead of arriving as an internal error from a constraint the caller cannot see. Blank is left to
// `db.ErrReasonRequired` rather than duplicated: one rule, in the package that enforces it.
func reason(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get(ReasonHeader))
	if len(value) > 1000 {
		return value[:1000]
	}
	return value
}
