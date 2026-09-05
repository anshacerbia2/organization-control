package httpapi

// The one authentication path `authentication_test.go` does not cover: key material fetched from a
// JWKS endpoint over HTTP.
//
// That file signs real tokens and verifies them through a real `verify.Verifier`, which is the
// important half — but it supplies the key with `verify.StaticKeys`, so `verify.NewJWKS` and the
// document it parses are never exercised here. Production uses nothing else: `main.go` builds the
// key source from `ORGANIZATION_JWKS_URL` and every request depends on the fetch succeeding and the
// document being shaped the way the parser expects.
//
// The gap is not hypothetical. `cmd/organization-devissuer` published a JWKS this repository wrote
// and signed tokens against it, and every route answered 401 — because the tool signed RS256 while
// the verifier permits only PS256. A uniform 401 reads as a permissions problem, so the wrong thing
// gets investigated. These tests fail loudly for the same class of mistake.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/verify"
)

const (
	// publishedAlgorithm is the only algorithm `verify` permits. Named here so a test that signs
	// with something else is visibly doing so on purpose.
	publishedAlgorithm = "PS256"

	// jwksKeyBits is `verify`'s minimum modulus size, not a preference. See newJWKSIssuer.
	jwksKeyBits = 3072
)

// jwksIssuer is a key set plus an endpoint publishing it in the shape `verify.NewJWKS` parses.
type jwksIssuer struct {
	key      *rsa.PrivateKey
	kid      string
	endpoint string
	fetches  int
}

func newJWKSIssuer(t *testing.T) *jwksIssuer {
	t.Helper()

	// 3072 bits, and not one bit fewer: `verify` rejects a modulus below `minRSABits`, which is
	// 3072. A 2048-bit key produces a JWKS the parser discards entirely, and the verification then
	// fails as "kid unknown and the key set could not be reloaded" — a message about key
	// distribution for what is actually a key-size policy. This is the operational fact worth
	// carrying into the Keycloak realm configuration: a realm signing with 2048-bit keys will have
	// every token refused by this service, and the refusal will not say why.
	key, err := rsa.GenerateKey(rand.Reader, jwksKeyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sum := sha256.Sum256(key.PublicKey.N.Bytes())
	issuer := &jwksIssuer{key: key, kid: base64.RawURLEncoding.EncodeToString(sum[:8])}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /certs", func(w http.ResponseWriter, _ *http.Request) {
		issuer.fetches++
		w.Header().Set("Content-Type", "application/json")
		// The modulus and exponent are base64url without padding, which is what RFC 7517 requires
		// and what the parser assumes. Standard base64 here would produce a key that parses into
		// the wrong number and a signature failure with no clue as to why.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "use": "sig", "alg": publishedAlgorithm, "kid": issuer.kid,
				"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
			}},
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	issuer.endpoint = server.URL + "/certs"
	return issuer
}

