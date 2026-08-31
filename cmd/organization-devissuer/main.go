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
//	Terminal 1:  make issuer
//	Terminal 2:  make run
//	Terminal 3:  make token  &&  make api P=/v1/tenants/<id>
//
// The service's environment lives in .env, loaded by the Makefile. This tool no longer prints an
// environment block: printing one meant it had to be typed or pasted, in whichever shell syntax the
// block happened to be written in.
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
	"net"
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

	// Bind before printing anything. A second copy of this tool fails here, and printing the
	// instructions first made that failure read as the service refusing something.
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n\nThe issuer is most likely already running, so there is "+
			"nothing to start -- just fetch a token from http://%s/token?role=provider", listen, err, listen)
	}

	banner(kid)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.Serve(listener); err != nil {
		log.Fatalf("serve: %v", err)
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

// banner says what is running and where the instructions are. It used to print the whole
// walkthrough: eight lines of environment and forty of PowerShell, on every start.
//
// That was wrong twice over. The environment belongs in .env, where `make run` reads it and
// nobody types it; and the PowerShell was unusable in cmder, which is cmd.exe, so the reader
// got "The filename, directory name, or volume label syntax is incorrect" -- a message about
// paths for what was a shell mismatch, which sends them to look at the DSN.
func banner(kid string) {
	out := os.Stdout
	line := strings.Repeat("=", 78)

	fmt.Fprintf(out, "%s\ndev issuer on http://%s   alg=%s   kid=%s   tokens live %s\n%s\n",
		line, listen, algorithm, kid, ttl, line)

	fmt.Fprint(out, "Leave this window open. In another terminal, from the repository root:\n\n")
	fmt.Fprint(out, "    make run                                the service on 127.0.0.1:8099\n")
	fmt.Fprint(out, "    make token                              save a provider token\n")
	fmt.Fprint(out, "    make api P=/v1/tenants/<id>             call it\n")
	fmt.Fprint(out, "    make api M=POST P=/v1/organizations B=body.json\n\n")
	fmt.Fprintf(out, "Tokens directly: http://%s/token?role=provider  (also tenant, both)\n", listen)
	fmt.Fprint(out, "The walkthrough -- provisioning, idempotent retries, and the refusals worth\n")
	fmt.Fprint(out, "seeing for yourself -- is README.md, \"Driving the service by hand\".\n")
	fmt.Fprintf(out, "%s\n", line)
}
