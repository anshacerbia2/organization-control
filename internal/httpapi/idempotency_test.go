package httpapi

// The middleware's own decisions, which are the ones it makes without a database.
//
// What the claim *does* once attached is asserted in `internal/db`, against the real engine: that it
// commits with the effect, that a rolled-back mutation releases it, that a completed key replays. The
// division is deliberate — those are properties of a transaction and cannot be shown without one,
// and these are properties of a request and are obscured by one.

import (
	stdcontext "context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// completer records what it was asked to store.
type completer struct {
	calls  int
	claim  db.Claim
	status int
	body   json.RawMessage
	fail   error
}

func (c *completer) Complete(_ stdcontext.Context, claim db.Claim, status int,
	body json.RawMessage) error {
	c.calls++
	c.claim, c.status, c.body = claim, status, body
	return c.fail
}

// through runs one request through the middleware with a caller already established, and reports
// what the handler saw.
func through(t *testing.T, store ClaimCompleter, caller Caller, request *http.Request,
	handler http.HandlerFunc) (*httptest.ResponseRecorder, db.Claim, bool) {
	t.Helper()

	middleware, err := Idempotent(store, nil)
	if err != nil {
		t.Fatalf("Idempotent: %v", err)
	}

	var (
		seen  db.Claim
		found bool
	)
	wrapped := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, found = db.ClaimFrom(r.Context())
		handler(w, r)
	}))

	recorder := httptest.NewRecorder()
	wrapped.ServeHTTP(recorder, request.WithContext(WithCaller(request.Context(), caller)))
	return recorder, seen, found
}

