package httpapi

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	"github.com/pb33f/libopenapi-validator/config"
	verrors "github.com/pb33f/libopenapi-validator/errors"
	"github.com/pb33f/libopenapi-validator/paths"
	"github.com/pb33f/libopenapi-validator/schema_validation"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
)

// The contract test (ADR-GLB-010 §5.1, TDD §API specification): every exchange the route tests make
// with a handler is checked against api/openapi.yaml. A response must satisfy the schema its status
// names, and a request the handler accepted must satisfy the operation's parameters and request body.
// The same file is in identity-control and organization-control.

// contractSpecPath is the document, from this package's directory.
const contractSpecPath = "../../api/openapi.yaml"

// contract is the loaded document, the validator over it, and what the tests have exercised.
var contract struct {
	once  sync.Once
	err   error
	model *v3.Document
	check validator.Validator

	mu   sync.Mutex
	seen map[string]map[int]bool // "METHOD /spec/path" to the statuses a test exercised
}

func loadContract() error {
	contract.once.Do(func() {
		contract.seen = map[string]map[int]bool{}
		source, err := os.ReadFile(contractSpecPath)
		if err != nil {
			contract.err = err
			return
		}
		document, err := libopenapi.NewDocument(source)
		if err != nil {
			contract.err = err
			return
		}
		model, err := document.BuildV3Model()
		if err != nil {
			contract.err = err
			return
		}
		contract.model = &model.Model
		// The route tests set the caller on the request's context, as the composition root's middleware
		// does, so no Authorization header is on the request; security is the route tests' subject.
		contract.check = validator.NewValidatorFromV3Model(contract.model, config.WithoutSecurityValidation())
	})
	return contract.err
}

// conforms checks one exchange against the specification and records it as exercised. body is what
// the request carried, read before the handler consumed it. A request no operation describes is not
// checked: a route the mux does not serve answers outside the specification.
func conforms(t testing.TB, request *http.Request, body []byte, recorded *httptest.ResponseRecorder) {
	t.Helper()
	if err := loadContract(); err != nil {
		t.Fatalf("load %s: %v", contractSpecPath, err)
	}
	asked := request.Clone(request.Context())
	asked.Body = io.NopCloser(bytes.NewReader(body))
	asked.ContentLength = int64(len(body))
	if len(body) > 0 && asked.Header.Get("Content-Type") == "" {
		asked.Header.Set("Content-Type", "application/json")
	}
	// The document's paths put a custom method after a parameter ({principal_id}:suspend), which the
	// validator's own radix-tree router cannot tell apart, so the path is resolved here with its
	// regular-expression matcher and handed to the validator by path item.
	item, found, template := paths.FindPath(asked, contract.model, config.NewValidationOptions())
	if found != nil || template == "" {
		return
	}
	operation := request.Method + " " + template
	response := recorded.Result()
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	checked := &http.Response{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(answer)),
	}
	// A server sets the Content-Type of a body written without one; a recorder does not.
	if len(answer) > 0 && checked.Header.Get("Content-Type") == "" {
		checked.Header.Set("Content-Type", http.DetectContentType(answer))
	}
	if undeclaredInternalError(item, request.Method, response.StatusCode) {
		// An unexpected failure is not a documented response: it is checked as a problem document.
		if media := response.Header.Get("Content-Type"); !strings.HasPrefix(media, "application/problem+json") {
			t.Errorf("%s answered 500 as %q, not application/problem+json", operation, media)
		}
		problem, _ := contract.model.Components.Schemas.Get("Problem")
		if ok, problems := schema_validation.NewSchemaValidator().ValidateSchemaBytes(problem.Schema(), answer); !ok {
			t.Errorf("%s answered 500 outside the Problem schema:\n%s\nbody: %s", operation, describe(problems), answer)
		}
	} else if ok, problems := contract.check.GetResponseBodyValidator().ValidateResponseBodyWithPathItem(asked, checked, item, template); !ok {
		t.Errorf("%s answered %d against api/openapi.yaml:\n%s\nbody: %s", operation, response.StatusCode, describe(problems), answer)
	}
	if response.StatusCode < 400 {
		asked = request.Clone(request.Context())
		asked.Body = io.NopCloser(bytes.NewReader(body))
		asked.ContentLength = int64(len(body))
		if len(body) > 0 && asked.Header.Get("Content-Type") == "" {
			asked.Header.Set("Content-Type", "application/json")
		}
		if ok, problems := contract.check.ValidateHttpRequestWithPathItem(asked, item, template); !ok {
			t.Errorf("%s was accepted with %d, but the request does not satisfy api/openapi.yaml:\n%s\nbody: %s",
				operation, response.StatusCode, describe(problems), body)
		}
	}
	contract.mu.Lock()
	defer contract.mu.Unlock()
	if contract.seen[operation] == nil {
		contract.seen[operation] = map[int]bool{}
	}
	contract.seen[operation][response.StatusCode] = true
}

