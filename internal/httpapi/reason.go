package httpapi

import (
	"net/http"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
)

// visibleASCII reports whether a header value holds only visible US-ASCII, space and horizontal tab:
// what RFC 9110 §5.5 asks of a newly defined field. Any other octet carries no encoding this service
// could rely on.
func visibleASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c != '\t' && (c < 0x20 || c > 0x7e) {
			return false
		}
	}
	return true
}

// reasonHeaders refuses an X-Administrative-Reason that is not visible US-ASCII, before any handler
// reads it (STD-GLB-001 §Request Header Values, TDD-organization-control-001 §Provider Grants). The
// reason is stored as text in the access record, and an octet of no known encoding would reach the
// database as whatever the client's library chose.
func reasonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, value := range r.Header.Values(ReasonHeader) {
			if !visibleASCII(value) {
				platform.Problem(w, r, platform.ValidationFailed,
					"X-Administrative-Reason must be visible US-ASCII (RFC 9110 section 5.5); put other text in a body")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
