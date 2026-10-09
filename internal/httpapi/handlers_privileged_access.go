package httpapi

import (
	stdcontext "context"
	"fmt"
	"net/http"
	"strings"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
	"github.com/anshacerbia2/organization-control/internal/db"
)

// The privileged-access record and its review (ADR-ORG-002 §5.6, TDD-organization-control-001
// §Privileged Access Review). Four provider routes and one Tenant route. Every provider route takes
// X-Administrative-Reason and leaves an access row of its own; the review is a command and requires
// an Idempotency-Key.

// routeMux is the API mux, recording for each request the route pattern serving it and the Tenant its
// path names, so the privileged-access record a transaction writes says which operation it served
// and which Tenant it named (ADR-ORG-002 §5.6).
//
// A wrapper at registration rather than a middleware, because only the mux knows the pattern and the
// path values, and only for the handler it chose. Routes registers every API route through it, so a
// route cannot be added that records no operation.
type routeMux struct {
	*http.ServeMux
	signals *surfaceSignals
}

func newRouteMux() routeMux { return routeMux{ServeMux: http.NewServeMux()} }

// HandleFunc registers handler for pattern, with the pattern and the path's Tenant in its context.
func (m routeMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.ServeMux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var tenant id.UUID
		if raw := r.PathValue("tenant_id"); raw != "" {
			// A malformed segment names no Tenant; the handler refuses it with 400 on its own terms.
			if parsed, err := id.Parse(raw); err == nil {
				tenant = parsed
			}
		}
		ctx := db.WithAccessRoute(r.Context(), pattern, tenant)
		if m.signals != nil {
			ctx = withSignals(ctx, m.signals)
		}
		handler(w, r.WithContext(ctx))
	})
}

type accessView struct {
	AccessID      string    `json:"access_id"`
	ActorID       string    `json:"actor_id"`
	Authority     string    `json:"authority"`
	ActivationID  *string   `json:"activation_id"`
	TenantID      *string   `json:"tenant_id"`
	Operation     *string   `json:"operation"`
	CorrelationID string    `json:"correlation_id"`
	Reason        string    `json:"reason"`
	OccurredAt    time.Time `json:"occurred_at"`
}

type accessPageView struct {
	Accesses []accessView `json:"accesses"`
	Next     *string      `json:"next"`
}

func optionalText(value *id.UUID) *string {
	if value == nil {
		return nil
	}
	text := value.String()
	return &text
}

func viewAccessPage(page authority.AccessPage) accessPageView {
	view := accessPageView{Accesses: make([]accessView, 0, len(page.Accesses)), Next: nextCursor(page.Next)}
	for _, a := range page.Accesses {
		var operation *string
		if a.Operation != "" {
			operation = &a.Operation
		}
		view.Accesses = append(view.Accesses, accessView{
			AccessID: a.ID.String(), ActorID: a.Actor.String(), Authority: a.Authority,
			ActivationID: optionalText(a.Activation), TenantID: optionalText(a.Tenant), Operation: operation,
			CorrelationID: a.Correlation.String(), Reason: a.Reason, OccurredAt: a.OccurredAt,
		})
	}
	return view
}

// window reads the time window, `from` inclusive and `to` exclusive, each an RFC 3339 instant with
// its offset (STD-GLB-001 1.6.0 §Pagination). A malformed instant is refused rather than ignored, as
// an unknown filter is: a list said to be narrowed that was not is the worse answer.
func window(w http.ResponseWriter, r *http.Request, params listParams) (from, to *time.Time, ok bool) {
	parse := func(name string) (*time.Time, bool) {
		raw := strings.TrimSpace(params.Filters[name])
		if raw == "" {
			return nil, true
		}
		instant, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			platform.Problem(w, r, platform.ValidationFailed,
				fmt.Sprintf("%s must be an RFC 3339 instant with its offset, such as 2026-10-01T00:00:00Z", name))
			return nil, false
		}
		return &instant, true
	}
	if from, ok = parse("from"); !ok {
		return nil, nil, false
	}
	if to, ok = parse("to"); !ok {
		return nil, nil, false
	}
	return from, to, true
}

// listPrivilegedAccess serves `GET /v1/privileged-access?after=&limit=&actor_id=&tenant_id=
// &correlation_id=&authority=&from=&to=`, for a provider in force. The read is itself an access,
// recorded with the caller's reason before the page is read.
func (h *handlers) listPrivilegedAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"authority", "from", "to"},
		[]string{"actor_id", "tenant_id", "correlation_id"})
	if !ok {
		return
	}
	from, to, ok := window(w, r, params)
	if !ok {
		return
	}
	page, err := h.services.AccessReview.List(r.Context(), authority.AccessQuery{
		After: params.After, Limit: params.Limit, Actor: params.IDs["actor_id"],
		Tenant: params.IDs["tenant_id"], Correlation: params.IDs["correlation_id"],
		Authority: params.Filters["authority"], From: from, To: to,
	}, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewAccessPage(page))
}

