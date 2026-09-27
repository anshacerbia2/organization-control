package httpapi

// A registered consumer reaches its own routes and no others.
//
// A consumer resolves to the provider isolation scope, because it reads across Tenants. Until this
// test existed, requireProvider checked the scope alone, so a consumer token that added
// X-Administrative-Reason was admitted to every provider route: it could suspend a Tenant, retire an
// Organization, and close or waive a dead letter. The consumer's client credential was, in effect,
// the provider's.
//
// The routes are read from routes.go rather than listed here, so a route added later is covered
// without anyone remembering to add it.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// consumerRoutes are the routes a registered consumer may call as itself. Everything else refuses it.
var consumerRoutes = map[string]bool{
	"GET /v1/projections/consumers/{consumer_id}":            true,
	"POST /v1/projections/consumers/{consumer_id}/progress":  true,
	"POST /v1/projections/consumers/{consumer_id}/bootstrap": true,
	"POST /v1/projections/snapshot":                          true,
	"GET /v1/projections/frontier":                           true,
	"POST /v1/context/verify":                                true,
	"POST /v1/context/switch-eligible":                       true,
}

var apiRoute = regexp.MustCompile(`api\.HandleFunc\("([A-Z]+) ([^"]+)"`)

func registeredAPIRoutes(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("reading routes.go: %v", err)
	}
	var routes []string
	for _, match := range apiRoute.FindAllStringSubmatch(string(source), -1) {
		routes = append(routes, match[1]+" "+match[2])
	}
	if len(routes) < 40 {
		t.Fatalf("found %d API routes in routes.go; the pattern no longer matches how routes are registered", len(routes))
	}
	return routes
}

var pathParameter = regexp.MustCompile(`\{[a-z_]+\}`)

func TestAConsumerIsRefusedOnEveryRouteThatIsNotItsOwn(t *testing.T) {
	t.Parallel()

	caller := consumerCallerFixture(t, "foundation-reference")
	handler := mounted(t, &caller)
	placeholder := mustID(t).String()

	seen := map[string]bool{}
	for _, route := range registeredAPIRoutes(t) {
		seen[route] = true
		if consumerRoutes[route] {
			continue
		}
		method, pattern, _ := strings.Cut(route, " ")
		path := pathParameter.ReplaceAllString(pattern, placeholder)

		request := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		// The header a provider would send. Without it a refusal would prove only that the header
		// was missing.
		request.Header.Set(ReasonHeader, "a consumer claiming provider authority")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s answered %d to a consumer carrying a reason, want 403", route, recorder.Code)
		}
	}

	for route := range consumerRoutes {
		if !seen[route] {
			t.Errorf("consumer route %s is no longer registered; update the list", route)
		}
	}
}
