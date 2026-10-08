package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/observability"
	"github.com/anshacerbia2/foundation-platform/verify"

	"github.com/anshacerbia2/organization-control/internal/authority"
)

// activationRoute is whether a path is one an eligible caller may reach: the activation routes.
func activationRoute(path string) bool {
	return path == "/v1/provider-activations" || strings.HasPrefix(path, "/v1/provider-activations/")
}

// contextsPrefix and contextsSuffix bound the one route a self caller reaches.
const (
	contextsPrefix = "/v1/principals/"
	contextsSuffix = "/contexts"
)

// selfTarget reports the principal_id a request reads the contexts of, when it is the context list
// route: GET /v1/principals/{principal_id}/contexts, exactly (ADR-ORG-005 §5.1). Any other method,
// path or segment count is not the route, and the caller is authorized as on every other route.
func selfTarget(r *http.Request) (id.UUID, bool) {
	if r.Method != http.MethodGet {
		return id.UUID{}, false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, contextsPrefix)
	if !ok {
		return id.UUID{}, false
	}
	segment, ok := strings.CutSuffix(rest, contextsSuffix)
	if !ok || segment == "" || strings.Contains(segment, "/") {
		return id.UUID{}, false
	}
	principal, err := id.Parse(segment)
	if err != nil || principal.IsNil() {
		return id.UUID{}, false
	}
	return principal, true
}

// TokenVerifier is the one thing authentication needs from a token library.
//
// An interface rather than *verify.Verifier so this package can be tested without an RSA key set or
// a JWKS endpoint, and so it cannot reach past the single method it uses. *verify.Verifier satisfies
// it.
type TokenVerifier interface {
	Verify(token string) (verify.Claims, error)
}

// The claims a caller is read from, as STD-IAM-002 §3.2 names them. Constants rather than
// configuration: the standard fixes the names, and a setting would let one deployment read a claim
// another deployment's realm never writes.
const (
	// PrincipalIDClaim is the enterprise identifier, and the actor every event and every evidence row
	// names. `sub` is the protocol subject and is not read: STD-IAM-002 §3.2 keeps it out of every
	// foreign key.
	PrincipalIDClaim = "principal_id"

	// SubjectTypeClaim says whether the Principal is a person or a workload.
	SubjectTypeClaim = "subject_type"

	// TenantIDClaim selects the one Tenant a tenant-scoped caller acts in. It confers nothing: the
	// caller administers that Tenant only by its records (ADR-ORG-003).
	TenantIDClaim = "tenant_id"

	// AuthContextClassClaim and AuthTimeClaim are the assurance claims STD-IAM-002 §3.2 makes
	// mandatory on a privileged token, which a provider's and a Tenant administrator's are.
	AuthContextClassClaim = "acr"
	AuthTimeClaim         = "auth_time"

	// WorkloadOwnerClaim is mandatory on a workload token (STD-IAM-002 §3.5 rule 7).
	WorkloadOwnerClaim = "workload_owner"
)

// CallerRecords reads the two records a caller's authority comes from (ADR-ORG-001 §5.11).
//
// A token says who the caller is; whether that Principal is a provider or a registered consumer is
// this service's own record, read for each request, so a revoked grant or a retired consumer stops at
// the next request rather than when the token expires. An interface so this package reads no table:
// internal/authority implements it on the provider connections.
type CallerRecords interface {
	// ProviderStanding reads the Principal's provider authority over this service: whether it holds
	// a grant, and whether authority is in force by an emergency grant or an approved activation
	// (ADR-ORG-002).
	ProviderStanding(ctx context.Context, principal id.UUID) (authority.Standing, error)

	// ConsumerFor names the active projection consumer registered with the Principal, or "" when
	// there is none.
	ConsumerFor(ctx context.Context, principal id.UUID) (string, error)

	// TenantStanding reads, in the Tenant, the Principal's Membership, the Tenant's status and its
	// tenant administration grant (ADR-ORG-003 §5.3).
	TenantStanding(ctx context.Context, principal, tenant, correlation id.UUID) (authority.TenantStanding, error)
}

