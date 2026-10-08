package httpapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// postRoutes reads routes.go for every POST the API mux registers, and whether it is a command.
//
// Read from the source rather than asked of the mux: ServeMux does not list its patterns, and the
// question is what the author wrote -- `command(` or not -- which is what a reviewer reads too.
func postRoutes(t *testing.T) (commands, others []string) {
	t.Helper()
	source, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	pattern := regexp.MustCompile(`api\.HandleFunc\("(POST [^"]+)", (command\()?`)
	for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
		if match[2] != "" {
			commands = append(commands, match[1])
		} else {
			others = append(others, match[1])
		}
	}
	// A broken pattern would find nothing and pass everything below.
	if len(commands) < 40 || len(others) < len(keyOptional) {
		t.Fatalf("found %d command and %d other POST routes in routes.go; the source walk is not working",
			len(commands), len(others))
	}
	return commands, others
}

// TestEveryPostRouteIsClassified holds every POST to a decision: a command, refused without an
// Idempotency-Key, or a route named in keyOptional with its reason. Never both, never neither.
func TestEveryPostRouteIsClassified(t *testing.T) {
	t.Parallel()

	commands, others := postRoutes(t)
	for _, route := range commands {
		if reason, ok := keyOptional[route]; ok {
			t.Errorf("%s is a command and also named optional (%q)", route, reason)
		}
	}
	registered := map[string]bool{}
	for _, route := range others {
		registered[route] = true
		if reason := keyOptional[route]; strings.TrimSpace(reason) == "" {
			t.Errorf("%s is registered without command( and keyOptional gives no reason it needs no "+
				"Idempotency-Key; wrap it in command, or name it with the reason", route)
		}
	}
	for route := range keyOptional {
		if !registered[route] {
			t.Errorf("keyOptional names %s, which routes.go does not register without command(", route)
		}
	}
}

// fill gives every wildcard of a pattern a value, so the mux routes the request to its handler.
func fill(t *testing.T, pattern string) string {
	t.Helper()
	path := strings.TrimPrefix(pattern, "POST ")
	return regexp.MustCompile(`\{[^}]+\}`).ReplaceAllStringFunc(path, func(string) string {
		return mustID(t).String()
	})
}

// TestACommandWithoutAKeyIsRefusedBeforeTheDatabase sends every command without a key, as the
// caller its route admits: one of a Tenant administrator and a provider is admitted and must be
// answered 400 naming the header; the other is refused for its authority, also before the database.
// The surface's transactor fails the test if a request reaches it.
func TestACommandWithoutAKeyIsRefusedBeforeTheDatabase(t *testing.T) {
	t.Parallel()

	commands, _ := postRoutes(t)
	tenant, provider := tenantCaller(t), providerCaller(t)
	tenantHandler, providerHandler := mounted(t, &tenant), mounted(t, &provider)

	for _, route := range commands {
		t.Run(route, func(t *testing.T) {
			path := fill(t, route)
			refusedForTheKey := 0
			for _, handler := range []http.Handler{tenantHandler, providerHandler} {
				recorder := post(t, handler, path, `{}`, map[string]string{
					ReasonHeader: "an audit", IdempotencyHeader: "",
				})
				switch {
				case recorder.Code == http.StatusBadRequest && strings.Contains(recorder.Body.String(), IdempotencyHeader):
					refusedForTheKey++
				case recorder.Code == http.StatusForbidden:
				default:
					t.Errorf("answered %d without a key, want 400 naming %s or 403:\n%s",
						recorder.Code, IdempotencyHeader, recorder.Body.String())
				}
			}
			if refusedForTheKey != 1 {
				t.Errorf("%d callers were refused for the missing key, want exactly the one the route admits",
					refusedForTheKey)
			}
		})
	}
}

// TestABlankKeyIsNoKey: whitespace makes no claim, so honouring it as a key would tell the caller
// its retries were safe when they are not.
func TestABlankKeyIsNoKey(t *testing.T) {
	t.Parallel()

	caller := tenantCaller(t)
	recorder := post(t, mounted(t, &caller), "/v1/workspaces", `{}`, map[string]string{IdempotencyHeader: "   "})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), IdempotencyHeader) {
		t.Errorf("a blank key answered %d, want 400 naming the header:\n%s", recorder.Code, recorder.Body.String())
	}
}

// TestRequireKeyPassesARouteThatIsNotACommand: the check is the command wrapper's mark, and a route
// without it is untouched.
func TestRequireKeyPassesARouteThatIsNotACommand(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	if !requireKey(recorder, httptest.NewRequest(http.MethodPost, "/v1/projections/reconcile", nil)) {
		t.Errorf("a route not wrapped in command was refused for its key: %s", recorder.Body.String())
	}
	marked := false
	command(func(w http.ResponseWriter, r *http.Request) { marked = !requireKey(w, r) })(
		recorder, httptest.NewRequest(http.MethodPost, "/v1/workspaces", nil))
	if !marked {
		t.Error("a command without a key passed requireKey")
	}
}

// recordedAfter names the commands whose response is completed after the handler writes, in a
// transaction of its own, rather than inside the transaction that commits the effect, with the
// reason (TDD-organization-control-003 §The Response Is Recorded with the Effect).
var recordedAfter = map[string]string{
	"executeMembershipBatch": "each item commits in its own transaction (ADR-ORG-004 §5.1), so no one " +
		"transaction holds every effect the response reports",
	"freezeOffboarding": "the stage is read in one transaction and the batch frozen in another, on the " +
		"tenant pool",
	"cancelOffboarding": "the Tenant's return commits first and each Membership restore after it " +
		"(ADR-ORG-006 §5.2)",
	"retireConsumer": "204 with no body: there is no JSON response to record, before or after",
}

// TestEveryCommandRecordsItsResponseWithItsEffect holds each command route to answer -- which hands
// the service a renderer so the response is recorded inside the transaction that commits the effect
// -- or to a named reason in recordedAfter. A new command written with respond would otherwise
// quietly reopen the window between the commit and the completion.
func TestEveryCommandRecordsItsResponseWithItsEffect(t *testing.T) {
	bodies := handlerBodies(t)
	routes, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	commands := regexp.MustCompile(`command\(h\.(\w+)\)`).FindAllStringSubmatch(string(routes), -1)
	if len(commands) == 0 {
		t.Fatal("no command routes found in routes.go; the pattern is stale")
	}
	calls := regexp.MustCompile(`h\.(\w+)\(w, r`)
	answers := func(name string) bool {
		body := bodies[name]
		if strings.Contains(body, "answer(w, r,") {
			return true
		}
		for _, call := range calls.FindAllStringSubmatch(body, -1) {
			if strings.Contains(bodies[call[1]], "answer(w, r,") {
				return true
			}
		}
		return false
	}
	for _, match := range commands {
		name := match[1]
		_, excepted := recordedAfter[name]
		switch {
		case answers(name) && excepted:
			t.Errorf("%s answers inside the transaction and is still listed in recordedAfter", name)
		case !answers(name) && !excepted:
			t.Errorf("%s writes its response with respond: use answer, or name it in recordedAfter with the reason", name)
		}
	}
	for name := range recordedAfter {
		if _, ok := bodies[name]; !ok {
			t.Errorf("recordedAfter names %s, which is not a handler", name)
		}
	}
}

// handlerBodies reads every handler method's source by name.
func handlerBodies(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("handlers*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	bodies := map[string]string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		parsed, err := parser.ParseFile(fset, file, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			bodies[fn.Name.Name] = string(source[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
		}
	}
	return bodies
}
