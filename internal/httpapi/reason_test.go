package httpapi

// X-Administrative-Reason is refused unless it is visible US-ASCII (STD-GLB-001 §Request Header
// Values, RFC 9110 §5.5).

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAReasonOutsideVisibleASCIIIsRefusedBeforeTheHandler(t *testing.T) {
	for name, c := range map[string]struct {
		value  string
		set    bool
		status int
	}{
		"absent":               {"", false, http.StatusNoContent},
		"ascii":                {"suspend the tenant for review", true, http.StatusNoContent},
		"with a tab":           {"grant\tthe provider", true, http.StatusNoContent},
		"utf-8 section sign":   {"ADR-ORG-002 §5.2", true, http.StatusBadRequest},
		"latin-1 section sign": {"ADR-ORG-002 \xa75.2", true, http.StatusBadRequest},
		"a control character":  {"reason\x01", true, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			reached := false
			handler := reasonHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusNoContent)
			}))
			r := httptest.NewRequest(http.MethodPost, "/v1/provider-grants", nil)
			if c.set {
				r.Header[ReasonHeader] = []string{c.value}
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != c.status || reached != (c.status == http.StatusNoContent) {
				t.Errorf("answered %d (handler reached %t), want %d", w.Code, reached, c.status)
			}
		})
	}
}
