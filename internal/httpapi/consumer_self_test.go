package httpapi

// A consumer may act on its own records and on nobody else's.
//
// The rule matters most on the routes that write a position. RecordProgress refuses a consumer that
// never took a snapshot, precisely because "a consumer that subscribed and started applying without
// a snapshot holds a model containing everything that happened since it connected and nothing that
// happened before". A consumer able to bootstrap or report progress for a different consumer could
// record a position for that consumer's model — making a stale model look current, which is the
// exact deception the bootstrap contract exists to prevent.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/observability"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// selfRequest builds a request already carrying a resolved caller and scope, which is what the
// middleware chain would have established by the time a handler runs.
func selfRequest(t *testing.T, caller Caller) *http.Request {
	t.Helper()

	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("minting a correlation: %v", err)
	}
	scope, err := resolve(caller, correlation)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/projections/consumers/x/progress", nil)
	ctx := observability.WithCorrelationID(request.Context(), correlation)
	ctx = WithCaller(ctx, caller)
	ctx = db.WithScope(ctx, scope)
	return request.WithContext(ctx)
}

func consumerCallerFixture(t *testing.T, name string) Caller {
	t.Helper()
	return Caller{Subject: mustParse(t, testSubject), Consumer: name}
}

func TestAConsumerMayActOnItsOwnRecords(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := selfRequest(t, consumerCallerFixture(t, "foundation-reference"))

	_, consumer, ok := requireConsumerSelfOrProvider(recorder, request, "foundation-reference")
	if !ok {
		t.Fatalf("a consumer naming itself was refused: %d %s", recorder.Code, recorder.Body.String())
	}
	if consumer != "foundation-reference" {
		t.Errorf("effective consumer = %q, want foundation-reference", consumer)
	}
}

// TestAConsumerMayNotActOnAnother is the assertion that keeps the token, not the request, as the
// source of consumer identity.
func TestAConsumerMayNotActOnAnother(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := selfRequest(t, consumerCallerFixture(t, "foundation-reference"))

	if _, _, ok := requireConsumerSelfOrProvider(recorder, request, "somebody-elses-consumer"); ok {
		t.Fatal("a consumer acted on another consumer's records")
	}
	if recorder.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", recorder.Code)
	}
}

// TestAConsumerNeedsNoAdministrativeReason states the asymmetry deliberately. A consumer reading or
// reporting its own position is the designed traffic, and its accountability is the per-consumer
// meter, which cannot be forgotten the way a header can.
func TestAConsumerNeedsNoAdministrativeReason(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := selfRequest(t, consumerCallerFixture(t, "foundation-reference"))
	// No ReasonHeader set.

	if _, _, ok := requireConsumerSelfOrProvider(recorder, request, "foundation-reference"); !ok {
		t.Errorf("a consumer was refused for want of an administrative reason: %d", recorder.Code)
	}
}

// TestAProviderStillOwesAReason keeps the other half: a provider performing the same action is doing
// something out of the ordinary, and the evidence should say why.
func TestAProviderStillOwesAReason(t *testing.T) {
	provider := Caller{Subject: mustParse(t, testSubject), Provider: true}

	withoutReason := httptest.NewRecorder()
	if _, _, ok := requireConsumerSelfOrProvider(withoutReason, selfRequest(t, provider), "foundation-reference"); ok {
		t.Error("a provider acted with no administrative reason")
	}

	withReason := httptest.NewRecorder()
	request := selfRequest(t, provider)
	request.Header.Set(ReasonHeader, "investigating a lagging consumer")

	_, consumer, ok := requireConsumerSelfOrProvider(withReason, request, "foundation-reference")
	if !ok {
		t.Fatalf("a provider with a reason was refused: %d %s", withReason.Code, withReason.Body.String())
	}
	// A provider may legitimately act on another consumer's behalf, so the requested identifier
	// stands rather than being replaced.
	if consumer != "foundation-reference" {
		t.Errorf("effective consumer = %q, want the requested one", consumer)
	}
}

// TestATenantCallerIsRefused keeps the third authority out. A tenant-scoped caller has no business
// reading the projection registry at all.
func TestATenantCallerIsRefused(t *testing.T) {
	tenant := Caller{
		Subject: mustParse(t, testSubject),
		Tenant:  mustParse(t, "11111111-1111-4111-8111-11111111111a"),
	}

	recorder := httptest.NewRecorder()
	if _, _, ok := requireConsumerSelfOrProvider(recorder, selfRequest(t, tenant), "foundation-reference"); ok {
		t.Fatal("a tenant-scoped caller reached a consumer record")
	}
	if recorder.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", recorder.Code)
	}
}
