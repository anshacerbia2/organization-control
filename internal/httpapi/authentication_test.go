package httpapi

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/verify"

	"github.com/anshacerbia2/organization-control/internal/authority"
)

// The tests below sign real tokens and verify them through a real verifier.
//
// `verify.Claims` keeps its non-registered claims in an unexported map populated only by decoding a
// token, so a fake verifier cannot produce a Claims carrying a principal_id or a tenant_id — the
// values this mapping exists to read. A stub would therefore only be able to test the paths that read
// nothing, which are not the paths worth testing. Signing is the cheaper honesty.
//
// The records are a fake: which Principals hold a provider grant and which are registered consumers
// is a database read, asserted as the provider role in internal/authority's integration suite.

const (
	testIssuer   = "https://issuer.test/realms/scnehaux"
	testAudience = "organization-control-api"
	testKeyID    = "test-key"
)

// fakeRecords answers the two record reads from maps. A provider in providers is in force; one in
// eligible holds a grant with no activation; one in emergency is in force by an emergency grant.
type fakeRecords struct {
	providers map[id.UUID]bool
	eligible  map[id.UUID]bool
	emergency map[id.UUID]bool
	consumers map[id.UUID]string
	// standings is each Principal's standing in each Tenant; one absent stands nowhere.
	standings map[[2]id.UUID]authority.TenantStanding
	err       error
	reads     int
}

func (f *fakeRecords) ProviderStanding(_ context.Context, principal id.UUID) (authority.Standing, error) {
	f.reads++
	switch {
	case f.emergency[principal]:
		return authority.Standing{Holder: true, InForce: true, Emergency: true}, f.err
	case f.providers[principal]:
		return authority.Standing{Holder: true, InForce: true, Activation: testActivation}, f.err
	case f.eligible[principal]:
		return authority.Standing{Holder: true}, f.err
	}
	return authority.Standing{}, f.err
}

func (f *fakeRecords) ConsumerFor(_ context.Context, principal id.UUID) (string, error) {
	f.reads++
	return f.consumers[principal], f.err
}

func (f *fakeRecords) TenantStanding(_ context.Context, principal, tenant, _ id.UUID) (authority.TenantStanding, error) {
	f.reads++
	return f.standings[[2]id.UUID{principal, tenant}], f.err
}

// The Principals the fake records know.
var (
	testProvider = id.MustParse("01a0f64a-c533-7000-a956-c3f095484a01")
	// testActivation is the activation in force for a provider in providers.
	testActivation       = id.MustParse("01a0f64a-c533-7000-a956-c3f095484a05")
	testConsumerWorkload = id.MustParse("01a0f64a-c533-7000-a956-c3f095484a02")
	testConsumerName     = "foundation-reference"

	// testAdministrator administers testTenant: an active Membership, an active Tenant and a grant.
	testAdministrator = id.MustParse("01a0f64a-c533-7000-a956-c3f095484a03")
	testTenant        = id.MustParse("01a0f64a-c533-7000-a956-c3f095484a04")
)

func testRecords() *fakeRecords {
	return &fakeRecords{
		providers: map[id.UUID]bool{testProvider: true},
		consumers: map[id.UUID]string{testConsumerWorkload: testConsumerName},
		standings: map[[2]id.UUID]authority.TenantStanding{
			{testAdministrator, testTenant}: {Member: true, TenantActive: true, Granted: true},
		},
	}
}

func testAuthConfig() AuthenticationConfig {
	return AuthenticationConfig{Records: testRecords(), Consumers: true}
}

// The three token shapes of TDD-organization-control-001 §Caller Authority.
func tenantClaims(principal, tenant id.UUID) map[string]any {
	return map[string]any{"sub": "kc-user", "principal_id": principal.String(), "subject_type": "human",
		"tenant_id": tenant.String(), "acr": "aal2", "auth_time": time.Now().Unix()}
}

func providerClaims(principal id.UUID) map[string]any {
	return map[string]any{"sub": "kc-user", "principal_id": principal.String(), "subject_type": "human",
		"acr": "2", "auth_time": time.Now().Unix()}
}

func consumerClaims(principal id.UUID) map[string]any {
	return map[string]any{"sub": "service-account-reference", "principal_id": principal.String(),
		"subject_type": "workload", "workload_owner": "01a0f64a-c533-7000-a956-c3f095484a0f"}
}

