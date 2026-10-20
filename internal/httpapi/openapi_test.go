package httpapi

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The published API specification (ADR-GLB-010 §5.1) is api/openapi.yaml, maintained by hand. These
// tests hold it to routes.go: every registered route has an operation, every operation names a
// registered route, and each operation's path is one that route serves.

// specPath is the document, from this package's directory.
const specPath = "../../api/openapi.yaml"

// specOperation is one operation of the document: its method, its path, and the mux pattern it names
// in x-mux-pattern.
type specOperation struct {
	method, path, mux string
	line              int
}

var (
	specPathLine   = regexp.MustCompile(`^  "(/[^"]*)":\s*$`)
	specMethodLine = regexp.MustCompile(`^    (get|put|post|delete|patch|head|options|trace):\s*$`)
	specMuxLine    = regexp.MustCompile(`^      x-mux-pattern: "([^"]+)"\s*$`)
)

// specOperations reads the document's operations. It reads lines rather than parsing YAML, which
// keeps a YAML library out of go.mod: the document's paths are two-space keys quoted under `paths:`,
// its methods four-space keys, and x-mux-pattern a six-space key. CI's linter holds the document to
// OpenAPI 3.1, so a path or method written another way is caught there, and an operation this reader
// misses fails the coverage test below.
func specOperations(t *testing.T) []specOperation {
	t.Helper()
	file, err := os.Open(specPath)
	if err != nil {
		t.Fatalf("open %s: %v", specPath, err)
	}
	defer func() { _ = file.Close() }()
	var (
		operations []specOperation
		inPaths    bool
		path       string
		current    *specOperation
	)
	scanner := bufio.NewScanner(file)
	for number := 1; scanner.Scan(); number++ {
		line := scanner.Text()
		switch {
		case line == "paths:":
			inPaths = true
			continue
		case !inPaths:
			continue
		case line != "" && !strings.HasPrefix(line, " "):
			// The next top-level key ends the paths object.
			inPaths = false
			continue
		}
		if match := specPathLine.FindStringSubmatch(line); match != nil {
			path, current = match[1], nil
			continue
		}
		if match := specMethodLine.FindStringSubmatch(line); match != nil && path != "" {
			operations = append(operations, specOperation{method: strings.ToUpper(match[1]), path: path, line: number})
			current = &operations[len(operations)-1]
			continue
		}
		if match := specMuxLine.FindStringSubmatch(line); match != nil && current != nil {
			current.mux = match[1]
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	if len(operations) < 50 {
		t.Fatalf("found %d operations in %s; the reader is not working", len(operations), specPath)
	}
	return operations
}

// registeredRoutes reads every method-qualified pattern routes.go registers on any of its muxes: the
// probes, the anonymous lookup, the API, and the root mux Mount joins them on.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	source, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	pattern := regexp.MustCompile(`(?:api\.HandleFunc|probes\.HandleFunc|anonymous\.HandleFunc|root\.Handle)\("((?:GET|POST|PUT|PATCH|DELETE) [^"]+)"`)
	routes := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
		routes[match[1]] = true
	}
	if len(routes) < 50 {
		t.Fatalf("found %d routes in routes.go; the pattern no longer reads it", len(routes))
	}
	return routes
}

// servedBy reports whether a mux pattern's path serves the document's path: the same segments, where
// a wildcard segment of the pattern ({name}) stands for any one segment of the document, and every
// other segment is equal. Every route here is its own pattern, so a document path is its pattern's
// path; the rule still lets a path name its parameter differently.
func servedBy(muxPath, specPath string) bool {
	mux, spec := strings.Split(muxPath, "/"), strings.Split(specPath, "/")
	if len(mux) != len(spec) {
		return false
	}
	for i := range mux {
		wildcard := strings.HasPrefix(mux[i], "{") && strings.HasSuffix(mux[i], "}")
		if !wildcard && mux[i] != spec[i] {
			return false
		}
	}
	return true
}

// TestTheSpecificationDescribesEveryRoute fails on a route routes.go registers with no operation in
// api/openapi.yaml.
func TestTheSpecificationDescribesEveryRoute(t *testing.T) {
	named := map[string]bool{}
	for _, operation := range specOperations(t) {
		named[operation.mux] = true
	}
	var missing []string
	for route := range registeredRoutes(t) {
		if !named[route] {
			missing = append(missing, route)
		}
	}
	sort.Strings(missing)
	for _, route := range missing {
		t.Errorf("%s is registered in routes.go and no operation of %s names it in x-mux-pattern; describe it, "+
			"and bump info.version's minor version", route, specPath)
	}
}

// TestEveryOperationNamesARegisteredRoute fails on an operation whose mux pattern routes.go does not
// register, whose method differs from it, or whose path that pattern does not serve.
func TestEveryOperationNamesARegisteredRoute(t *testing.T) {
	routes := registeredRoutes(t)
	for _, operation := range specOperations(t) {
		where := specPath + ":" + strconv.Itoa(operation.line) + " " + operation.method + " " + operation.path
		if operation.mux == "" {
			t.Errorf("%s has no x-mux-pattern", where)
			continue
		}
		method, muxPath, _ := strings.Cut(operation.mux, " ")
		switch {
		case !routes[operation.mux]:
			t.Errorf("%s names %q, which routes.go does not register", where, operation.mux)
		case method != operation.method:
			t.Errorf("%s names %q, a route of another method", where, operation.mux)
		case !servedBy(muxPath, operation.path):
			t.Errorf("%s names %q, which does not serve that path", where, operation.mux)
		}
	}
}

// TestServedByMatchesWholeSegments pins the matching rule the two tests above rely on.
func TestServedByMatchesWholeSegments(t *testing.T) {
	cases := []struct {
		mux, spec string
		want      bool
	}{
		{"/v1/memberships/{membership_id}/suspend", "/v1/memberships/{membership_id}/suspend", true},
		{"/v1/memberships/{membership_id}", "/v1/memberships/{membership_id}/suspend", false},
		{"/v1/workspaces", "/v1/workspaces/{workspace_id}", false},
		{"/v1/privileged-access:unreviewed", "/v1/privileged-access:unreviewed", true},
		{"/v1/privileged-access:unreviewed", "/v1/privileged-access", false},
		{"/v1/dead-letters/{event_id}/consumers/{consumer}/replay", "/v1/dead-letters/{event_id}/consumers/{consumer_id}/replay", true},
		{"/v1/tenants/{tenant_id}/administrators", "/v1/tenants/{tenant_id}/provisioning", false},
	}
	for _, tc := range cases {
		if got := servedBy(tc.mux, tc.spec); got != tc.want {
			t.Errorf("servedBy(%q, %q) = %v, want %v", tc.mux, tc.spec, got, tc.want)
		}
	}
}