// EmergencyUseRecorder records that a request was authorized by the Principal's emergency grant.
type EmergencyUseRecorder interface {
	RecordEmergencyUse(ctx context.Context, principal id.UUID) error
}

// AuthenticationConfig is what authentication needs besides the verifier.
type AuthenticationConfig struct {
	// Records is where provider grants and consumer registrations are read. Required.
	Records CallerRecords

	// Logger reports every request an emergency grant authorizes (ADR-ORG-002 §5.2). Nil uses
	// slog.Default.
	Logger *slog.Logger

	// EmergencyUses records each request an emergency grant authorizes, which is how the grant is
	// validated (ADR-ORG-002 §5.2). Nil records nothing.
	EmergencyUses EmergencyUseRecorder

	// Consumers is whether consumer authority exists: true when the consumer pool is configured.
	// Without the pool no consumer route could be served, so a consumer token is refused instead
	// of admitted to a scope nothing can open.
	Consumers bool
}

// Authenticate builds the middleware that resolves a Caller from a bearer token.
//
// It is supplied to `platform.Chain` rather than wrapped around the mux here, so it runs at the
// position TDD-foundation-platform-002 fixes: after load shedding, so rejecting overload costs no
// signature verification, and before anything that acts on a caller.
//
// # What this function decides, and what it does not
//
// It decides who the caller is and which of the three scopes they may ask for
// (TDD-organization-control-001 §Caller Authority). It decides nothing about whether they may
// perform the operation — the routes do that, via `requireTenant` and `requireProvider`, and the
// domain refuses independently. A transport layer that made the authorization decision would make
// it somewhere the domain cannot see it.
func Authenticate(verifier TokenVerifier, cfg AuthenticationConfig) (Middleware, error) {
	switch {
	case verifier == nil:
		return nil, errors.New("httpapi: a token verifier is required")
	case cfg.Records == nil:
		// Refused rather than defaulted to "nobody is a provider". A middleware unable to read the
		// records would refuse every provider and consumer, and the deployment would look broken
		// for a reason nothing names.
		return nil, errors.New("httpapi: the caller records are required")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearer(r)
			if !ok {
				platform.Problem(w, r, platform.AuthenticationRequired,
					"A bearer token is required")
				return
			}

			claims, err := verifier.Verify(token)
			if err != nil {
				// A requirement failure is 403, and everything else is 401.
				//
				// The distinction matters to a client. 401 says the credential was not accepted and
				// a fresh one may work. A requirement failure says the signature, issuer, audience,
				// and expiry were all fine and the claims confer no authority here — a new token
				// carrying the same claims would fail identically, so telling the client to
				// re-authenticate sends it into a loop.
				if errors.Is(err, verify.ErrClaimRequirement) {
					platform.Problem(w, r, platform.Forbidden, insufficientClaims)
					return
				}

				// The verifier's message is not returned. It distinguishes an unknown key from a
				// bad signature from an expired token, which is useful to an operator reading logs
				// and is a probe oracle for anybody else.
				platform.Problem(w, r, platform.AuthenticationRequired,
					"The bearer token was not accepted")
				return
			}

			// Unreachable for a claim failure when the verifier carries this package's own
			// Requirement, which applies the same rule and rejects first — the composition root wires
			// it that way. It stays because `Authenticate` accepts any TokenVerifier.
			presented, err := presentedFromClaims(claims)
			if err != nil {
				platform.Problem(w, r, platform.Forbidden, err.Error())
				return
			}

			var caller Caller
			// A person reading their own contexts is admitted with no record read: the route reaches
			// only their own rows, so no grant, Membership or Tenant has to be established first
			// (ADR-ORG-005 §5.1). Anyone else on the route, and this person on any other, is
			// authorized below as before.
			if target, ok := selfTarget(r); ok && !presented.workload && target == presented.principal {
				caller = Caller{Subject: presented.principal, Self: true}
			} else {
				caller, err = authorize(r.Context(), presented, cfg)
			}
			switch {
			case errors.Is(err, errRecords):
				// The records could not be read. Nobody is admitted: a provider let through because
				// the grant table was unreachable is the failure this read exists to prevent.
				platform.Problem(w, r, platform.DependencyUnavailable, "The caller's authority could not be read")
				return
			case err != nil:
				platform.Problem(w, r, platform.Forbidden, err.Error())
				return
			case caller.Eligible && !activationRoute(r.URL.Path):
				// Before a transaction opens: an eligible caller holds no authority in force, and the
				// activation routes are how it gets some (TDD-organization-control-001 §Provider
				// Activation).
				platform.Problem(w, r, platform.Forbidden,
					"The caller's provider grant has no activation in force; request one at /v1/provider-activations")
				return
			case caller.Emergency:
				logger := cfg.Logger
				if logger == nil {
					logger = slog.Default()
				}
				logger.WarnContext(r.Context(), "a provider acted on an emergency grant",
					slog.String("principal_id", caller.Subject.String()), slog.String("method", r.Method),
					slog.String("route", r.URL.Path))
				// The use is the grant's validation. A failure to record it is reported and does not refuse
				// the request: the grant is in force, and a break-glass path that fails on its own
				// bookkeeping fails when it is needed.
				if cfg.EmergencyUses != nil {
					if err := cfg.EmergencyUses.RecordEmergencyUse(r.Context(), caller.Subject); err != nil {
						logger.ErrorContext(r.Context(), "an emergency grant's use could not be recorded",
							slog.String("principal_id", caller.Subject.String()), slog.String("error", err.Error()))
					}
				}
			}

			next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), caller)))
		})
	}, nil
}

