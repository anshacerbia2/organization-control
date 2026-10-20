package httpapi

import (
	stdcontext "context"
	"net/http"
	"strings"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
	"github.com/anshacerbia2/organization-control/internal/db"
)

// The reporter registry (ADR-ORG-001 §5.11, amended 2026-10-20; TDD-organization-control-001
// §Reporters). Four provider routes, each requiring X-Administrative-Reason, and the two commands an
// Idempotency-Key. The reason is the access record's and the registration's or the retirement's own.
//
// The report routes themselves are the provisioning system's and the obligation-owning domains', in
// handlers_provider.go; requireProvisioningReport and requireObligationReporter below admit them.

type reporterView struct {
	ReporterID   string     `json:"reporter_id"`
	PrincipalID  string     `json:"principal_id"`
	Reports      string     `json:"reports"`
	Purpose      string     `json:"purpose"`
	State        string     `json:"state"`
	Version      int64      `json:"version"`
	RegisteredAt time.Time  `json:"registered_at"`
	RegisteredBy string     `json:"registered_by"`
	RetiredAt    *time.Time `json:"retired_at"`
	RetiredBy    *string    `json:"retired_by"`
}

func viewReporter(r authority.Reporter) reporterView {
	view := reporterView{
		ReporterID: r.ID.String(), PrincipalID: r.Principal.String(), Reports: string(r.Reports),
		Purpose: r.Purpose, State: "active", Version: r.Version, RegisteredAt: r.RegisteredAt,
		RegisteredBy: r.RegisteredBy.String(), RetiredAt: r.RetiredAt,
	}
	if !r.Active() {
		view.State = "retired"
	}
	if r.RetiredBy != nil {
		retiredBy := r.RetiredBy.String()
		view.RetiredBy = &retiredBy
	}
	return view
}

type reporterPageView struct {
	Reporters []reporterView `json:"reporters"`
	Next      *string        `json:"next"`
}

func (h *handlers) listReporters(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"reports", "state"}, nil)
	if !ok {
		return
	}
	page, err := h.services.Reporters.List(r.Context(), authority.ReporterQuery{
		After: params.After, Limit: params.Limit,
		Reports: params.Filters["reports"], State: params.Filters["state"],
	}, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := reporterPageView{Reporters: make([]reporterView, 0, len(page.Reporters)), Next: nextCursor(page.Next)}
	for _, reporter := range page.Reporters {
		view.Reporters = append(view.Reporters, viewReporter(reporter))
	}
	respond(w, http.StatusOK, view)
}

func (h *handlers) getReporter(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	reporterID, ok := pathUUID(w, r, "reporter_id")
	if !ok {
		return
	}
	reporter, err := h.services.Reporters.Get(r.Context(), reporterID, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewReporter(reporter))
}

type registerReporterRequest struct {
	PrincipalID string `json:"principal_id"`

	// Reports is provisioning, or obligation:<domain>.
	Reports string `json:"reports"`
	Purpose string `json:"purpose"`
}

func (h *handlers) registerReporter(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[registerReporterRequest](w, r)
	if !ok {
		return
	}
	principal, err := id.Parse(strings.TrimSpace(body.PrincipalID))
	if err != nil || principal.IsNil() {
		platform.Problem(w, r, platform.ValidationFailed, "principal_id is not a valid identifier")
		return
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (authority.Reporter, error) {
		return h.services.Reporters.Register(ctx, authority.ReporterRegistration{
			Principal: principal, Reports: authority.Reports(strings.TrimSpace(body.Reports)),
			Purpose: body.Purpose, Reason: reason(r),
		})
	}, viewReporter)
}

type retireReporterRequest struct {
	ExpectedVersion int64 `json:"expected_version"`
}

func (h *handlers) retireReporter(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	reporterID, ok := pathUUID(w, r, "reporter_id")
	if !ok {
		return
	}
	body, ok := decode[retireReporterRequest](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (authority.Reporter, error) {
		return h.services.Reporters.Retire(ctx, reporterID, body.ExpectedVersion, reason(r))
	}, viewReporter)
}

// reporterReason is recorded when a reporter sends no X-Administrative-Reason. A reporter is a
// workload doing what it is registered for, as a consumer is, so the header is not demanded of it; its
// access record still carries a reason, and this one says whose report it was.
func reporterReason(r *http.Request, caller Caller) string {
	if given := reason(r); given != "" {
		return given
	}
	return "reported by the registered " + string(caller.Reports) + " reporter " + caller.Reporter.String()
}

// requireProvisioningReport admits the two callers of the provisioning system's report routes: its
// registered reporter workload, or a provider reporting on its behalf with a reason that carries the
// system's confirmation (ADR-ORG-001 §5.11). It returns the reason the access record carries.
func requireProvisioningReport(w http.ResponseWriter, r *http.Request) (db.Scope, string, bool) {
	caller, _ := CallerFrom(r.Context())
	if caller.Reporter.IsNil() {
		scope, ok := requireProvider(w, r)
		return scope, reason(r), ok
	}
	scope, ok := db.ScopeFrom(r.Context())
	if !ok {
		platform.Problem(w, r, platform.Internal, "The request could not be completed")
		return db.Scope{}, "", false
	}
	if !caller.Reports.Provisioning() {
		// Unreachable behind authentication, which admits a reporter on its own kind of report alone;
		// refused here as well so the route does not rest its one rule on another file.
		platform.Problem(w, r, platform.Forbidden, "This route takes the provisioning system's report, and the caller reports "+
			string(caller.Reports))
		return db.Scope{}, "", false
	}
	if !requireKey(w, r) {
		return db.Scope{}, "", false
	}
	return scope, reporterReason(r, caller), true
}

// requireObligationReporter admits an obligation-owning domain's registered reporter alone, and
// returns its domain, from the record (ADR-ORG-001 §5.11). No provider resolves another domain's
// obligation: that is TDD-organization-control-004's rule, and the board offers no control for it.
func requireObligationReporter(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller, _ := CallerFrom(r.Context())
	domain := caller.Reports.Domain()
	if caller.Reporter.IsNil() || domain == "" {
		platform.Problem(w, r, platform.Forbidden,
			"An obligation is resolved by its domain's registered reporter workload; no provider resolves another domain's obligation")
		return "", false
	}
	if !requireKey(w, r) {
		return "", false
	}
	return domain, true
}
