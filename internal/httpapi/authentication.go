package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/verify"
)

// TokenVerifier is the one thing authentication needs from a token library.
//
// An interface rather than *verify.Verifier so this package can be tested without an RSA key set or
// a JWKS endpoint, and so it cannot reach past the single method it uses. *verify.Verifier satisfies
// it.
type TokenVerifier interface {
	Verify(token string) (verify.Claims, error)
}

// AuthenticationConfig names the two claims that decide a caller's authority.
//
// They are configuration rather than constants because the claim namespace belongs to the realm this
// deployable is pointed at. `config.Config` requires both, so a deployment cannot start with either
// unset.
type AuthenticationConfig struct {
	// TenantClaim carries the Tenant a caller administers, as a UUID string.
	TenantClaim string

	// ProviderRole is the realm role conferring cross-Tenant authority. Read from `realm_access.
	// roles`, which is where Keycloak puts realm roles.
	ProviderRole string
}

// Authenticate builds the middleware that resolves a Caller from a bearer token.
//
// It is supplied to `platform.Chain` rather than wrapped around the mux here, so it runs at the
// position TDD-foundation-platform-002 fixes: after load shedding, so rejecting overload costs no
// signature verification, and before anything that acts on a caller.
//
// # What this function decides, and what it does not
//
// It decides who the caller is and which of the two isolation scopes they may ask for. It decides
// nothing about whether they may perform the operation — the routes do that, via `requireTenant` and
// `requireProvider`, and the domain refuses independently. A transport layer that made the
// authorization decision would make it somewhere the domain cannot see it.
func Authenticate(verifier TokenVerifier, cfg AuthenticationConfig) (Middleware, error) {
	switch {
	case verifier == nil:
		return nil, errors.New("httpapi: a token verifier is required")
	case strings.TrimSpace(cfg.TenantClaim) == "":
		return nil, errors.New("httpapi: the tenant claim name is required")
	case strings.TrimSpace(cfg.ProviderRole) == "":
		return nil, errors.New("httpapi: the provider role name is required")
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

			// Unreachable when the verifier carries this package's own Requirement, which applies
			// the same rule and rejects first — the composition root wires it that way. It stays
			// because `Authenticate` accepts any TokenVerifier: a caller supplying a verifier with
			// a weaker requirement would otherwise reach a handler with no caller in the context,
			// and `ResolveScope` would report that as 401 rather than as what it is.
			caller, err := callerFromClaims(claims, cfg)
			if err != nil {
				platform.Problem(w, r, platform.Forbidden, err.Error())
				return
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
// the moment that prefix changed. The three ways to fail are named here instead, which is what a
// caller needs to fix their own token and discloses nothing about this service's configuration.
const insufficientClaims = "The token's claims confer no scope: it must carry either the provider " +
	"role or a Tenant claim, not both and not neither, and its subject must be an identifier"

// Requirement is the claim rule `verify.New` refuses to build without.
//
// foundation-platform makes it mandatory because a verifier with no requirement checks signatures,
// issuer, audience, and expiry — the mechanics — and nothing about what the claims mean, which
// STD-IAM-002 §3.5 does not accept. The rule belongs here because it is stated in terms of claims
// foundation-platform is forbidden from naming.
//
// It is the same function `Authenticate` uses to build the caller, with the caller discarded. Two
// implementations of one rule would drift, and the direction they drift in is the dangerous one: a
// verifier that admits a token the mapper then cannot place would produce a 403 on a valid token, and
// a verifier stricter than the mapper would reject callers the service means to serve.
func Requirement(cfg AuthenticationConfig) verify.RequirementFunc {
	return func(claims verify.Claims) error {
		_, err := callerFromClaims(claims, cfg)
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

// realmAccess is the Keycloak claim carrying realm roles.
type realmAccess struct {
	Roles []string `json:"roles"`
}

// callerFromClaims maps verified claims onto the two authorities this service recognises.
//
// The provider check comes first and is exclusive: a token carrying the provider role AND a Tenant
// claim is refused rather than resolved to either. The two readings — cross-Tenant authority, or
// authority over that one Tenant — differ in the permissive direction, and a service that silently
// picked one would disagree with the caller about what the request did. `db.ProviderScope` refuses
// the same combination, so the refusal does not depend on this function being the only check.
func callerFromClaims(claims verify.Claims, cfg AuthenticationConfig) (Caller, error) {
	subject, err := id.Parse(strings.TrimSpace(claims.Subject))
	if err != nil {
		// The subject becomes `db.Scope.Actor`, which every lifecycle event and every
		// privileged-access record is attributed to. A non-UUID subject cannot be recorded as an
		// actor, so the alternative to refusing is evidence naming nobody.
		return Caller{}, errors.New("the token subject is not a valid identifier")
	}

	provider := false
	if raw, ok := claims.Raw("realm_access"); ok {
		var access realmAccess
		if err := json.Unmarshal(raw, &access); err != nil {
			return Caller{}, errors.New("the realm_access claim is not an object")
		}
		for _, role := range access.Roles {
			if role == cfg.ProviderRole {
				provider = true
				break
			}
		}
	}

	rawTenant, hasTenant := claims.String(cfg.TenantClaim)
	rawTenant = strings.TrimSpace(rawTenant)
	hasTenant = hasTenant && rawTenant != ""

	if provider {
		if hasTenant {
			return Caller{}, errors.New(
				"the token carries both provider authority and a Tenant, and the two confer " +
					"different scopes")
		}
		return Caller{Subject: subject, Provider: true}, nil
	}

	if !hasTenant {
		// Refused here rather than left to produce an empty scope downstream. `db.TenantScope`
		// would reject a nil Tenant, but that rejection is a programming-error path reported as
		// 500 — and a caller whose token simply lacks the claim deserves to be told that.
		return Caller{}, errors.New("the token carries neither provider authority nor a Tenant")
	}

	tenantID, err := id.Parse(rawTenant)
	if err != nil {
		return Caller{}, errors.New("the Tenant claim is not a valid identifier")
	}
	return Caller{Subject: subject, Tenant: tenantID}, nil
}