// Middleware is the shape platform.Chain accepts.
type Middleware = func(http.Handler) http.Handler

// insufficientClaims is the 403 detail for a token whose claims confer no scope.
//
// Fixed text rather than the requirement's own message. `verify` wraps a requirement failure with
// two `%w` verbs, which `errors.Unwrap` cannot split, so recovering the inner message would mean
// string-matching the outer prefix — brittle, and it would put the verifier's phrasing on the wire
// the moment that prefix changed. The ways to fail are named here instead, which is what a caller
// needs to fix their own token and discloses nothing about this service's configuration.
const insufficientClaims = "The token's claims confer no scope: it must carry a principal_id and a " +
	"subject_type, and either a tenant_id with a person's acr of aal2 or higher and auth_time, or no " +
	"tenant_id with acr and auth_time for a person or workload_owner for a workload"

// notAdministrator is the 403 detail for a tenant token whose Principal does not administer the
// Tenant. One message for the three facts: the caller learns that it does not administer the
// Tenant, not which record is missing.
const notAdministrator = "The token's principal_id does not administer this Tenant: that needs an active " +
	"Membership in an active Tenant and a tenant administration grant"

// Requirement is the claim rule `verify.New` refuses to build without.
//
// foundation-platform makes it mandatory because a verifier with no requirement checks signatures,
// issuer, audience, and expiry — the mechanics — and nothing about what the claims mean, which
// STD-IAM-002 §3.5 does not accept. The rule belongs here because it is stated in terms of claims
// foundation-platform is forbidden from naming.
//
// It is the claim half of authentication, the same function the middleware runs, with the result
// discarded. The record half needs a request context and a database and so cannot run inside the
// verifier; a token that passes here and names nobody the records know is refused by the middleware.
func Requirement() verify.RequirementFunc {
	return func(claims verify.Claims) error {
		_, err := presentedFromClaims(claims)
		return err
	}
}

