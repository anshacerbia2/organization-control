package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A tenant caller on a route only a registered consumer or a provider may call is refused with the
// 403 problem the specification documents. These routes had no refusal checked against it
// (contract_test.go).
func TestATenantCallerIsRefusedOnTheConsumerRoutes(t *testing.T) {
	t.Parallel()

	caller := tenantCaller(t)
	handler := mounted(t, &caller)
	placeholder := mustID(t).String()

	for route := range consumerRoutes {
		method, pattern, _ := strings.Cut(route, " ")
		request := httptest.NewRequest(method, pathParameter.ReplaceAllString(pattern, placeholder), strings.NewReader(`{}`))
		request.Header.Set(ReasonHeader, "an audit")
		if recorder := serveChecked(t, handler, request); recorder.Code != http.StatusForbidden {
			t.Errorf("%s answered %d to a tenant caller, want 403: %s", route, recorder.Code, recorder.Body)
		}
	}
}
