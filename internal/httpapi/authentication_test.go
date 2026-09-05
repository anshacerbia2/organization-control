package httpapi

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/verify"
)

// The tests below sign real tokens and verify them through a real verifier.
//
// `verify.Claims` keeps its non-registered claims in an unexported map populated only by decoding a
// token, so a fake verifier cannot produce a Claims carrying a Tenant claim or a realm role — the two
// values this mapping exists to read. A stub would therefore only be able to test the paths that read
// nothing, which are not the paths worth testing. Signing is the cheaper honesty.

const (
	testIssuer   = "https://issuer.test/realms/scnehaux"
	testAudience = "organization-control"
	testKeyID    = "test-key"
	testClaim    = "https://scnehaux.com/tenant_id"
	testRole     = "organization-provider"
)

func testAuthConfig() AuthenticationConfig {
	return AuthenticationConfig{TenantClaim: testClaim, ProviderRole: testRole}
}

// signer mints tokens for the tests and exposes the matching public key.
type signer struct {
	key *rsa.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	// 2048 rather than a larger modulus: this is a test key, generation cost is paid on every run,
	// and the algorithm under test is the same at any size.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	return signer{key: key}
}

// sign builds a PS256 token carrying the supplied claims plus valid registered claims.
func (s signer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()

	now := time.Now().UTC()
	payload := map[string]any{
		"iss": testIssuer,
		"aud": testAudience,
		"exp": now.Add(5 * time.Minute).Unix(),
		"iat": now.Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
	}
	for name, value := range claims {
		payload[name] = value
	}

	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal a token segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	// PS256 is the one algorithm the verifier permits, and it is named here rather than taken from
	// a variable so a test cannot accidentally assert against a different one.
	head := encode(map[string]string{"alg": "PS256", "kid": testKeyID, "typ": "JWT"})
	body := encode(payload)
	signed := head + "." + body

	digest := crypto.SHA256.New()
	digest.Write([]byte(signed))
	signature, err := rsa.SignPSS(rand.Reader, s.key, crypto.SHA256, digest.Sum(nil),
		&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (s signer) verifier(t *testing.T) *verify.Verifier {
	t.Helper()
	verifier, err := verify.New(verify.Config{
		Issuer:      testIssuer,
		Audience:    testAudience,
		Keys:        verify.StaticKeys{testKeyID: &s.key.PublicKey},
		Requirement: Requirement(testAuthConfig()),
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return verifier
}

// authenticated runs one request through the middleware and reports the caller it established.
func authenticated(t *testing.T, s signer, token string) (Caller, bool, *httptest.ResponseRecorder) {
	t.Helper()

	middleware, err := Authenticate(s.verifier(t), testAuthConfig())
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	var (
		seen   Caller
		called bool
	)
	handler := middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, called = CallerFrom(r.Context())
	}))

	request := httptest.NewRequest(http.MethodPost, "/v1/memberships", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return seen, called, recorder
}

func TestAuthenticateResolvesATenantCaller(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	subject := mustID(t)
	tenantID := mustID(t)

	caller, called, recorder := authenticated(t, s, s.sign(t, map[string]any{
		"sub":     subject.String(),
		testClaim: tenantID.String(),
	}))

	if !called {
		t.Fatalf("the handler did not run; the middleware answered %d: %s",
			recorder.Code, recorder.Body.String())
	}
	switch {
	case caller.Provider:
		t.Error("a token with no provider role produced a provider caller")
	case caller.Subject != subject:
		t.Errorf("the caller's subject is %s, want %s", caller.Subject, subject)
	case caller.Tenant != tenantID:
		t.Errorf("the caller's Tenant is %s, want %s", caller.Tenant, tenantID)
	}
}

func TestAuthenticateResolvesAProviderCaller(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	subject := mustID(t)

	caller, called, recorder := authenticated(t, s, s.sign(t, map[string]any{
		"sub":          subject.String(),
		"realm_access": map[string]any{"roles": []string{"offline_access", testRole}},
	}))

	if !called {
		t.Fatalf("the handler did not run; the middleware answered %d: %s",
			recorder.Code, recorder.Body.String())
	}
	switch {
	case !caller.Provider:
		t.Error("a token carrying the provider role produced a tenant caller")
	case !caller.Tenant.IsNil():
		t.Errorf("a provider caller carries Tenant %s", caller.Tenant)
	case caller.Subject != subject:
		t.Errorf("the caller's subject is %s, want %s", caller.Subject, subject)
	}
}

// TestAuthenticateRefusesBothAuthorities is the ambiguity that must not be resolved silently.
//
// A token carrying provider authority and a Tenant could mean cross-Tenant authority or authority
// over that one Tenant. The two differ in the permissive direction.
func TestAuthenticateRefusesBothAuthorities(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	_, called, recorder := authenticated(t, s, s.sign(t, map[string]any{
		"sub":          mustID(t).String(),
		testClaim:      mustID(t).String(),
		"realm_access": map[string]any{"roles": []string{testRole}},
	}))

	if called {
		t.Error("a token carrying both authorities was admitted")
	}
	// 403 rather than 401: the token is valid and its claims do not confer a usable scope, so
	// obtaining a new token with the same claims would not help.
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a token carrying both authorities answered %d, want 403", recorder.Code)
	}
}

func TestAuthenticateRefusesATokenWithNeitherAuthority(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	_, called, recorder := authenticated(t, s, s.sign(t, map[string]any{
		"sub": mustID(t).String(),
	}))

	if called {
		t.Error("a token conferring no scope was admitted")
	}
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a token conferring no scope answered %d, want 403", recorder.Code)
	}
}

