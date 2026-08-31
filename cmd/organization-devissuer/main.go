//go:build devissuer

// Command organization-devissuer is a local token issuer for driving this service by hand.
//
// # Why it exists
//
// Every authenticated route needs an RS256 token from the issuer named in
// `ORGANIZATION_JWKS_URL`, verified against that issuer's published keys. Without one, the only
// things reachable by hand are the two probes and the anonymous invitation lookup — so the whole
// provider surface could only ever be exercised from a test that injected a `Caller` and skipped the
// verifier. That left the real authentication path unexercised outside foundation-platform's own
// suite, and left no way for a person to see the service work.
//
// This mints keys and tokens the real verifier accepts, so the service under test runs completely
// unmodified: same binary, same middleware chain, same verifier.
//
// # Why a build tag
//
// `//go:build devissuer` keeps it out of `go build ./...`, out of CI, and out of any image. It is not
// a flag that can be left on and not a check that can be misconfigured — the code is absent from the
// standard build, so there is nothing to deploy by accident. Running it takes a deliberate
// `-tags devissuer`.
//
// It signs tokens for whoever asks, which is exactly what makes it useful and exactly why it must
// never run anywhere real. It binds to loopback only.
//
// # Use
//
//	Terminal 1:  go run -tags devissuer ./cmd/organization-devissuer
//	Terminal 2:  (copy the environment block it prints, then start the service)
//	Terminal 3:  (copy the curl commands it prints)
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
)

const (
	listen = "127.0.0.1:8098"

	// issuer is what the service compares for exact equality, so the two must agree to the
	// character. Printed below rather than documented, because a trailing slash here rejects every
	// token and the failure reads as a permissions bug.
	issuer = "http://127.0.0.1:8098"

	audience = "organization-control"

	// tenantClaim and providerRole must match ORGANIZATION_TENANT_CLAIM and
	// ORGANIZATION_PROVIDER_ROLE. They are properties of the realm a deployment points at, which is
	// why the service takes them as configuration rather than as constants.
	tenantClaim  = "https://scnehaux.com/tenant"
	providerRole = "provider-admin"

	// ttl is short on purpose. A token that outlives the session that minted it is a token somebody
	// pastes into a note.
	ttl = 30 * time.Minute

	// keyBits is foundation-platform's floor, not a preference.
	//
	// `verify` rejects any modulus below 3072 bits, and it does so while *parsing the key set*: a
	// 2048-bit key is silently discarded, the set then carries no usable key, and every
	// verification fails as "kid unknown and the key set could not be reloaded". That message is
	// about key distribution and the cause is key size, which is why the first version of this tool
	// looked like a permissions problem for an hour.
	//
	// The same applies to the real realm: a Keycloak realm signing with 2048-bit keys will have
	// every token refused by this service, with no message saying why.
	keyBits = 3072
)

func main() {
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}
	kid := thumbprint(&key.PublicKey)

	mux := http.NewServeMux()

	// The JWKS document, in the shape verify.NewJWKS parses: RSA public keys by identifier, with
	// the modulus and exponent base64url-encoded without padding.
	mux.HandleFunc("GET /certs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"use": "sig",
				"alg": algorithm,
				"kid": kid,
				"n":   raw(key.PublicKey.N.Bytes()),
				"e":   raw(big.NewInt(int64(key.PublicKey.E)).Bytes()),
			}},
		})
	})

	// GET /token?role=provider
	// GET /token?role=tenant&tenant_id=<uuid>
	//
	// The subject is minted per token unless supplied, because it becomes `db.Scope.Actor` and lands
	// in every privileged-access record and every event. A fixed subject would make two sessions
	// indistinguishable in the evidence.
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		role := r.URL.Query().Get("role")
		if role == "" {
			role = "provider"
		}

		subject := strings.TrimSpace(r.URL.Query().Get("subject"))
		if subject == "" {
			minted, err := id.NewV7()
			if err != nil {
				http.Error(w, "mint subject: "+err.Error(), http.StatusInternalServerError)
				return
			}
			subject = minted.String()
		}

		now := time.Now().UTC()
		claims := map[string]any{
			"iss": issuer,
			"aud": []string{audience},
			"sub": subject,
			"iat": now.Unix(),
			"nbf": now.Unix(),
			"exp": now.Add(ttl).Unix(),
		}

		switch role {
		case "provider":
			claims["realm_access"] = map[string]any{"roles": []string{providerRole}}
		case "tenant":
			tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
			if tenantID == "" {
				http.Error(w, "role=tenant needs a tenant_id", http.StatusBadRequest)
				return
			}
			if _, err := id.Parse(tenantID); err != nil {
				http.Error(w, "tenant_id is not a UUID", http.StatusBadRequest)
				return
			}
			claims[tenantClaim] = tenantID
		case "both":
			// Deliberately offered. A token carrying provider authority *and* a Tenant must be
			// refused rather than resolved to either, and the only way to see that refusal by hand
			// is to be able to mint one.
			claims["realm_access"] = map[string]any{"roles": []string{providerRole}}
			claims[tenantClaim] = "11111111-1111-4111-8111-11111111111a"
		default:
			http.Error(w, "role must be provider, tenant, or both", http.StatusBadRequest)
			return
		}

		token, err := sign(key, kid, claims)
		if err != nil {
			http.Error(w, "sign: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintln(w, token)
	})

	banner(kid)

	server := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// algorithm is PS256, and the choice is foundation-platform's rather than this tool's.