func ok201(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"created":true}`))
}

func providerCallerValue(t *testing.T) Caller {
	t.Helper()
	return Caller{Subject: mustID(t), Provider: true}
}

func TestIdempotentRefusesToBeBuiltWithoutAStore(t *testing.T) {
	t.Parallel()
	if _, err := Idempotent(nil, nil); err == nil {
		t.Fatal("Idempotent accepted a nil store, so completions would silently vanish")
	}
}

// TestARequestWithNoKeyPassesThroughUntouched is what keeps every existing caller working.
func TestARequestWithNoKeyPassesThroughUntouched(t *testing.T) {
	t.Parallel()

	store := &completer{}
	request := httptest.NewRequest(http.MethodPost, "/v1/tenants", strings.NewReader(`{}`))

	recorder, _, found := through(t, store, providerCallerValue(t), request, ok201)

	if found {
		t.Error("a claim was attached to a request that carried no key")
	}
	if recorder.Code != http.StatusCreated {
		t.Errorf("the handler answered %d, want it reached untouched", recorder.Code)
	}
	if store.calls != 0 {
		t.Errorf("the store was called %d times for an unkeyed request", store.calls)
	}
}

// TestAKeyOnAMutationIsAttachedAndCompleted covers the ordinary path.
//
// The completion is asserted through the claim the middleware built rather than through a database:
// what reaches the store is the middleware's decision, and what the store does with it is the
// store's.
func TestAKeyOnAMutationIsAttachedAndCompleted(t *testing.T) {
	t.Parallel()

	store := &completer{}
	caller := providerCallerValue(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/tenants", strings.NewReader(`{"a":1}`))
	request.Header.Set(IdempotencyHeader, "key-one")

	recorder, claim, found := through(t, store, caller, request, ok201)

	if !found {
		t.Fatal("no claim was attached to a keyed mutation")
	}
	if claim.Key != "key-one" {
		t.Errorf("the claim carries key %q, want key-one", claim.Key)
	}
	// The subject is in the scope, not only the authority. A key is a value the client chooses, so
	// two provider operators sharing a namespace could replay each other's responses.
	if !strings.Contains(claim.Scope, caller.Subject.String()) {
		t.Errorf("the claim scope %q does not identify the caller", claim.Scope)
	}
	if claim.Digest == "" {
		t.Error("the claim carries no digest")
	}
	if recorder.Code != http.StatusCreated {
		t.Errorf("the response was %d, want 201", recorder.Code)
	}

	// The completion does not happen here: `db.ClaimMade` is false because no scoped transaction
	// ran, and the middleware only completes a claim a transaction actually reserved. Asserted
	// because the alternative — completing unconditionally — would store a response for a key that
	// was never claimed, and `idempotency.Complete` would report `ErrNotClaimed` on every request.
	if store.calls != 0 {
		t.Errorf("the store was called %d times for a claim no transaction reserved", store.calls)
	}
}

// TestTheBodyIsRestoredForTheHandler is the part that breaks quietly.
//
// The body has to be read to be hashed. A middleware that read it and did not put it back would
// leave every keyed mutation with an empty body, and the handler would refuse it as malformed — an
// error that looks like the caller's fault.
func TestTheBodyIsRestoredForTheHandler(t *testing.T) {
	t.Parallel()

	const payload = `{"display_name":"Acme","isolation_profile":"pooled"}`

	store := &completer{}
	request := httptest.NewRequest(http.MethodPost, "/v1/tenants", strings.NewReader(payload))
	request.Header.Set(IdempotencyHeader, "key-two")

	var read string
	recorder, _, _ := through(t, store, providerCallerValue(t), request,
		func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("the handler could not read the body: %v", err)
			}
			read = string(raw)
			ok201(w, r)
		})

	if read != payload {
		t.Errorf("the handler read %q, want the original body %q", read, payload)
	}
	if recorder.Code != http.StatusCreated {
		t.Errorf("the response was %d", recorder.Code)
	}
}

// TestTheDigestSeparatesRequestsThatShareAKey stops one key answering for two intentions.
func TestTheDigestSeparatesRequestsThatShareAKey(t *testing.T) {
	t.Parallel()

	caller := providerCallerValue(t)
	digestFor := func(method, path, body string) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set(IdempotencyHeader, "shared")
		_, claim, found := through(t, &completer{}, caller, request, ok201)
		if !found {
			t.Fatal("no claim was attached")
		}
		return claim.Digest
	}

	base := digestFor(http.MethodPost, "/v1/tenants", `{"a":1}`)

	// A different body is a different request.
	if other := digestFor(http.MethodPost, "/v1/tenants", `{"a":2}`); other == base {
		t.Error("two different bodies produced the same digest")
	}
	// So is the same body on a different route. Without the path in the digest, one key reused
	// across two routes with identical bodies would replay the first route's response for the
	// second.
	if other := digestFor(http.MethodPost, "/v1/organizations", `{"a":1}`); other == base {
		t.Error("the same body on a different path produced the same digest")
	}
	// And the same body under a different method.
	if other := digestFor(http.MethodPut, "/v1/tenants", `{"a":1}`); other == base {
		t.Error("the same body under a different method produced the same digest")
	}
	// The same request twice must produce the same digest, or a legitimate retry would be refused
	// as a conflict.
	if again := digestFor(http.MethodPost, "/v1/tenants", `{"a":1}`); again != base {
		t.Error("the identical request produced a different digest, so every retry would conflict")
	}
}

// TestAKeyOnAReadIsRefused keeps a key from being spent on a request that changes nothing.
func TestAKeyOnAReadIsRefused(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			reached := false
			request := httptest.NewRequest(method, "/v1/tenants/"+mustID(t).String(), nil)
			request.Header.Set(IdempotencyHeader, "key-on-a-read")

			recorder, _, _ := through(t, &completer{}, providerCallerValue(t), request,
				func(w http.ResponseWriter, _ *http.Request) {
					reached = true
					w.WriteHeader(http.StatusOK)
				})

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("a key on %s answered %d, want 400", method, recorder.Code)
			}
			if reached {
				t.Errorf("the handler ran for a %s carrying a key", method)
			}
		})
	}
}

// TestARequestWithNoCallerIsRefused fails closed if the chain is ever reordered.
//
// A claim built with no caller in its scope is a claim any caller could present.
func TestARequestWithNoCallerIsRefused(t *testing.T) {
	t.Parallel()

	middleware, err := Idempotent(&completer{}, nil)
	if err != nil {
		t.Fatalf("Idempotent: %v", err)
	}

	reached := false
	wrapped := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusCreated)
	}))

	request := httptest.NewRequest(http.MethodPost, "/v1/tenants", strings.NewReader(`{}`))
	request.Header.Set(IdempotencyHeader, "key-three")
	recorder := httptest.NewRecorder()
	wrapped.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("a keyed request with no caller answered %d, want 400", recorder.Code)
	}
	if reached {
		t.Error("the handler ran for a keyed request with no authenticated caller")
	}
}

// TestABodyPastTheBoundIsRefusedRatherThanUnclaimed refuses instead of silently dropping the key.
//
// A caller that supplied a key and had it ignored would believe its retries were safe.
func TestABodyPastTheBoundIsRefusedRatherThanUnclaimed(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("x", maxIdempotentBody+1)
	request := httptest.NewRequest(http.MethodPost, "/v1/tenants", strings.NewReader(oversized))
	request.Header.Set(IdempotencyHeader, "key-four")

	reached := false
	recorder, _, _ := through(t, &completer{}, providerCallerValue(t), request,
		func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusCreated)
		})

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("an oversized keyed body answered %d, want 400", recorder.Code)
	}
	if reached {
		t.Error("the handler ran for a body past the bound")
	}
}

// TestAReplayIsWrittenAsTheStoredResponse covers the one place a `*db.Replayed` becomes a response.
//
// It travels out of a service as an error because the services know nothing about idempotency —
// which is what stops them being able to forget it — so `writeError` is where it stops being one.
func TestAReplayIsWrittenAsTheStoredResponse(t *testing.T) {
	t.Parallel()

	stored := json.RawMessage(`{"tenant_id":"01a0-stored","status":"requested"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/tenants", nil)

	writeError(recorder, request, &db.Replayed{Status: http.StatusCreated, Body: stored})

	if recorder.Code != http.StatusCreated {
		t.Errorf("the replay answered %d, want the stored 201", recorder.Code)
	}
	if got := recorder.Body.String(); got != string(stored) {
		t.Errorf("the replay wrote %q, want the stored body %q", got, stored)
	}
	if got := recorder.Header().Get("Idempotent-Replay"); got != "true" {
		t.Errorf("the replay header is %q, want true — a retrying client cannot otherwise tell a "+
			"replay from a fresh execution", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("the replay content type is %q", got)
	}
}

// TestAReplayWrappedInAnotherErrorIsStillARelay proves the detection is `errors.As` and not a type
// assertion.
//
// A service that annotates the error on its way out — which several do — would otherwise turn a
// replay into a 500.
func TestAReplayWrappedInAnotherErrorIsStillARelay(t *testing.T) {
	t.Parallel()

	wrapped := errorWrapper{inner: &db.Replayed{Status: http.StatusOK, Body: json.RawMessage(`{}`)}}
	recorder := httptest.NewRecorder()
	writeError(recorder, httptest.NewRequest(http.MethodPost, "/v1/tenants", nil), wrapped)

	if recorder.Code != http.StatusOK {
		t.Errorf("a wrapped replay answered %d, want the stored 200", recorder.Code)
	}
	if recorder.Header().Get("Idempotent-Replay") != "true" {
		t.Error("a wrapped replay was not marked as one")
	}
}

type errorWrapper struct{ inner error }

func (e errorWrapper) Error() string { return "service: " + e.inner.Error() }
func (e errorWrapper) Unwrap() error { return e.inner }

// TestTheReplaySentinelIsNotMistakenForAProblem guards the ordering inside writeError.
//
// `*db.Replayed` is handled before the mapping table. If the table were consulted first it would
// report the error unmapped and answer 500 — a successful mutation reported as a broken service.
func TestTheReplaySentinelIsNotMistakenForAProblem(t *testing.T) {
	t.Parallel()

	if _, mapped := problemFor(&db.Replayed{Status: 201}); mapped {
		t.Error("the mapping table claims a replay, which would make the ordering in writeError " +
			"a coincidence rather than a decision")
	}
}