// undeclaredInternalError reports a 500 the operation does not declare and has no default for. OpenAPI
// 3.1 does not ask a document to list every status ("The documentation is not necessarily expected to
// cover all possible HTTP response codes because they may not be known in advance"), and RFC 9110
// §15.6.1 defines 500 as an unexpected condition. Any other undeclared status is a known answer the
// document is missing, and fails.
func undeclaredInternalError(item *v3.PathItem, method string, status int) bool {
	operation := item.GetOperations().GetOrZero(strings.ToLower(method))
	if status != http.StatusInternalServerError || operation == nil || operation.Responses == nil {
		return false
	}
	_, declared := operation.Responses.Codes.Get("500")
	return !declared && operation.Responses.Default == nil
}

// describe lists what the validator found, with the schema's own reasons.
func describe(problems []*verrors.ValidationError) string {
	var lines []string
	for _, problem := range problems {
		lines = append(lines, fmt.Sprintf("  - %s: %s (spec line %d)", problem.Message, problem.Reason, problem.SpecLine))
		for _, failure := range problem.SchemaValidationErrors {
			lines = append(lines, fmt.Sprintf("      at %s: %s", failure.FieldPath, failure.Reason))
		}
	}
	return strings.Join(lines, "\n")
}

// serveChecked runs a request through a handler and checks the exchange.
func serveChecked(t testing.TB, handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if request.Body != nil {
		var err error
		if body, err = io.ReadAll(request.Body); err != nil {
			t.Fatalf("read the request body: %v", err)
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	recorded := httptest.NewRecorder()
	handler.ServeHTTP(recorded, request)
	conforms(t, request, body, recorded)
	return recorded
}

// TestMain holds the route tests to the specification's coverage: after a whole run of the package,
// every operation that declares a success response has had a success checked, and every operation
// has had a problem checked. A run narrowed by -run or -short is not held to it, and neither is a run
// with no integration database: the success exchanges are contract_live_test.go's, against it. CI sets
// REQUIRE_INTEGRATION, so there the gate always applies.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && wholeRun() {
		if missing := uncovered(); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "contract coverage: %d operation(s) of api/openapi.yaml were not exercised by the route tests:\n  %s\n",
				len(missing), strings.Join(missing, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}

func wholeRun() bool {
	run := flag.Lookup("test.run")
	short := flag.Lookup("test.short")
	database := os.Getenv("TEST_DATABASE_URL") != "" || os.Getenv("REQUIRE_INTEGRATION") != ""
	return database && (run == nil || run.Value.String() == "") && (short == nil || short.Value.String() != "true")
}

// uncovered names each operation lacking a checked success (when it declares one) or a checked
// problem.
func uncovered() []string {
	if err := loadContract(); err != nil {
		return []string{err.Error()}
	}
	var missing []string
	contract.mu.Lock()
	defer contract.mu.Unlock()
	for pair := contract.model.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		for method, operation := range pair.Value().GetOperations().FromOldest() {
			key := strings.ToUpper(method) + " " + pair.Key()
			var wantSuccess, wantProblem bool
			for code := range operation.Responses.Codes.KeysFromOldest() {
				wantSuccess = wantSuccess || strings.HasPrefix(code, "2")
				wantProblem = wantProblem || strings.HasPrefix(code, "4") || strings.HasPrefix(code, "5")
			}
			var success, problem bool
			for status := range contract.seen[key] {
				success = success || status < 300
				problem = problem || status >= 400
			}
			switch {
			case wantSuccess && !success && !problem:
				missing = append(missing, key+": no success and no problem")
			case wantSuccess && !success:
				missing = append(missing, key+": no success")
			case wantProblem && !problem:
				missing = append(missing, key+": no problem")
			}
		}
	}
	sort.Strings(missing)
	return missing
}
