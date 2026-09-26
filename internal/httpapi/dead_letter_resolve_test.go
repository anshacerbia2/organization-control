package httpapi

// The resolution body, at the transport boundary.
//
// Every case here is a refusal that lands before the database is touched: the fixture's transactor
// fails the test if reached. The resolutions that do reach it are asserted in internal/projection,
// against the real role.

import (
	"net/http"
	"testing"
)

// An unknown reason is refused, never read as REPLAYED.
//
// A caller asking for WAIVED -- which this service does not implement -- and receiving a REPLAYED
// attempt has been answered about a question it did not ask. If the event happens to carry applied
// evidence, the incident closes under a reason the caller never chose.
func TestAResolutionNamingAnUnsupportedReasonIsRefused(t *testing.T) {
	t.Parallel()

	caller := providerCaller(t)
	handler := mounted(t, &caller)
	headers := map[string]string{ReasonHeader: "an incident review"}
	path := "/v1/dead-letters/" + mustID(t).String() + "/resolve"

	for _, body := range []string{
		`{"resolution_type":"WAIVED"}`,
		`{"resolution_type":"RESNAPSHOTTED"}`,
		// Case matters: the column stores the upper-case value, and quietly folding would make two
		// spellings of one request produce different idempotency digests for the same effect.
		`{"resolution_type":"superseded"}`,
		// A field this body does not have. Unknown fields are refused so a misspelling is a 400
		// rather than a REPLAYED attempt.
		`{"resolution":"SUPERSEDED"}`,
		`{"resolution_type":`,
	} {
		recorder := post(t, handler, path, body, headers)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %s answered %d, want 400", body, recorder.Code)
		}
	}
}

func TestAResolutionIsProviderScoped(t *testing.T) {
	t.Parallel()

	caller := tenantCaller(t)
	handler := mounted(t, &caller)
	path := "/v1/dead-letters/" + mustID(t).String() + "/resolve"

	recorder := post(t, handler, path, `{"resolution_type":"SUPERSEDED"}`,
		map[string]string{ReasonHeader: "an incident review"})
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a tenant caller answered %d, want 403", recorder.Code)
	}
}