// listTenantProviderAccess serves `GET /v1/provider-access?after=&limit=&authority=&from=&to=`, for a
// Tenant administrator: the provider access that named its Tenant. The Tenant is the scope's, never
// the request's; a tenant_id in the query is refused as a parameter the list does not take.
func (h *handlers) listTenantProviderAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"authority", "from", "to"}, nil)
	if !ok {
		return
	}
	from, to, ok := window(w, r, params)
	if !ok {
		return
	}
	page, err := h.services.AccessReview.TenantList(r.Context(), authority.AccessQuery{
		After: params.After, Limit: params.Limit, Authority: params.Filters["authority"], From: from, To: to,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewAccessPage(page))
}

type reviewView struct {
	ReviewID          string    `json:"review_id"`
	ActorID           string    `json:"actor_id"`
	From              time.Time `json:"from"`
	To                time.Time `json:"to"`
	Outcome           string    `json:"outcome"`
	Statement         string    `json:"statement"`
	Accesses          int64     `json:"accesses"`
	EmergencyAccesses int64     `json:"emergency_accesses"`
	ReviewedBy        string    `json:"reviewed_by"`
	ReviewedAt        time.Time `json:"reviewed_at"`
}

func viewReview(review authority.Review) reviewView {
	return reviewView{
		ReviewID: review.ID.String(), ActorID: review.Actor.String(), From: review.From, To: review.To,
		Outcome: review.Outcome, Statement: review.Statement, Accesses: review.Accesses,
		EmergencyAccesses: review.EmergencyAccesses, ReviewedBy: review.ReviewedBy.String(),
		ReviewedAt: review.ReviewedAt,
	}
}

type recordReviewRequest struct {
	ActorID string    `json:"actor_id"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Outcome string    `json:"outcome"`
}

// recordPrivilegedAccessReview serves `POST /v1/privileged-access/reviews`: a provider's review of
// another provider's access over a period, with X-Administrative-Reason as its statement.
func (h *handlers) recordPrivilegedAccessReview(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[recordReviewRequest](w, r)
	if !ok {
		return
	}
	actor, err := id.Parse(strings.TrimSpace(body.ActorID))
	if err != nil || actor.IsNil() {
		platform.Problem(w, r, platform.ValidationFailed, "actor_id is not a valid identifier")
		return
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (authority.Review, error) {
		return h.services.AccessReview.Record(ctx, authority.ReviewRequest{
			Actor: actor, From: body.From, To: body.To, Outcome: strings.TrimSpace(body.Outcome),
			Statement: reason(r),
		})
	}, viewReview)
}

type reviewPageView struct {
	Reviews []reviewView `json:"reviews"`
	Next    *string      `json:"next"`
}

// listPrivilegedAccessReviews serves `GET /v1/privileged-access/reviews?after=&limit=&actor_id=
// &reviewed_by=`.
func (h *handlers) listPrivilegedAccessReviews(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	params, ok := readList(w, r, nil, []string{"actor_id", "reviewed_by"})
	if !ok {
		return
	}
	page, err := h.services.AccessReview.Reviews(r.Context(), authority.ReviewQuery{
		After: params.After, Limit: params.Limit, Actor: params.IDs["actor_id"],
		ReviewedBy: params.IDs["reviewed_by"],
	}, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := reviewPageView{Reviews: make([]reviewView, 0, len(page.Reviews)), Next: nextCursor(page.Next)}
	for _, review := range page.Reviews {
		view.Reviews = append(view.Reviews, viewReview(review))
	}
	respond(w, http.StatusOK, view)
}

type unreviewedView struct {
	ActorID    string    `json:"actor_id"`
	Unreviewed int64     `json:"unreviewed"`
	Emergency  int64     `json:"emergency"`
	OldestAt   time.Time `json:"oldest_at"`
	DueAt      time.Time `json:"due_at"`
	Overdue    bool      `json:"overdue"`
}

// unreviewedPrivilegedAccess serves `GET /v1/privileged-access:unreviewed`: each Principal with a
// provider access no review covers, oldest first. A report rather than a list: one row per Principal
// that has acted as a provider, which the grants bound.
func (h *handlers) unreviewedPrivilegedAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	report, err := h.services.AccessReview.Unreviewed(r.Context(), reason(r), time.Now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	views := make([]unreviewedView, 0, len(report))
	for _, u := range report {
		views = append(views, unreviewedView{
			ActorID: u.Actor.String(), Unreviewed: u.Unreviewed, Emergency: u.Emergency,
			OldestAt: u.OldestAt, DueAt: u.DueAt, Overdue: u.Overdue,
		})
	}
	respond(w, http.StatusOK, map[string]any{
		"review_due_days": int(authority.ReviewDue.Hours() / 24),
		"actors":          views,
	})
}