// TestAuthenticateRefusesANonUUIDSubject guards what the subject becomes.
//
// It is written into every lifecycle event and every privileged-access row as the actor. A subject
// that cannot be parsed cannot be recorded, and the alternative to refusing is evidence naming
// nobody.
func TestAuthenticateRefusesANonUUIDSubject(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	_, called, recorder := authenticated(t, s, s.sign(t, map[string]any{
		"sub":     "service-account-organization",
		testClaim: mustID(t).String(),
	}))

	if called {
		t.Error("a token with a non-UUID subject was admitted")
	}
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a non-UUID subject answered %d, want 403", recorder.Code)
	}
}

func TestAuthenticateRefusesAMissingOrMalformedHeader(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	valid := s.sign(t, map[string]any{
		"sub":     mustID(t).String(),
		testClaim: mustID(t).String(),
	})

	middleware, err := Authenticate(s.verifier(t), testAuthConfig())
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	cases := map[string]string{
		"absent":           "",
		"no scheme":        valid,
		"wrong scheme":     "Basic " + valid,
		"scheme only":      "Bearer ",
		"tampered payload": strings.Replace(valid, ".", ".x", 1),
		"unsigned":         strings.Join(strings.Split(valid, ".")[:2], ".") + ".",
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			handler := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))

			request := httptest.NewRequest(http.MethodPost, "/v1/memberships", nil)
			if header != "" {
				request.Header.Set("Authorization", header)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if called {
				t.Fatal("the handler ran")
			}
			if recorder.Code != http.StatusUnauthorized {
				t.Errorf("answered %d, want 401", recorder.Code)
			}
			// The verifier distinguishes an unknown key from a bad signature from an expired
			// token. That is useful in a log and is an oracle in a response body.
			for _, leak := range []string{"signature", "kid", "expired", "issuer", "audience"} {
				if strings.Contains(strings.ToLower(recorder.Body.String()), leak) {
					t.Errorf("the refusal disclosed %q:\n%s", leak, recorder.Body.String())
				}
			}
		})
	}
}

// TestRequirementAndMapperAgree is the property that keeps one rule from becoming two.
//
// The verifier applies Requirement and the middleware applies callerFromClaims. If they disagreed, a
// token could pass verification and then fail to map — a 403 on a token the service accepted — or the
// reverse. They are the same function here, and this asserts the equivalence rather than trusting it
// to stay true.
func TestRequirementAndMapperAgree(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	cfg := testAuthConfig()
	requirement := Requirement(cfg)

	// A verifier with a requirement that admits everything, so this test observes the mapper and
	// the requirement independently on the same claim sets.
	permissive, err := verify.New(verify.Config{
		Issuer:      testIssuer,
		Audience:    testAudience,
		Keys:        verify.StaticKeys{testKeyID: &s.key.PublicKey},
		Requirement: verify.RequirementFunc(func(verify.Claims) error { return nil }),
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	sets := []map[string]any{
		{"sub": mustID(t).String(), testClaim: mustID(t).String()},
		{"sub": mustID(t).String(), "realm_access": map[string]any{"roles": []string{testRole}}},
		{"sub": mustID(t).String()},
		{"sub": "not-a-uuid", testClaim: mustID(t).String()},
		{"sub": mustID(t).String(), testClaim: "not-a-uuid"},
		{"sub": mustID(t).String(), testClaim: mustID(t).String(),
			"realm_access": map[string]any{"roles": []string{testRole}}},
	}

	for index, set := range sets {
		claims, err := permissive.Verify(s.sign(t, set))
		if err != nil {
			t.Fatalf("set %d did not verify: %v", index, err)
		}

		_, mapErr := callerFromClaims(claims, cfg)
		requireErr := requirement.Require(claims)

		if (mapErr == nil) != (requireErr == nil) {
			t.Errorf("set %d: the mapper says %v and the requirement says %v",
				index, mapErr, requireErr)
		}
	}
}

func TestAuthenticateRefusesToBuildWithoutItsClaimNames(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	verifier := s.verifier(t)

	cases := map[string]AuthenticationConfig{
		"no tenant claim":  {ProviderRole: testRole},
		"no provider role": {TenantClaim: testClaim},
		"neither":          {},
	}
	for name, cfg := range cases {
		if _, err := Authenticate(verifier, cfg); err == nil {
			t.Errorf("%s: Authenticate built a middleware that could confer no scope", name)
		}
	}

	if _, err := Authenticate(nil, testAuthConfig()); err == nil {
		t.Error("Authenticate built a middleware with no verifier")
	}
}
