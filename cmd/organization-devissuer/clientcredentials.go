//go:build devissuer

package main

// The client credentials grant (RFC 6749 §4.4), with the client authenticated by a signed JWT
// assertion (RFC 7523 §2.2): what `clientauth` sends when this service delivers to a consumer.
//
// # Why the dev issuer serves it
//
// `GET /token` signs a token for whoever asks, which drives a route by hand and proves nothing about
// how this service obtains its own credential. The in-process dispatchers obtain theirs from the
// kernel's token endpoint with a key pair, and foundation-reference's system proof runs those
// dispatchers. Served here, the proof obtains the delivery token through the same `clientauth` code a
// deployment runs, and a key the issuer does not hold is refused here as the kernel would refuse it.
//
// # What it checks
//
// RFC 7523 §3 lists what an authorization server MUST check, and each is checked: the assertion is
// signed by the configured client's key, names the client as issuer and subject, names this issuer
// (or its token URL) as audience, and has not expired. The assertion's `jti` is required and is
// refused the second time it is presented, which §3 makes optional and `clientauth` relies on ("the
// kernel refuses one presented twice").
//
// One client, configured in the environment, because the issuer stands in for one workload's
// registration: Organization Control's. Its token is for foundation-reference alone, the only
// delivery target the issuer knows (ROADMAP item 44 records one token for every target as open).

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
)

const (
	// assertionType is RFC 7523 §2.2's client_assertion_type.
	assertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	// assertionSkew is the clock skew an assertion's exp and nbf are allowed (RFC 7523 §3 item 4).
	assertionSkew = 30 * time.Second

	// assertionHorizon is how far ahead an exp may be. RFC 7523 §3: an issuer "may reject JWTs with
	// an exp claim value that is unreasonably far in the future". clientauth signs for one minute.
	assertionHorizon = 5 * time.Minute

	// devDeliveryWorkload is the workload principal_id a client credentials token names unless
	// ORGANIZATION_DEVISSUER_CLIENT_PRINCIPAL_ID says otherwise: Organization Control's workload, the
	// one foundation-reference's REFERENCE_DELIVERY_PRINCIPAL_ID admits.
	devDeliveryWorkload = "55555555-5555-4555-8555-55555555550b"
)

// workloadClient is the one client the grant admits.
type workloadClient struct {
	id        string
	key       *rsa.PublicKey
	principal string
}

// loadWorkloadClient reads the client from the environment. Neither variable set is a client that is
// not configured, and the grant refuses every request. One without the other is a mistake, refused at
// startup rather than as a 401 that reads as a bad key.
func loadWorkloadClient() (*workloadClient, error) {
	clientID := strings.TrimSpace(os.Getenv("ORGANIZATION_DEVISSUER_CLIENT_ID"))
	keyFile := strings.TrimSpace(os.Getenv("ORGANIZATION_DEVISSUER_CLIENT_PUBLIC_KEY_FILE"))
	principal := strings.TrimSpace(os.Getenv("ORGANIZATION_DEVISSUER_CLIENT_PRINCIPAL_ID"))
	switch {
	case clientID == "" && keyFile == "":
		return nil, nil
	case clientID == "" || keyFile == "":
		return nil, errors.New("ORGANIZATION_DEVISSUER_CLIENT_ID and ORGANIZATION_DEVISSUER_CLIENT_PUBLIC_KEY_FILE " +
			"are set together or not at all")
	}
	if principal == "" {
		principal = devDeliveryWorkload
	}
	if _, err := id.Parse(principal); err != nil {
		return nil, fmt.Errorf("ORGANIZATION_DEVISSUER_CLIENT_PRINCIPAL_ID is not a UUID: %q", principal)
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading the client's public key at %s: %w", keyFile, err)
	}
	key, err := parsePublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("the client's public key at %s: %w", keyFile, err)
	}
	return &workloadClient{id: clientID, key: key, principal: principal}, nil
}

// parsePublicKey reads an RSA public key in PKIX or PKCS#1 PEM and refuses one below keyBits, the
// floor clientauth enforces on the private half.
func parsePublicKey(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	var key *rsa.PublicKey
	switch block.Type {
	case "PUBLIC KEY":
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("not a PKIX public key: %w", err)
		}
		rsaKey, ok := parsed.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		key = rsaKey
	case "RSA PUBLIC KEY":
		parsed, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("not a PKCS#1 public key: %w", err)
		}
		key = parsed
	default:
		return nil, fmt.Errorf("a %q PEM block is not a public key", block.Type)
	}
	if bits := key.N.BitLen(); bits < keyBits {
		return nil, fmt.Errorf("the key is %d bits; at least %d are required", bits, keyBits)
	}
	return key, nil
}

// clientCredentials is POST /token.
type clientCredentials struct {
	client *workloadClient // nil: no client is configured, and every request is refused

	// audiences are the values an assertion's aud may name: this issuer, and its token URL, which
	// RFC 7523 §3 item 3 allows.
	audiences []string

	sign func(claims map[string]any) (string, error)
	now  func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // jti -> the assertion's exp, after which it is forgotten
}

