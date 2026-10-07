package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anshacerbia2/organization-control/internal/membership"
)

// TestABatchIsRefusedBeforeTheDatabase holds the batch routes to the rules the single transition
// keeps: tenant callers only, a revocation carries a reason, and a malformed request is refused
// whole. Every case is answered before the refusing transactor is reached.
func TestABatchIsRefusedBeforeTheDatabase(t *testing.T) {
	t.Parallel()

	tenant := tenantCaller(t)
	provider := providerCaller(t)
	audit := map[string]string{ReasonHeader: "an audit"}
	one := `"` + mustID(t).String() + `"`
	batch := "/v1/membership-batches/" + mustID(t).String()

	var tooMany []string
	for i := 0; i <= membership.MaxBatchItems; i++ {
		tooMany = append(tooMany, `"`+mustID(t).String()+`"`)
	}

	cases := []struct {
		name    string
		caller  Caller
		path    string
		body    string
		headers map[string]string
		status  int
		want    string
	}{
		{"a provider previewing", provider, "/v1/membership-batches", `{"action":"suspend","membership_ids":[` + one + `]}`, audit, http.StatusForbidden, ""},
		{"a provider executing", provider, batch + "/execute", ``, audit, http.StatusForbidden, ""},
		{"a revocation without a reason", tenant, "/v1/membership-batches", `{"action":"revoke","membership_ids":[` + one + `]}`, nil, http.StatusBadRequest, ReasonHeader},
		{"no items", tenant, "/v1/membership-batches", `{"action":"suspend","membership_ids":[]}`, nil, http.StatusBadRequest, "at least one"},
		{"a repeated item", tenant, "/v1/membership-batches", `{"action":"suspend","membership_ids":[` + one + `,` + one + `]}`, nil, http.StatusBadRequest, "more than once"},
		{"more than the bound", tenant, "/v1/membership-batches", `{"action":"suspend","membership_ids":[` + strings.Join(tooMany, ",") + `]}`, nil, http.StatusBadRequest, "at most 500"},
		{"a grant", tenant, "/v1/membership-batches", `{"action":"grant","membership_ids":[` + one + `]}`, nil, http.StatusBadRequest, "action"},
		{"an identifier that is not a UUID", tenant, "/v1/membership-batches", `{"action":"suspend","membership_ids":["m-1"]}`, nil, http.StatusBadRequest, ""},
		{"an unknown field", tenant, "/v1/membership-batches", `{"action":"suspend","membership_ids":[` + one + `],"dry_run":true}`, nil, http.StatusBadRequest, "dry_run"},
		{"a negative allowance", tenant, batch + "/execute", `{"fail_on_errors":-1}`, nil, http.StatusBadRequest, "fail_on_errors"},
		{"an execute body with an unknown field", tenant, batch + "/execute", `{"failOnErrors":1}`, nil, http.StatusBadRequest, "failOnErrors"},
		{"a batch named by something other than a UUID", tenant, "/v1/membership-batches/b-1/execute", ``, nil, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caller := tc.caller
			recorder := post(t, mounted(t, &caller), tc.path, tc.body, tc.headers)
			if recorder.Code != tc.status {
				t.Fatalf("%s answered %d, want %d:\n%s", tc.path, recorder.Code, tc.status, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.want) {
				t.Errorf("the refusal did not name %q:\n%s", tc.want, recorder.Body.String())
			}
		})
	}

	for _, path := range []string{batch, "/v1/memberships/" + mustID(t).String() + "/enforcement"} {
		caller := provider
		if recorder := get(t, mounted(t, &caller), path, audit); recorder.Code != http.StatusForbidden {
			t.Errorf("a provider reading %s answered %d, want 403", path, recorder.Code)
		}
	}
}

// TestAnItemsProblemIsTheSingleCommandsProblem: the classifier renders through writeError, so an
// item's refusal carries the type, title and status the single command's response would.
func TestAnItemsProblemIsTheSingleCommandsProblem(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/v1/membership-batches", nil)
	classify := classifierFor(request)
	cases := []struct {
		err    error
		status int
		suffix string
	}{
		{fmt.Errorf("%w: expected 1, stored 3", membership.ErrVersionMismatch), http.StatusConflict, "/version-conflict"},
		{membership.ErrRevoked, http.StatusConflict, "/state-transition-refused"},
		{fmt.Errorf("%w: m", membership.ErrNotFound), http.StatusNotFound, "/not-found"},
		{errors.New("a statement failed"), http.StatusInternalServerError, "/internal"},
	}
	for _, tc := range cases {
		problem := classify(tc.err)
		recorder := httptest.NewRecorder()
		writeError(recorder, request, tc.err)
		if problem.Status != tc.status || problem.Status != recorder.Code || !strings.HasSuffix(problem.Type, tc.suffix) || problem.Title == "" {
			t.Errorf("%v classified as %+v; the single response was %d", tc.err, problem, recorder.Code)
		}
	}
	if detail := classify(fmt.Errorf("%w: expected 1, stored 3", membership.ErrVersionMismatch)).Detail; !strings.Contains(detail, "stored 3") {
		t.Errorf("the detail %q is not the single command's", detail)
	}
	if detail := classify(errors.New("SELECT secret FROM x")).Detail; strings.Contains(detail, "secret") {
		t.Errorf("an internal failure's detail leaked: %q", detail)
	}
}