func without(claims map[string]any, names ...string) map[string]any {
	out := map[string]any{}
	for k, v := range claims {
		out[k] = v
	}
	for _, name := range names {
		delete(out, name)
	}
	return out
}

func with(claims map[string]any, name string, value any) map[string]any {
	out := without(claims)
	out[name] = value
	return out
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
	return s.signTyped(t, "JWT", claims)
}

// signTyped is sign with the header typ given.
func (s signer) signTyped(t *testing.T, typ string, claims map[string]any) string {
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
	head := encode(map[string]string{"alg": "PS256", "kid": testKeyID, "typ": typ})
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
		Requirement: Requirement(),
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return verifier
}

// authenticated runs one request through the middleware and reports the caller it established.
func authenticated(t *testing.T, s signer, cfg AuthenticationConfig, token string) (Caller, bool, *httptest.ResponseRecorder) {
	t.Helper()

	middleware, err := Authenticate(s.verifier(t), cfg)
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

func TestAuthenticateResolvesATenantCallerByPrincipalID(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	principal, tenantID := testAdministrator, testTenant
	records := testRecords()

	caller, called, recorder := authenticated(t, s, AuthenticationConfig{Records: records, Consumers: true},
		s.sign(t, tenantClaims(principal, tenantID)))

	if !called {
		t.Fatalf("the handler did not run; the middleware answered %d: %s", recorder.Code, recorder.Body.String())
	}
	switch {
	case caller.Provider || caller.Consumer != "":
		t.Errorf("a tenant token produced %+v", caller)
	case caller.Subject != principal:
		t.Errorf("the actor is %s, want the principal_id %s, never sub", caller.Subject, principal)
	case caller.Tenant != tenantID:
		t.Errorf("the caller's Tenant is %s, want %s", caller.Tenant, tenantID)
	}
	if records.reads != 1 {
		t.Errorf("a tenant token read %d records, want its standing in the Tenant once", records.reads)
	}
}

// A tenant token is admitted only with all three facts in the Tenant it selects (ADR-ORG-003 §5.3).
// Each missing alone is refused with the same message, and so is the right Principal in another
// Tenant: tenant_id selects, and confers nothing.
func TestATenantTokenNeedsAllThreeFactsInItsTenant(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	other := mustID(t)
	cases := map[string]authority.TenantStanding{
		"not a member":      {TenantActive: true, Granted: true},
		"Tenant not active": {Member: true, Granted: true},
		"no grant":          {Member: true, TenantActive: true},
	}
	var messages []string
	for name, standing := range cases {
		records := testRecords()
		records.standings[[2]id.UUID{testAdministrator, other}] = standing
		_, called, recorder := authenticated(t, s, AuthenticationConfig{Records: records, Consumers: true},
			s.sign(t, tenantClaims(testAdministrator, other)))
		if called || recorder.Code != http.StatusForbidden {
			t.Errorf("%s: answered %d, called=%t, want 403", name, recorder.Code, called)
		}
		messages = append(messages, recorder.Body.String())
	}
	for _, message := range messages[1:] {
		if message != messages[0] {
			t.Errorf("the refusals differ, and so tell which record is missing:\n%s\n%s", messages[0], message)
		}
	}

	_, called, recorder := authenticated(t, s, testAuthConfig(), s.sign(t, tenantClaims(testAdministrator, mustID(t))))
	if called || recorder.Code != http.StatusForbidden {
		t.Errorf("the administrator of one Tenant selecting another answered %d, called=%t, want 403", recorder.Code, called)
	}
}

// Tenant administration is privileged access at aal2 (ADR-IAM-004), and only a person holds it. The
// claims are checked before any record is read.
func TestATenantTokenNeedsAPersonAtAAL2(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	admin := tenantClaims(testAdministrator, testTenant)
	for name, claims := range map[string]map[string]any{
		"no acr":          without(admin, "acr"),
		"acr aal1":        with(admin, "acr", "aal1"),
		"acr unmapped 1":  with(admin, "acr", "1"),
		"no auth_time":    without(admin, "auth_time"),
		"a workload":      with(admin, "subject_type", "workload"),
		"acr not aal2ish": with(admin, "acr", "AAL2"),
	} {
		records := testRecords()
		_, called, recorder := authenticated(t, s, AuthenticationConfig{Records: records, Consumers: true},
			s.sign(t, claims))
		if called || recorder.Code != http.StatusForbidden {
			t.Errorf("%s: answered %d, called=%t, want 403", name, recorder.Code, called)
		}
		if records.reads != 0 {
			t.Errorf("%s: read %d records before refusing the claims", name, records.reads)
		}
	}
	_, called, recorder := authenticated(t, s, testAuthConfig(), s.sign(t, with(admin, "acr", "phr")))
	if !called {
		t.Errorf("acr phr, above aal2, answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAuthenticateResolvesAProviderFromItsGrant(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	caller, called, recorder := authenticated(t, s, testAuthConfig(), s.sign(t, providerClaims(testProvider)))

	if !called {
		t.Fatalf("the handler did not run; the middleware answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if !caller.Provider || !caller.Tenant.IsNil() || caller.Subject != testProvider {
		t.Errorf("a granted provider produced %+v", caller)
	}
}

func TestAuthenticateResolvesAConsumerFromItsRegistration(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	caller, called, recorder := authenticated(t, s, testAuthConfig(), s.sign(t, consumerClaims(testConsumerWorkload)))

	if !called {
		t.Fatalf("the handler did not run; the middleware answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if caller.Consumer != testConsumerName || caller.Provider || caller.Subject != testConsumerWorkload {
		t.Errorf("a registered consumer produced %+v", caller)
	}
}

// Every refusal below is 403: the token is authentic, and its claims or the records confer no scope,
// so a new token carrying the same claims would fail the same way.
func TestAuthenticateRefusesWhatConfersNoScope(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	stranger := mustID(t)
	cases := map[string]struct {
		cfg    AuthenticationConfig
		claims map[string]any
	}{
		"no principal_id":         {testAuthConfig(), without(providerClaims(testProvider), "principal_id")},
		"principal_id not a UUID": {testAuthConfig(), with(providerClaims(testProvider), "principal_id", "kc-user")},
		"no subject_type":         {testAuthConfig(), without(providerClaims(testProvider), "subject_type")},
		"subject_type unknown":    {testAuthConfig(), with(providerClaims(testProvider), "subject_type", "agent")},
		"tenant_id nil":           {testAuthConfig(), tenantClaims(mustID(t), id.UUID{})},
		"tenant_id not a UUID":    {testAuthConfig(), with(providerClaims(testProvider), "tenant_id", "acme")},
		"tenant_id empty":         {testAuthConfig(), with(providerClaims(testProvider), "tenant_id", "")},

		// A provider token is a person's, without a Tenant, with the assurance claims and a grant.
		"provider without a grant": {testAuthConfig(), providerClaims(stranger)},
		"provider without acr":     {testAuthConfig(), without(providerClaims(testProvider), "acr")},
		"provider without auth_time": {testAuthConfig(),
			without(providerClaims(testProvider), "auth_time")},
		// The role this service used to read. It confers nothing now.
		"a realm role and no grant": {testAuthConfig(), with(providerClaims(stranger), "realm_access",
			map[string]any{"roles": []string{"provider-admin"}})},
		// A workload holding a provider grant is not a provider: a workload token never carries
		// provider authority (STD-IAM-002 §3.2), so the grant read is not even made.
		"workload with a grant": {testAuthConfig(), consumerClaims(testProvider)},

		// A consumer token is a workload's, without a Tenant, with its owner and a registration.
		"consumer not registered":   {testAuthConfig(), consumerClaims(stranger)},
		"consumer without an owner": {testAuthConfig(), without(consumerClaims(testConsumerWorkload), "workload_owner")},
		"consumer authority off": {AuthenticationConfig{Records: testRecords()},
			consumerClaims(testConsumerWorkload)},
		// A person registered as a consumer is not one.
		"person registered as a consumer": {testAuthConfig(), providerClaims(testConsumerWorkload)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, called, recorder := authenticated(t, s, c.cfg, s.sign(t, c.claims))
			if called {
				t.Fatal("the token was admitted")
			}
			if recorder.Code != http.StatusForbidden {
				t.Errorf("answered %d, want 403: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// A record read that fails admits nobody, and says the dependency is down rather than that the
// caller is not entitled.
func TestAFailedRecordReadAnswers503(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	records := testRecords()
	records.err = errors.New("connection refused")
	for name, claims := range map[string]map[string]any{
		"provider": providerClaims(testProvider),
		"consumer": consumerClaims(testConsumerWorkload),
		"tenant":   tenantClaims(testAdministrator, testTenant),
	} {
		_, called, recorder := authenticated(t, s, AuthenticationConfig{Records: records, Consumers: true},
			s.sign(t, claims))
		if called || recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: answered %d, called=%t, want 503 and no handler", name, recorder.Code, called)
		}
		if strings.Contains(recorder.Body.String(), "connection refused") {
			t.Errorf("%s: the refusal disclosed the database error", name)
		}
	}
}

func TestAuthenticateRefusesAMissingOrMalformedHeader(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	valid := s.sign(t, tenantClaims(mustID(t), mustID(t)))

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
// The verifier applies Requirement and the middleware applies presentedFromClaims. They are the same
// function here, and this asserts the equivalence rather than trusting it to stay true.
func TestRequirementAndMapperAgree(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	requirement := Requirement()

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
		tenantClaims(mustID(t), mustID(t)),
		providerClaims(mustID(t)),
		consumerClaims(mustID(t)),
		without(providerClaims(mustID(t)), "principal_id"),
		without(providerClaims(mustID(t)), "acr"),
		without(consumerClaims(mustID(t)), "workload_owner"),
		with(providerClaims(mustID(t)), "tenant_id", "not-a-uuid"),
		without(tenantClaims(mustID(t), mustID(t)), "auth_time"),
		with(tenantClaims(mustID(t), mustID(t)), "acr", "aal1"),
		with(tenantClaims(mustID(t), mustID(t)), "subject_type", "workload"),
	}

	for index, set := range sets {
		claims, err := permissive.Verify(s.sign(t, set))
		if err != nil {
			t.Fatalf("set %d did not verify: %v", index, err)
		}

		_, mapErr := presentedFromClaims(claims)
		requireErr := requirement.Require(claims)

		if (mapErr == nil) != (requireErr == nil) {
			t.Errorf("set %d: the mapper says %v and the requirement says %v", index, mapErr, requireErr)
		}
	}
}

func TestAuthenticateRefusesToBuildWithoutItsRecords(t *testing.T) {
	t.Parallel()

	s := newSigner(t)
	if _, err := Authenticate(s.verifier(t), AuthenticationConfig{}); err == nil {
		t.Error("Authenticate built a middleware with no caller records")
	}
	if _, err := Authenticate(nil, testAuthConfig()); err == nil {
		t.Error("Authenticate built a middleware with no verifier")
	}
}

// ORGANIZATION_TOKEN_TYPE=report accepts a token typed JWT and logs it with its client; one typed
// at+jwt is accepted and not logged.
func TestReportModeLogsATokenNotTypedAtJWT(t *testing.T) {
	s := newSigner(t)
	var logged bytes.Buffer
	verifier := ReportTokenType(s.verifier(t), slog.New(slog.NewJSONHandler(&logged, nil)))
	claims := with(tenantClaims(mustID(t), mustID(t)), "azp", "legacy-client")

	if _, err := verifier.Verify(s.signTyped(t, "JWT", claims)); err != nil {
		t.Fatalf("report mode refused a JWT-typed token: %v", err)
	}
	if !strings.Contains(logged.String(), "not typed at+jwt") || !strings.Contains(logged.String(), `"client":"legacy-client"`) {
		t.Errorf("logged %q, want the client named", logged.String())
	}
	logged.Reset()
	if _, err := verifier.Verify(s.signTyped(t, "at+jwt", claims)); err != nil || logged.Len() != 0 {
		t.Errorf("an at+jwt token answered %v and logged %q", err, logged.String())
	}
}

// ORGANIZATION_TOKEN_TYPE=enforce refuses a token typed JWT with the 401 every verification failure
// gets, and accepts one typed at+jwt.
func TestEnforceModeRefusesATokenNotTypedAtJWT(t *testing.T) {
	s := newSigner(t)
	strict, err := verify.New(verify.Config{Issuer: testIssuer, Audience: testAudience,
		Keys: verify.StaticKeys{testKeyID: &s.key.PublicKey}, Requirement: Requirement(),
		RequireAccessTokenType: true})
	if err != nil {
		t.Fatal(err)
	}
	claims := tenantClaims(mustID(t), mustID(t))
	if _, err := strict.Verify(s.signTyped(t, "JWT", claims)); !errors.Is(err, verify.ErrTokenType) {
		t.Errorf("a JWT-typed token answered %v, want ErrTokenType", err)
	}
	if _, err := strict.Verify(s.signTyped(t, "at+jwt", claims)); err != nil {
		t.Errorf("an at+jwt token was refused: %v", err)
	}
}

// authenticatedAt is authenticated at a chosen path, for the routes an eligible caller may reach.
func authenticatedAt(t *testing.T, s signer, cfg AuthenticationConfig, token, path string) (Caller, bool, *httptest.ResponseRecorder) {
	t.Helper()
	middleware, err := Authenticate(s.verifier(t), cfg)
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
	request := httptest.NewRequest(http.MethodPost, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return seen, called, recorder
}

// An eligible grant holder reaches the activation routes and nothing else (ADR-ORG-002 §5.1).
func TestAnEligibleHolderReachesOnlyTheActivationRoutes(t *testing.T) {
	s := newSigner(t)
	holder := id.MustParse("01a0f64a-c533-7000-a956-c3f095484a10")
	records := testRecords()
	records.eligible = map[id.UUID]bool{holder: true}
	cfg := AuthenticationConfig{Records: records, Consumers: true}
	token := s.sign(t, providerClaims(holder))

	for _, path := range []string{"/v1/provider-activations", "/v1/provider-activations/grants",
		"/v1/provider-activations/" + holder.String() + "/approve"} {
		caller, called, recorder := authenticatedAt(t, s, cfg, token, path)
		if !called || !caller.Eligible || caller.Provider {
			t.Errorf("%s: an eligible holder resolved to %+v (called %t, status %d)", path, caller, called, recorder.Code)
		}
	}
	for _, path := range []string{"/v1/provider-grants", "/v1/tenants", "/v1/provider-activationsx", "/v1/memberships"} {
		if _, called, recorder := authenticatedAt(t, s, cfg, token, path); called || recorder.Code != http.StatusForbidden {
			t.Errorf("%s: an eligible holder reached the handler (called %t, status %d), want 403", path, called, recorder.Code)
		}
	}
}

// Every request an emergency grant authorizes is reported (ADR-ORG-002 §5.2).
func TestAnEmergencyGrantsEveryRequestIsReported(t *testing.T) {
	s := newSigner(t)
	breakGlass := id.MustParse("01a0f64a-c533-7000-a956-c3f095484a11")
	records := testRecords()
	records.emergency = map[id.UUID]bool{breakGlass: true}
	var logs bytes.Buffer
	cfg := AuthenticationConfig{Records: records, Consumers: true,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))}

	caller, called, _ := authenticated(t, s, cfg, s.sign(t, providerClaims(breakGlass)))
	if !called || !caller.Provider || !caller.Emergency {
		t.Fatalf("an emergency grant resolved to %+v", caller)
	}
	for _, want := range []string{"a provider acted on an emergency grant", "principal_id=" + breakGlass.String(), "route=/v1/memberships"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the report lacks %q: %s", want, logs.String())
		}
	}

	logs.Reset()
	if _, _, _ = authenticated(t, s, cfg, s.sign(t, providerClaims(testProvider))); strings.Contains(logs.String(), "emergency") {
		t.Errorf("an activated provider was reported as emergency: %s", logs.String())
	}
}

// fakeUses records the Principals whose emergency grant authorized a request.
type fakeUses struct {
	used []id.UUID
	err  error
}

func (f *fakeUses) RecordEmergencyUse(_ context.Context, principal id.UUID) error {
	f.used = append(f.used, principal)
	return f.err
}

// Each request an emergency grant authorizes is its validation, recorded (ADR-ORG-002 §5.2); a
// failure to record it is reported and does not refuse the break-glass path.
func TestAnEmergencyGrantsUseIsRecordedAndNeverRefused(t *testing.T) {
	s := newSigner(t)
	breakGlass := id.MustParse("01a0f64a-c533-7000-a956-c3f095484a12")
	records := testRecords()
	records.emergency = map[id.UUID]bool{breakGlass: true}
	uses := &fakeUses{}
	var logs bytes.Buffer
	cfg := AuthenticationConfig{Records: records, Consumers: true, EmergencyUses: uses,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))}

	if _, called, _ := authenticated(t, s, cfg, s.sign(t, providerClaims(breakGlass))); !called {
		t.Fatal("the emergency grant's request did not reach the handler")
	}
	if len(uses.used) != 1 || uses.used[0] != breakGlass {
		t.Errorf("recorded uses %v, want one by %s", uses.used, breakGlass)
	}
	if _, _, _ = authenticated(t, s, cfg, s.sign(t, providerClaims(testProvider))); len(uses.used) != 1 {
		t.Errorf("an activated provider's request was recorded as an emergency use: %v", uses.used)
	}

	uses.err = errors.New("the table is unreachable")
	if _, called, _ := authenticated(t, s, cfg, s.sign(t, providerClaims(breakGlass))); !called {
		t.Error("a failure to record the use refused the emergency grant's request")
	}
	if !strings.Contains(logs.String(), "an emergency grant's use could not be recorded") {
		t.Errorf("the failure was not reported: %s", logs.String())
	}
}

func authenticatedWith(t *testing.T, s signer, cfg AuthenticationConfig, token, method, path string) (Caller, bool, *httptest.ResponseRecorder) {
	t.Helper()
	middleware, err := Authenticate(s.verifier(t), cfg)
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
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return seen, called, recorder
}

// A person reads their own contexts with any human token, and with it reaches nothing else
// (ADR-ORG-005 §5.1, TDD-organization-control-001 1.17.0 §Caller Authority).
func TestASelfCallerReachesOnlyItsOwnContexts(t *testing.T) {
	s := newSigner(t)
	nobody := id.MustParse("01a0f64a-c533-7000-a956-c3f095484a20")
	holder := id.MustParse("01a0f64a-c533-7000-a956-c3f095484a21")
	records := testRecords()
	records.eligible = map[id.UUID]bool{holder: true}
	cfg := AuthenticationConfig{Records: records, Consumers: true}
	own := func(principal id.UUID) string { return "/v1/principals/" + principal.String() + "/contexts" }

	for name, tc := range map[string]struct {
		principal id.UUID
		claims    map[string]any
	}{
		"a person with no Tenant and no grant": {nobody, providerClaims(nobody)},
		"a person whose token names a Tenant":  {nobody, tenantClaims(nobody, testTenant)},
		"a Tenant administrator":               {testAdministrator, tenantClaims(testAdministrator, testTenant)},
		"a provider":                           {testProvider, providerClaims(testProvider)},
		"an eligible provider":                 {holder, providerClaims(holder)},
	} {
		t.Run(name, func(t *testing.T) {
			before := records.reads
			caller, called, recorder := authenticatedWith(t, s, cfg, s.sign(t, tc.claims), http.MethodGet, own(tc.principal))
			if !called || !caller.Self || caller.Subject != tc.principal || caller.Provider || caller.Eligible ||
				!caller.Tenant.IsNil() || caller.Consumer != "" {
				t.Fatalf("resolved to %+v (called %t, status %d), want a self caller for %s",
					caller, called, recorder.Code, tc.principal)
			}
			if records.reads != before {
				t.Errorf("a self caller read %d records, want none", records.reads-before)
			}
		})
	}

	person := s.sign(t, providerClaims(nobody))
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, own(testProvider)},
		{http.MethodPost, own(nobody)},
		{http.MethodGet, own(nobody) + "/"},
		{http.MethodGet, "/v1/principals/" + nobody.String()},
		{http.MethodGet, "/v1/principals/" + nobody.String() + "/x/contexts"},
		{http.MethodGet, "/v1/tenants"},
	} {
		if _, called, recorder := authenticatedWith(t, s, cfg, person, tc.method, tc.path); called ||
			recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s: a person with no Tenant and no grant reached the handler (called %t, status %d), want 403",
				tc.method, tc.path, called, recorder.Code)
		}
	}

	caller, called, _ := authenticatedWith(t, s, cfg, s.sign(t, providerClaims(testProvider)), http.MethodGet, own(nobody))
	if !called || caller.Self || !caller.Provider {
		t.Errorf("a provider reading another person's contexts resolved to %+v, want a provider", caller)
	}
	caller, called, _ = authenticatedWith(t, s, cfg, s.sign(t, consumerClaims(testConsumerWorkload)), http.MethodGet,
		own(testConsumerWorkload))
	if !called || caller.Self || caller.Consumer == "" {
		t.Errorf("a workload on its own principal_id resolved to %+v, want the consumer it is", caller)
	}
}