func newClientCredentials(client *workloadClient, sign func(map[string]any) (string, error)) *clientCredentials {
	return &clientCredentials{
		client:    client,
		audiences: []string{issuer, issuer + "/token"},
		sign:      sign,
		now:       time.Now,
		seen:      map[string]time.Time{},
	}
}

// oauthError is RFC 6749 §5.2's error response.
func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

func (c *clientCredentials) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
		return
	}
	// RFC 6749 §3.2: parameters "MUST NOT be included more than once".
	for name, values := range r.PostForm {
		if len(values) > 1 {
			oauthError(w, http.StatusBadRequest, "invalid_request", name+" is repeated")
			return
		}
	}
	if grant := r.PostForm.Get("grant_type"); grant != "client_credentials" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"this issuer serves grant_type=client_credentials on POST; for a hand-driven token use GET /token")
		return
	}
	if c.client == nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client",
			"no workload client is configured: set ORGANIZATION_DEVISSUER_CLIENT_ID and "+
				"ORGANIZATION_DEVISSUER_CLIENT_PUBLIC_KEY_FILE")
		return
	}
	if r.PostForm.Get("client_assertion_type") != assertionType {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client_assertion_type must be "+assertionType)
		return
	}
	if named := r.PostForm.Get("client_id"); named != "" && named != c.client.id {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client_id is not the configured client")
		return
	}
	if err := c.authenticate(r.PostForm.Get("client_assertion")); err != nil {
		// RFC 7523 §3.2: an invalid client JWT is invalid_client.
		oauthError(w, http.StatusUnauthorized, "invalid_client", err.Error())
		return
	}

	now := c.now().UTC()
	token, err := c.sign(map[string]any{
		"iss": issuer,
		"aud": []string{referenceAudience},
		"sub": c.client.principal,
		// RFC 9068 §2.2 names the client a token was issued to.
		"client_id": c.client.id,
		"iat":       now.Unix(),
		"nbf":       now.Unix(),
		"exp":       now.Add(ttl).Unix(),
		// Only the scope that admits a delivery: this token cannot invoke an operation.
		"scope":          deliveryScope,
		"principal_id":   c.client.principal,
		"subject_type":   "workload",
		"workload_owner": devWorkloadOwner,
	})
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "signing the access token failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int(ttl / time.Second),
		"scope":        deliveryScope,
	})
}

// authenticate checks a client assertion as RFC 7523 §3 requires, and records its jti.
func (c *clientCredentials) authenticate(assertion string) error {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		// §2.2: the parameter "MUST NOT contain more than one JWT".
		return errors.New("client_assertion is not one compact JWT")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return errors.New("the assertion's header is not JSON")
	}
	if header.Alg != algorithm {
		return fmt.Errorf("the assertion is signed with %q; %s is the one algorithm accepted", header.Alg, algorithm)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("the assertion's signature is not base64url")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	// §3 item 9: "MUST reject JWTs with an invalid signature".
	if err := rsa.VerifyPSS(c.client.key, crypto.SHA256, digest[:], signature,
		&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}); err != nil {
		return errors.New("the assertion is not signed by the configured client's key")
	}

	var claims struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Exp *int64          `json:"exp"`
		Nbf *int64          `json:"nbf"`
		Jti string          `json:"jti"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return errors.New("the assertion's claims are not JSON")
	}
	now := c.now()
	switch {
	// §3 items 1 and 2B: the client names itself, and "the subject MUST be the client_id".
	case claims.Iss != c.client.id || claims.Sub != c.client.id:
		return errors.New("the assertion's iss and sub must both be the configured client_id")
	// §3 item 3: reject a JWT "that does not contain its own identity as the intended audience".
	case !c.namesThisIssuer(claims.Aud):
		return fmt.Errorf("the assertion's aud must name %s", issuer)
	// §3 item 4: exp is required, and an expired assertion is refused.
	case claims.Exp == nil:
		return errors.New("the assertion carries no exp")
	case now.After(time.Unix(*claims.Exp, 0).Add(assertionSkew)):
		return errors.New("the assertion has expired")
	case time.Unix(*claims.Exp, 0).After(now.Add(assertionHorizon)):
		return fmt.Errorf("the assertion's exp is more than %s ahead", assertionHorizon)
	// §3 item 5.
	case claims.Nbf != nil && time.Unix(*claims.Nbf, 0).After(now.Add(assertionSkew)):
		return errors.New("the assertion is not valid yet")
	case claims.Jti == "":
		return errors.New("the assertion carries no jti, so it cannot be refused when replayed")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for jti, expiry := range c.seen {
		if now.After(expiry.Add(assertionSkew)) {
			delete(c.seen, jti)
		}
	}
	if _, replayed := c.seen[claims.Jti]; replayed {
		return errors.New("the assertion was presented before")
	}
	c.seen[claims.Jti] = time.Unix(*claims.Exp, 0)
	return nil
}

// namesThisIssuer reads aud as a string or an array of strings.
func (c *clientCredentials) namesThisIssuer(raw json.RawMessage) bool {
	var named []string
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		named = []string{one}
	} else if err := json.Unmarshal(raw, &named); err != nil {
		return false
	}
	for _, value := range named {
		for _, audience := range c.audiences {
			// RFC 7523 §3 item 3: simple string comparison.
			if value == audience {
				return true
			}
		}
	}
	return false
}

func decodeSegment(segment string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