//
// `verify` permits exactly one algorithm and verifies with `rsa.VerifyPSS` at
// `PSSSaltLengthEqualsHash`, which RFC 7518 requires for PS256. Signing with RS256 here produced a
// well-formed token that every route answered 401 to — the failure this tool exists to make
// visible, and one that reads as a permissions problem rather than as a signature one.
const algorithm = "PS256"

// sign produces a compact PS256 JWT.
func sign(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]any{"alg": algorithm, "typ": "JWT", "kid": kid})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	signingInput := raw(header) + "." + raw(body)
	digest := sha256.Sum256([]byte(signingInput))

	// The salt length must equal the hash length, not PSSSaltLengthAuto. The verifier pins it, and
	// a signature with any other salt length is rejected.
	signature, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, digest[:], &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA256,
	})
	if err != nil {
		return "", err
	}
	return signingInput + "." + raw(signature), nil
}

// raw is base64url without padding, which is what JOSE uses everywhere.
func raw(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

// thumbprint gives the key a stable identifier derived from the key itself, so the `kid` in a token
// and the `kid` in the JWKS cannot drift apart.
func thumbprint(pub *rsa.PublicKey) string {
	sum := sha256.Sum256(pub.N.Bytes())
	return raw(sum[:8])
}

func banner(kid string) {
	out := os.Stdout
	line := strings.Repeat("=", 78)

	fmt.Fprintf(out, "%s\ndev issuer on http://%s   alg=%s   kid=%s   tokens live %s\n%s\n\n",
		line, listen, algorithm, kid, ttl, line)

	fmt.Fprint(out, "1. Start the service in another terminal with this environment:\n\n")
	fmt.Fprintf(out, "$env:ORGANIZATION_TENANT_DATABASE_URL   = 'postgres://organization_app:Scnehaux@localhost:5432/organization_control_dev?sslmode=disable'\n")
	fmt.Fprintf(out, "$env:ORGANIZATION_PROVIDER_DATABASE_URL = 'postgres://organization_provider_app:Scnehaux@localhost:5432/organization_control_dev?sslmode=disable'\n")
	fmt.Fprintf(out, "$env:ORGANIZATION_TOKEN_ISSUER   = '%s'\n", issuer)
	fmt.Fprintf(out, "$env:ORGANIZATION_TOKEN_AUDIENCE = '%s'\n", audience)
	fmt.Fprintf(out, "$env:ORGANIZATION_JWKS_URL       = '%s/certs'\n", issuer)
	fmt.Fprintf(out, "$env:ORGANIZATION_TENANT_CLAIM   = '%s'\n", tenantClaim)
	fmt.Fprintf(out, "$env:ORGANIZATION_PROVIDER_ROLE  = '%s'\n", providerRole)
	fmt.Fprintf(out, "$env:ORGANIZATION_LISTEN_ADDRESS = '127.0.0.1:8099'\n")
	fmt.Fprintf(out, "go run ./cmd/organization-control\n\n")

	fmt.Fprint(out, "2. Then in a third terminal, get a provider token and use it:\n\n")
	fmt.Fprintf(out, "$t = (Invoke-WebRequest 'http://%s/token?role=provider' -UseBasicParsing).Content.Trim()\n", listen)
	fmt.Fprint(out, "$h = @{ Authorization = \"Bearer $t\"; 'X-Administrative-Reason' = 'driving the service by hand' }\n\n")

	fmt.Fprint(out, "   # create an Organization to sponsor the Tenant\n")
	fmt.Fprint(out, "   $org = Invoke-RestMethod -Uri 'http://127.0.0.1:8099/v1/organizations' -Method POST -Headers $h `\n")
	fmt.Fprint(out, "     -ContentType 'application/json' -Body '{\"display_name\":\"Acme\",\"classification\":\"customer\"}'\n")
	fmt.Fprint(out, "   $org\n\n")

	fmt.Fprint(out, "   # request a Tenant under it -- this is the path that did not exist before\n")
	fmt.Fprint(out, "   $body = @{ organization_id = $org.organization_id; display_name = 'Acme Production'\n")
	fmt.Fprint(out, "              isolation_profile = 'silo'; residency_region = 'ap-southeast-3' } | ConvertTo-Json\n")
	fmt.Fprint(out, "   $t1 = Invoke-RestMethod -Uri 'http://127.0.0.1:8099/v1/tenants' -Method POST -Headers $h `\n")
	fmt.Fprint(out, "     -ContentType 'application/json' -Body $body\n")
	fmt.Fprint(out, "   $t1\n\n")

	fmt.Fprint(out, "   # it is 'requested', not 'active'. Activating now must be refused:\n")
	fmt.Fprint(out, "   Invoke-WebRequest -Uri \"http://127.0.0.1:8099/v1/tenants/$($t1.tenant.tenant_id)/activate\" `\n")
	fmt.Fprint(out, "     -Method POST -Headers $h -ContentType 'application/json' -Body '{\"expected_version\":1}'\n\n")

	fmt.Fprint(out, "   # record the dispatch, then report the boundary built, then activate\n")
	fmt.Fprint(out, "   Invoke-RestMethod -Uri \"http://127.0.0.1:8099/v1/tenants/$($t1.tenant.tenant_id)/provisioning\" `\n")
	fmt.Fprint(out, "     -Method POST -Headers $h -ContentType 'application/json' -Body '{\"expected_version\":1}'\n")
	fmt.Fprint(out, "   $done = @{ correlation_id = $t1.correlation_id; detail = 'boundary built' } | ConvertTo-Json\n")
	fmt.Fprint(out, "   Invoke-RestMethod -Uri 'http://127.0.0.1:8099/v1/provisioning/realized' -Method POST -Headers $h `\n")
	fmt.Fprint(out, "     -ContentType 'application/json' -Body $done\n")
	fmt.Fprint(out, "   Invoke-RestMethod -Uri \"http://127.0.0.1:8099/v1/tenants/$($t1.tenant.tenant_id)/activate\" `\n")
	fmt.Fprint(out, "     -Method POST -Headers $h -ContentType 'application/json' -Body '{\"expected_version\":2}'\n\n")

	fmt.Fprint(out, "   # read it back\n")
	fmt.Fprint(out, "   Invoke-RestMethod -Uri \"http://127.0.0.1:8099/v1/tenants/$($t1.tenant.tenant_id)\" -Headers $h\n\n")

	fmt.Fprint(out, "3. Retry the same mutation safely -- send it twice with one Idempotency-Key:\n\n")
	fmt.Fprint(out, "   $key = [guid]::NewGuid().ToString()\n")
	fmt.Fprint(out, "   $hk = $h + @{ 'Idempotency-Key' = $key }\n")
	fmt.Fprint(out, "   $b2 = @{ organization_id = $org.organization_id; display_name = 'Retried'\n")
	fmt.Fprint(out, "            isolation_profile = 'pooled' } | ConvertTo-Json\n")
	fmt.Fprint(out, "   $a = Invoke-RestMethod -Uri 'http://127.0.0.1:8099/v1/tenants' -Method POST -Headers $hk `\n")
	fmt.Fprint(out, "     -ContentType 'application/json' -Body $b2\n")
	fmt.Fprint(out, "   $b = Invoke-RestMethod -Uri 'http://127.0.0.1:8099/v1/tenants' -Method POST -Headers $hk `\n")
	fmt.Fprint(out, "     -ContentType 'application/json' -Body $b2\n")
	fmt.Fprint(out, "   $a.tenant.tenant_id -eq $b.tenant.tenant_id   # True: one Tenant, not two\n\n")
	fmt.Fprint(out, "   The second response carries 'Idempotent-Replay: true'. The same key with a\n")
	fmt.Fprint(out, "   different body is 409 rather than a replay, and a key on a GET is 400.\n\n")

	fmt.Fprint(out, "4. Refusals worth seeing for yourself:\n\n")
	fmt.Fprintf(out, "   role=tenant on a provider route  -> 403\n")
	fmt.Fprintf(out, "     $tt = (Invoke-WebRequest 'http://%s/token?role=tenant&tenant_id=11111111-1111-4111-8111-11111111111a' -UseBasicParsing).Content.Trim()\n", listen)

	// 403, not 401, and the difference is the point: the token is authentic, so "we do not know who
	// you are" would be false. Its claims confer no scope. Asserted by
	// TestAuthenticateRefusesBothAuthorities, and this line said 401 until the by-hand walkthrough
	// disagreed with it.
	fmt.Fprintf(out, "   role=both                        -> 403: the token is authentic, and its\n")
	fmt.Fprintf(out, "                                      claims confer no scope. Cross-Tenant\n")
	fmt.Fprintf(out, "                                      authority and authority over one Tenant\n")
	fmt.Fprintf(out, "                                      are refused rather than resolved to either\n")
	fmt.Fprintf(out, "     $tb = (Invoke-WebRequest 'http://%s/token?role=both' -UseBasicParsing).Content.Trim()\n", listen)
	fmt.Fprintf(out, "   provider token, no reason header -> 400 naming the header\n")
	fmt.Fprintf(out, "   a body with an unknown field     -> 400, because DisallowUnknownFields is on\n")
	fmt.Fprintf(out, "   activating a 'requested' Tenant  -> 409: the state machine is consulted\n")
	fmt.Fprintf(out, "                                      before the preconditions, so the answer\n")
	fmt.Fprintf(out, "                                      names the transition rather than a check\n")
	fmt.Fprintf(out, "                                      that never ran. 412 is what you get once\n")
	fmt.Fprintf(out, "                                      it IS provisioning and unconfirmed\n\n")

	fmt.Fprintf(out, "%s\n", line)
}