// bearer reads the Authorization header.
//
// The scheme comparison is case-insensitive because RFC 7235 makes it so, and the value is not
// trimmed beyond the single separating space: a token with surrounding whitespace is a malformed
// header, and accepting it would mean two different header values authenticate the same caller.
func bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// presented is what a token claims about its bearer, before any record is read.
type presented struct {
	principal id.UUID
	workload  bool
	tenant    id.UUID
}

// presentedFromClaims applies the claim rule of TDD-organization-control-001 §Caller Authority.
//
// A token with a Tenant is a candidate Tenant administrator, and must be a person's at aal2 or higher
// with auth_time (ADR-ORG-003 §5.3). One without is a candidate provider
// when it names a person, and must then carry the assurance claims a privileged token carries; or a
// candidate consumer when it names a workload, and must then carry its owner. Realm roles are not
// read: STD-IAM-002 §3.2 keeps them out of every access token, and a role the token did carry would
// be a grant the kernel holds, which ADR-ORG-001 §5.11 does not let it.
func presentedFromClaims(claims verify.Claims) (presented, error) {
	raw, _ := claims.String(PrincipalIDClaim)
	principal, err := id.Parse(strings.TrimSpace(raw))
	if err != nil || principal.IsNil() {
		// The principal becomes `db.Scope.Actor`, which every lifecycle event and every
		// privileged-access record is attributed to. One that is not an identifier cannot be
		// recorded as an actor, so the alternative to refusing is evidence naming nobody.
		return presented{}, errors.New("the token carries no principal_id that is a valid identifier")
	}

	subjectType, _ := claims.String(SubjectTypeClaim)
	var workload bool
	switch subjectType {
	case "human":
	case "workload":
		workload = true
	default:
		return presented{}, errors.New("the token's subject_type is not human or workload")
	}

	// Present at all means tenant-scoped. An empty or non-string tenant_id is refused rather than
	// read as absent: read as absent, a person's token would fall through to the provider path,
	// which is the permissive reading of an ambiguous claim.
	if claims.Has(TenantIDClaim) {
		rawTenant, _ := claims.String(TenantIDClaim)
		tenant, err := id.Parse(strings.TrimSpace(rawTenant))
		if err != nil || tenant.IsNil() {
			return presented{}, errors.New("the tenant_id claim is not a valid identifier")
		}
		// Tenant administration is privileged access, and ADR-IAM-004 requires two factors for it.
		// Only a person administers a Tenant: a workload carries no acr to step up.
		if workload {
			return presented{}, errors.New("a token with a tenant_id must name a person: a workload administers no Tenant")
		}
		if acr, _ := claims.String(AuthContextClassClaim); !atLeastAAL2(acr) {
			return presented{}, errors.New("a token with a tenant_id must carry acr aal2 or higher")
		}
		if _, ok := claims.Int64(AuthTimeClaim); !ok {
			return presented{}, errors.New("a token with a tenant_id must carry auth_time")
		}
		return presented{principal: principal, tenant: tenant}, nil
	}

	if workload {
		if owner, ok := claims.String(WorkloadOwnerClaim); !ok || strings.TrimSpace(owner) == "" {
			return presented{}, errors.New("a workload token without a Tenant must carry workload_owner")
		}
		return presented{principal: principal, workload: true}, nil
	}

	// STD-IAM-002 §3.1.1: a provider-scope token carries acr and auth_time. Without auth_time no
	// step-up rule could ever be evaluated, so its absence is a refusal, not a downgrade.
	if acr, ok := claims.String(AuthContextClassClaim); !ok || strings.TrimSpace(acr) == "" {
		return presented{}, errors.New("a token without a Tenant must carry acr")
	}
	if _, ok := claims.Int64(AuthTimeClaim); !ok {
		return presented{}, errors.New("a token without a Tenant must carry auth_time")
	}
	return presented{principal: principal}, nil
}

