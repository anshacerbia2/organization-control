//go:build devissuer

package main

// The client credentials grant, driven by foundation-platform's clientauth: the client this
// service's dispatchers use, so a token these tests obtain is one the system proof obtains.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/clientauth"
)

const testClient = "organization-control-workload"

var (
	keysOnce          sync.Once
	clientKey, others *rsa.PrivateKey
)

// keys generates the client's key and an impostor's once: a 3072-bit key takes about a second.
func keys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	keysOnce.Do(func() {
		var err error
		if clientKey, err = rsa.GenerateKey(rand.Reader, keyBits); err != nil {
			t.Fatalf("generating the client key: %v", err)
		}
		if others, err = rsa.GenerateKey(rand.Reader, keyBits); err != nil {
			t.Fatalf("generating another key: %v", err)
		}
	})
	return clientKey, others
}

// grant is the endpoint for the test client, with its token signing recorded rather than performed.
func grant(t *testing.T) (*clientCredentials, *map[string]any) {
	t.Helper()
	private, _ := keys(t)
	minted := new(map[string]any)
	c := newClientCredentials(&workloadClient{id: testClient, key: &private.PublicKey, principal: devDeliveryWorkload},
		func(claims map[string]any) (string, error) {
			*minted = claims
			return "minted-token", nil
		})
	return c, minted
}

func assertion(t *testing.T, signer *rsa.PrivateKey, clientID, audience string, now time.Time) string {
	t.Helper()
	key, err := clientauth.KeyFromRSA(signer)
	if err != nil {
		t.Fatalf("wrapping the key: %v", err)
	}
	signed, err := key.Assertion(clientID, audience, now)
	if err != nil {
		t.Fatalf("signing the assertion: %v", err)
	}
	return signed
}

func post(t *testing.T, c *clientCredentials, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	c.ServeHTTP(recorder, request)
	return recorder
}

func form(assertion string) url.Values {
	return url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {testClient},
		"client_assertion_type": {clientauth.AssertionType},
		"client_assertion":      {assertion},
	}
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return body.Error
}

// clientauth obtains a token end to end, and the token is for foundation-reference, with only the
// delivery scope, naming the configured workload: what foundation-reference's acceptance API admits.
func TestClientAuthObtainsTheDeliveryToken(t *testing.T) {
	c, minted := grant(t)
	server := httptest.NewServer(c)
	defer server.Close()

	private, _ := keys(t)
	key, err := clientauth.KeyFromRSA(private)
	if err != nil {
		t.Fatalf("wrapping the key: %v", err)
	}
	tokens, err := clientauth.NewTokens(clientauth.Config{
		TokenURL: server.URL + "/token", Audience: issuer, ClientID: testClient, Key: key,
	})
	if err != nil {
		t.Fatalf("NewTokens: %v", err)
	}
	token, err := tokens.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "minted-token" {
		t.Errorf("clientauth returned %q", token)
	}

	claims := *minted
	audience, _ := claims["aud"].([]string)
	switch {
	case len(audience) != 1 || audience[0] != referenceAudience:
		t.Errorf("the token's audience is %v, want [%s]", claims["aud"], referenceAudience)
	case claims["scope"] != deliveryScope:
		t.Errorf("the token's scope is %v, want %s alone", claims["scope"], deliveryScope)
	case claims["principal_id"] != devDeliveryWorkload || claims["subject_type"] != "workload":
		t.Errorf("the token names %v (%v), want the workload %s", claims["principal_id"], claims["subject_type"],
			devDeliveryWorkload)
	case claims["client_id"] != testClient || claims["iss"] != issuer:
		t.Errorf("the token is from %v for client %v", claims["iss"], claims["client_id"])
	}
}