// verifier builds the production key source: an HTTP JWKS client, not a static map.
func (j *jwksIssuer) verifier(t *testing.T) *verify.Verifier {
	t.Helper()

	keys, err := verify.NewJWKS(verify.JWKSConfig{URL: j.endpoint})
	if err != nil {
		t.Fatalf("NewJWKS: %v", err)
	}
	verifier, err := verify.New(verify.Config{
		Issuer:      testIssuer,
		Audience:    testAudience,
		Keys:        keys,
		Requirement: Requirement(testAuthConfig()),
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	return verifier
}

// claims returns a provider token's claims, using the same claim and role names the rest of this
// package's tests use.
func (j *jwksIssuer) claims(t *testing.T) map[string]any {
	t.Helper()

	subject, err := id.NewV7()
	if err != nil {
		t.Fatalf("mint subject: %v", err)
	}
	now := time.Now().UTC()
	return map[string]any{
		"iss":          testIssuer,
		"aud":          []string{testAudience},
		"sub":          subject.String(),
		"iat":          now.Unix(),
		"nbf":          now.Unix(),
		"exp":          now.Add(10 * time.Minute).Unix(),
		"realm_access": map[string]any{"roles": []string{testRole}},
	}
}

// sign mints a token with the permitted algorithm.
func (j *jwksIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	return j.signWith(t, publishedAlgorithm, claims)
}

// signWith mints a token with whichever algorithm is named, so the refusal can be asserted.
//
// PS256 uses `SignPSS` at `PSSSaltLengthEqualsHash`, which RFC 7518 requires and the verifier pins;
// `PSSSaltLengthAuto` produces a signature it rejects. RS256 uses PKCS#1 v1.5. Both take an RSA key
// of the same size and produce a token of the same shape, which is what makes confusing them easy.
func (j *jwksIssuer) signWith(t *testing.T, algorithm string, claims map[string]any) string {
	t.Helper()

	header, err := json.Marshal(map[string]any{"alg": algorithm, "typ": "JWT", "kid": j.kid})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))

	var signature []byte
	switch algorithm {
	case "PS256":
		signature, err = rsa.SignPSS(rand.Reader, j.key, crypto.SHA256, digest[:], &rsa.PSSOptions{
			SaltLength: rsa.PSSSaltLengthEqualsHash,
			Hash:       crypto.SHA256,
		})
	case "RS256":
		signature, err = rsa.SignPKCS1v15(rand.Reader, j.key, crypto.SHA256, digest[:])
	default:
		t.Fatalf("the fixture cannot sign %q", algorithm)
	}
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// TestATokenVerifiesAgainstKeysFetchedOverHTTP is the production key path.
//
// `verify.NewJWKS` deliberately touches no network when constructed, so the first verification is
// also the first fetch. That ordering is asserted rather than assumed: a key source that fetched
// eagerly would make a JWKS outage a startup failure instead of a request failure, and the
// composition root chose the second.
func TestATokenVerifiesAgainstKeysFetchedOverHTTP(t *testing.T) {
	t.Parallel()

	issuer := newJWKSIssuer(t)
	verifier := issuer.verifier(t)

	if issuer.fetches != 0 {
		t.Errorf("the key set was fetched %d times before the first verification", issuer.fetches)
	}

	verified, err := verifier.Verify(issuer.sign(t, issuer.claims(t)))
	if err != nil {
		t.Fatalf("a token signed against the published key set was refused: %v", err)
	}
	if issuer.fetches == 0 {
		t.Error("the verification succeeded without ever fetching the key set")
	}

	caller, err := callerFromClaims(verified, testAuthConfig())
	if err != nil {
		t.Fatalf("the verified claims map to no caller: %v", err)
	}
	if !caller.Provider {
		t.Error("the realm role did not confer provider authority")
	}
}

// TestARequestCarryingAFetchedKeyTokenReachesTheHandler puts the whole middleware behind it.
//
// The verifier alone proves the token is good; this proves `Authenticate` establishes the caller
// from it, which is the pair a route depends on.
func TestARequestCarryingAFetchedKeyTokenReachesTheHandler(t *testing.T) {
	t.Parallel()

	issuer := newJWKSIssuer(t)
	middleware, err := Authenticate(issuer.verifier(t), testAuthConfig())
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	var seen Caller
	reached := false
	handler := middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		caller, ok := CallerFrom(r.Context())
		seen, reached = caller, ok
	}))

	request := httptest.NewRequest(http.MethodPost, "/v1/organizations", nil)
	request.Header.Set("Authorization", "Bearer "+issuer.sign(t, issuer.claims(t)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the request answered %d, want the handler to have run:\n%s",
			recorder.Code, recorder.Body.String())
	}
	if !reached || !seen.Provider {
		t.Errorf("the handler saw caller %+v, reached=%t", seen, reached)
	}
}

// TestATokenSignedWithTheWrongAlgorithmIsRefused pins the mistake that cost this session an hour.
//
// The verifier compares the header's `alg` against its own configuration rather than using it to
// select a verification path, which is the difference between an allowlist and algorithm confusion.
// The refusal is correct; this asserts it stays correct, and names the failure so the next person
// signing a token reads "algorithm" rather than "permissions".
func TestATokenSignedWithTheWrongAlgorithmIsRefused(t *testing.T) {
	t.Parallel()

	issuer := newJWKSIssuer(t)

	// The same key, the same claims, the same key identifier, a valid PKCS#1 v1.5 signature.
	token := issuer.signWith(t, "RS256", issuer.claims(t))

	if _, err := issuer.verifier(t).Verify(token); err == nil {
		t.Fatal("the verifier accepted an RS256 token while permitting only PS256")
	}
}

// TestAnUnreachableKeySourceRefusesRatherThanAdmits is the failure direction that matters.
//
// A key-distribution problem must not become an authentication bypass. The endpoint is closed before
// the first fetch, so there is no key to find and no cached one to fall back on.
func TestAnUnreachableKeySourceRefusesRatherThanAdmits(t *testing.T) {
	t.Parallel()

	issuer := newJWKSIssuer(t)
	token := issuer.sign(t, issuer.claims(t))

	keys, err := verify.NewJWKS(verify.JWKSConfig{URL: "http://127.0.0.1:1/certs"})
	if err != nil {
		t.Fatalf("NewJWKS: %v", err)
	}
	verifier, err := verify.New(verify.Config{
		Issuer:      testIssuer,
		Audience:    testAudience,
		Keys:        keys,
		Requirement: Requirement(testAuthConfig()),
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}

	if _, err := verifier.Verify(token); err == nil {
		t.Fatal("a verifier that could not reach any key source accepted a token")
	}
}