// atLeastAAL2 is whether acr is aal2 or a higher level, in STD-IAM-002 §3.2's order: aal1, aal2,
// phr. Any other value, the kernel's unmapped 0 and 1 included, is below aal1.
func atLeastAAL2(acr string) bool {
	return acr == "aal2" || acr == "phr"
}

// errRecords marks a failure to read the caller records, which is answered 503 rather than 403.
var errRecords = errors.New("httpapi: the caller records could not be read")

// authorize reads the records the token's caller needs, and builds the caller.
//
// A token with a Tenant reads that Tenant's records: the caller administers it only with an active
// Membership, an active Tenant and a tenant administration grant (ADR-ORG-003 §5.3). Otherwise one
// record per request, never both: a person can only be a provider and a workload only a consumer, so
// the token's subject_type decides which is read. A workload holding a provider grant gains nothing
// from it — STD-IAM-002 §3.2 keeps provider authority off workload tokens — and a person registered
// as a consumer is not one.
func authorize(ctx context.Context, p presented, cfg AuthenticationConfig) (Caller, error) {
	if !p.tenant.IsNil() {
		correlation, _ := observability.CorrelationID(ctx)
		standing, err := cfg.Records.TenantStanding(ctx, p.principal, p.tenant, correlation)
		if err != nil {
			return Caller{}, errors.Join(errRecords, err)
		}
		if !standing.Administers() {
			return Caller{}, errors.New(notAdministrator)
		}
		return Caller{Subject: p.principal, Tenant: p.tenant}, nil
	}

	if p.workload {
		if !cfg.Consumers {
			return Caller{}, errors.New("the token names a workload and no Tenant, and consumer authority is not configured")
		}
		consumer, err := cfg.Records.ConsumerFor(ctx, p.principal)
		if err != nil {
			return Caller{}, errors.Join(errRecords, err)
		}
		if consumer == "" {
			return Caller{}, errors.New("the token names a workload and no Tenant, and no active consumer is registered with its principal_id")
		}
		return Caller{Subject: p.principal, Consumer: consumer}, nil
	}

	standing, err := cfg.Records.ProviderStanding(ctx, p.principal)
	if err != nil {
		return Caller{}, errors.Join(errRecords, err)
	}
	if !standing.Holder {
		return Caller{}, errors.New("the token carries no Tenant, and its principal_id holds no provider grant")
	}
	// A holder with no authority in force is eligible: it reaches the activation routes alone
	// (ADR-ORG-002 §5.1, TDD-organization-control-001 §Provider Activation).
	return Caller{Subject: p.principal, Provider: standing.InForce, Eligible: !standing.InForce,
		Emergency: standing.Emergency, Activation: standing.Activation}, nil
}

// ReportTokenType wraps a verifier for ORGANIZATION_TOKEN_TYPE=report. A token whose header typ is
// not at+jwt is accepted, as the verifier without RequireAccessTokenType accepts it, and logged with
// the client it was issued to, so an operator sees which clients still need the token profile before
// the service moves to enforce. The log carries no claim value but the client identifier.
func ReportTokenType(verifier TokenVerifier, logger *slog.Logger) TokenVerifier {
	return reportingVerifier{verifier: verifier, logger: logger}
}

type reportingVerifier struct {
	verifier TokenVerifier
	logger   *slog.Logger
}

func (v reportingVerifier) Verify(token string) (verify.Claims, error) {
	claims, err := v.verifier.Verify(token)
	if err != nil {
		return claims, err
	}
	if typ := claims.TokenType(); !strings.EqualFold(typ, "at+jwt") && !strings.EqualFold(typ, "application/at+jwt") {
		client, _ := claims.String("azp")
		v.logger.Warn("a caller's token is not typed at+jwt; ORGANIZATION_TOKEN_TYPE=enforce would refuse it",
			slog.String("typ", typ), slog.String("client", client))
	}
	return claims, nil
}