// Every refusal RFC 7523 §3 requires, each answered invalid_client, and the RFC 6749 ones answered
// as that RFC names them.
func TestTheGrantRefusesWhatRFC7523Refuses(t *testing.T) {
	private, impostor := keys(t)
	now := time.Now()
	for name, c := range map[string]struct {
		form   func() url.Values
		status int
		code   string
	}{
		"another grant type": {func() url.Values {
			f := form(assertion(t, private, testClient, issuer, now))
			f.Set("grant_type", "password")
			return f
		}, http.StatusBadRequest, "unsupported_grant_type"},
		"no assertion type": {func() url.Values {
			f := form(assertion(t, private, testClient, issuer, now))
			f.Del("client_assertion_type")
			return f
		}, http.StatusUnauthorized, "invalid_client"},
		"two assertions": {func() url.Values {
			f := form(assertion(t, private, testClient, issuer, now))
			f.Add("client_assertion", assertion(t, private, testClient, issuer, now))
			return f
		}, http.StatusBadRequest, "invalid_request"},
		"signed by another key": {func() url.Values {
			return form(assertion(t, impostor, testClient, issuer, now))
		}, http.StatusUnauthorized, "invalid_client"},
		"another client as issuer and subject": {func() url.Values {
			f := form(assertion(t, private, "someone-else", issuer, now))
			f.Del("client_id")
			return f
		}, http.StatusUnauthorized, "invalid_client"},
		"another client_id beside the assertion": {func() url.Values {
			f := form(assertion(t, private, testClient, issuer, now))
			f.Set("client_id", "someone-else")
			return f
		}, http.StatusUnauthorized, "invalid_client"},
		"another audience": {func() url.Values {
			return form(assertion(t, private, testClient, "http://127.0.0.1:8081/realms/scnehaux", now))
		}, http.StatusUnauthorized, "invalid_client"},
		"expired": {func() url.Values {
			return form(assertion(t, private, testClient, issuer, now.Add(-5*time.Minute)))
		}, http.StatusUnauthorized, "invalid_client"},
		"not one JWT": {func() url.Values {
			return form("a.b")
		}, http.StatusUnauthorized, "invalid_client"},
	} {
		t.Run(name, func(t *testing.T) {
			endpoint, minted := grant(t)
			recorder := post(t, endpoint, c.form())
			if recorder.Code != c.status || errorCode(t, recorder) != c.code {
				t.Errorf("answered %d %s, want %d %s", recorder.Code, recorder.Body.String(), c.status, c.code)
			}
			if *minted != nil {
				t.Error("a token was minted for a refused request")
			}
		})
	}
}

// The token URL is an audience RFC 7523 §3 item 3 allows, and an assertion is accepted once.
func TestAnAssertionIsAcceptedOnceAndMayNameTheTokenURL(t *testing.T) {
	private, _ := keys(t)
	endpoint, _ := grant(t)
	signed := assertion(t, private, testClient, issuer+"/token", time.Now())

	if first := post(t, endpoint, form(signed)); first.Code != http.StatusOK {
		t.Fatalf("the first presentation answered %d %s", first.Code, first.Body.String())
	}
	if second := post(t, endpoint, form(signed)); second.Code != http.StatusUnauthorized ||
		errorCode(t, second) != "invalid_client" {
		t.Errorf("a replayed assertion answered %d %s", second.Code, second.Body.String())
	}
}

// With no client configured every request is refused as an unknown client.
func TestAnUnconfiguredIssuerAdmitsNoClient(t *testing.T) {
	private, _ := keys(t)
	endpoint := newClientCredentials(nil, func(map[string]any) (string, error) {
		return "", errors.New("must not sign")
	})
	recorder := post(t, endpoint, form(assertion(t, private, testClient, issuer, time.Now())))
	if recorder.Code != http.StatusUnauthorized || errorCode(t, recorder) != "invalid_client" {
		t.Errorf("answered %d %s", recorder.Code, recorder.Body.String())
	}
}

// The client's public key is read from PKIX or PKCS#1 PEM, and a half-configured client or a short
// key stops the issuer at startup.
func TestTheWorkloadClientIsReadFromTheEnvironment(t *testing.T) {
	private, _ := keys(t)
	dir := t.TempDir()
	pkix, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatalf("encoding the key: %v", err)
	}
	keyFile := filepath.Join(dir, "workload.pub.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix}), 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}

	t.Setenv("ORGANIZATION_DEVISSUER_CLIENT_ID", "")
	t.Setenv("ORGANIZATION_DEVISSUER_CLIENT_PUBLIC_KEY_FILE", "")
	t.Setenv("ORGANIZATION_DEVISSUER_CLIENT_PRINCIPAL_ID", "")
	if client, err := loadWorkloadClient(); client != nil || err != nil {
		t.Errorf("nothing set read as %+v, %v", client, err)
	}

	t.Setenv("ORGANIZATION_DEVISSUER_CLIENT_ID", testClient)
	if _, err := loadWorkloadClient(); err == nil {
		t.Error("a client with no key was accepted")
	}

	t.Setenv("ORGANIZATION_DEVISSUER_CLIENT_PUBLIC_KEY_FILE", keyFile)
	client, err := loadWorkloadClient()
	if err != nil || client.id != testClient || client.principal != devDeliveryWorkload ||
		client.key.N.Cmp(private.PublicKey.N) != 0 {
		t.Errorf("the client read as %+v, %v", client, err)
	}

	t.Setenv("ORGANIZATION_DEVISSUER_CLIENT_PRINCIPAL_ID", "not-a-uuid")
	if _, err := loadWorkloadClient(); err == nil {
		t.Error("a principal_id that is not a UUID was accepted")
	}

	short, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a short key: %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&short.PublicKey)})
	if _, err := parsePublicKey(encoded); err == nil {
		t.Error("a 2048-bit key was accepted")
	}
	encoded = pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&private.PublicKey)})
	if _, err := parsePublicKey(encoded); err != nil {
		t.Errorf("a PKCS#1 key was refused: %v", err)
	}
}
