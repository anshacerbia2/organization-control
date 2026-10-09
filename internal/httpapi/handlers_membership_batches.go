package httpapi

// Membership batches and the enforcement read (ADR-ORG-004, TDD-organization-control-002 1.10.0).
// Tenant-scoped, like the single transitions: the Tenant is the caller's, from the token, and
// Row-Level Security confines the batch and every Membership it names.

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/membership"
)

type membershipBatchRequest struct {
	Action        string    `json:"action"`
	MembershipIDs []id.UUID `json:"membership_ids"`
	Continues     *id.UUID  `json:"continues,omitempty"`
}

// previewMembershipBatch serves `POST /v1/membership-batches`.
//
// A revocation batch needs X-Administrative-Reason and is refused before anything is read without
// one, as the single revocation is.
func (h *handlers) previewMembershipBatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	body, ok := decode[membershipBatchRequest](w, r)
	if !ok {
		return
	}
	if membership.Action(body.Action) == membership.ActionRevoke && reason(r) == "" {
		platform.Problem(w, r, platform.ValidationFailed,
			"A revocation is irreversible and must carry the "+ReasonHeader+" header")
		return
	}
	req := membership.BatchRequest{
		Action:        membership.Action(body.Action),
		MembershipIDs: body.MembershipIDs,
		Reason:        reason(r),
	}
	if body.Continues != nil {
		req.Continues = *body.Continues
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (membership.Batch, error) {
		return h.services.Memberships.PreviewBatch(ctx, req, classifierFor(r))
	}, viewBatch)
}

type executeBatchRequest struct {
	FailOnErrors *int `json:"fail_on_errors"`
}

// executeMembershipBatch serves `POST /v1/membership-batches/{batch_id}/execute`. The body is
// optional: absent, every item is attempted. 200 whatever the items' outcomes, which the body
// carries one by one. On a batch left `executing` by a request that ended, it resumes the execution
// (TDD-organization-control-002 §Resuming an execution).
func (h *handlers) executeMembershipBatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	batchID, ok := pathUUID(w, r, "batch_id")
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		platform.Problem(w, r, platform.ValidationFailed, "The request body could not be read")
		return
	}
	var body executeBatchRequest
	if len(bytes.TrimSpace(raw)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if body, ok = decode[executeBatchRequest](w, r); !ok {
			return
		}
	}
	batch, err := h.services.Memberships.ExecuteBatch(r.Context(), batchID, body.FailOnErrors, classifierFor(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewBatch(batch))
}

func (h *handlers) getMembershipBatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	batchID, ok := pathUUID(w, r, "batch_id")
	if !ok {
		return
	}
	batch, err := h.services.Memberships.GetBatch(r.Context(), batchID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewBatch(batch))
}

// membershipEnforcement serves `GET /v1/memberships/{membership_id}/enforcement`.
func (h *handlers) membershipEnforcement(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	membershipID, ok := pathUUID(w, r, "membership_id")
	if !ok {
		return
	}
	report, err := h.services.Memberships.Enforcement(r.Context(), membershipID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewEnforcement(report))
}

// tenantMembershipEnforcement serves
// `GET /v1/tenants/{tenant_id}/memberships/{membership_id}/enforcement`: the same evidence, for a
// provider, with a reason. The Tenant is in the path so the access record names it and the Tenant's
// administrator reads it in GET /v1/provider-access (TDD-organization-control-002 1.15.0).
func (h *handlers) tenantMembershipEnforcement(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	membershipID, ok := pathUUID(w, r, "membership_id")
	if !ok {
		return
	}
	report, err := h.services.Memberships.EnforcementInTenant(r.Context(), tenantID, membershipID, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewEnforcement(report))
}

// classifierFor renders an item's error as the problem document the single command would answer
// the same request with: writeError itself, into a buffer, so the translation table stays the one
// in problem.go (RFC 7644 §3.7.3).
func classifierFor(r *http.Request) membership.Classifier {
	return func(err error) membership.Problem {
		buffer := &problemBuffer{header: http.Header{}}
		writeError(buffer, r, err)
		var document platform.ProblemDocument
		if json.Unmarshal(buffer.body.Bytes(), &document) != nil {
			return membership.Problem{Status: http.StatusInternalServerError}
		}
		return membership.Problem{Type: document.Type, Title: document.Title,
			Status: document.Status, Detail: document.Detail}
	}
}

// problemBuffer is the ResponseWriter classifierFor renders into.
type problemBuffer struct {
	header http.Header
	body   bytes.Buffer
}

func (b *problemBuffer) Header() http.Header         { return b.header }
func (b *problemBuffer) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *problemBuffer) WriteHeader(int)             {}

type problemView struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func viewProblem(p *membership.Problem) *problemView {
	if p == nil {
		return nil
	}
	return &problemView{Type: p.Type, Title: p.Title, Status: p.Status, Detail: p.Detail}
}

type batchCountsView struct {
	WouldChange    int `json:"would_change"`
	WouldNotChange int `json:"would_not_change"`
	Succeeded      int `json:"succeeded"`
	Failed         int `json:"failed"`
	NotAttempted   int `json:"not_attempted"`
}

// itemOutcomeView carries only the fields its status has.
type itemOutcomeView struct {
	Status     string       `json:"status"`
	AcceptedAt *time.Time   `json:"accepted_at,omitempty"`
	EventID    *id.UUID     `json:"event_id,omitempty"`
	Version    *int64       `json:"version,omitempty"`
	Problem    *problemView `json:"problem,omitempty"`
	Reason     string       `json:"reason,omitempty"`
}

type batchItemView struct {
	MembershipID    id.UUID          `json:"membership_id"`
	PrincipalID     *id.UUID         `json:"principal_id"`
	CurrentStatus   *string          `json:"current_status"`
	Version         *int64           `json:"version"`
	ResultingStatus *string          `json:"resulting_status"`
	Refusal         *problemView     `json:"refusal"`
	Outcome         *itemOutcomeView `json:"outcome"`
}

type batchView struct {
	BatchID       id.UUID         `json:"batch_id"`
	Action        string          `json:"action"`
	State         string          `json:"state"`
	Reason        *string         `json:"reason"`
	CorrelationID id.UUID         `json:"correlation_id"`
	Continues     *id.UUID        `json:"continues"`
	CreatedBy     id.UUID         `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
	ExecutedAt    *time.Time      `json:"executed_at"`
	CompletedAt   *time.Time      `json:"completed_at"`
	FailOnErrors  *int            `json:"fail_on_errors"`
	HeartbeatAt   *time.Time      `json:"heartbeat_at"`
	ResumedBy     *id.UUID        `json:"resumed_by"`
	ResumedAt     *time.Time      `json:"resumed_at"`
	Counts        batchCountsView `json:"counts"`
	Items         []batchItemView `json:"items"`
}

func statusText(s *membership.State) *string {
	if s == nil {
		return nil
	}
	text := string(*s)
	return &text
}

func viewBatch(b membership.Batch) batchView {
	succeeded, failed, notAttempted := b.Counts()
	view := batchView{
		BatchID: b.BatchID, Action: string(b.Action), State: string(b.State), Reason: b.Reason,
		CorrelationID: b.CorrelationID, Continues: b.Continues, CreatedBy: b.CreatedBy,
		CreatedAt: b.CreatedAt, ExpiresAt: b.ExpiresAt, ExecutedAt: b.ExecutedAt,
		CompletedAt: b.CompletedAt, FailOnErrors: b.FailOnErrors,
		HeartbeatAt: b.HeartbeatAt, ResumedBy: b.ResumedBy, ResumedAt: b.ResumedAt,
		Counts: batchCountsView{WouldChange: b.WouldChange, WouldNotChange: b.WouldNotChange,
			Succeeded: succeeded, Failed: failed, NotAttempted: notAttempted},
		Items: make([]batchItemView, 0, len(b.Items)),
	}
	for _, item := range b.Items {
		itemView := batchItemView{
			MembershipID: item.MembershipID, PrincipalID: item.PrincipalID,
			CurrentStatus: statusText(item.CurrentStatus), Version: item.VersionRead,
			ResultingStatus: statusText(item.ResultingStatus), Refusal: viewProblem(item.Refusal),
		}
		if o := item.Outcome; o != nil {
			itemView.Outcome = &itemOutcomeView{Status: o.Status, AcceptedAt: o.AcceptedAt,
				EventID: o.EventID, Version: o.Version, Problem: viewProblem(o.Problem), Reason: o.Reason}
		}
		view.Items = append(view.Items, itemView)
	}
	return view
}

type consumerEvidenceView struct {
	ConsumerID string     `json:"consumer_id"`
	Evidence   string     `json:"evidence"`
	RecordedAt *time.Time `json:"recorded_at"`
}

type enforcementView struct {
	MembershipID  id.UUID                `json:"membership_id"`
	EventID       id.UUID                `json:"event_id"`
	Transition    string                 `json:"transition"`
	AcceptedAt    time.Time              `json:"accepted_at"`
	PublishedAt   *time.Time             `json:"published_at"`
	BudgetSeconds int                    `json:"budget_seconds"`
	Consumers     []consumerEvidenceView `json:"consumers"`
	State         string                 `json:"state"`
	EvaluatedAt   time.Time              `json:"evaluated_at"`
}

func viewEnforcement(e membership.Enforcement) enforcementView {
	view := enforcementView{
		MembershipID: e.MembershipID, EventID: e.EventID, Transition: string(e.Transition),
		AcceptedAt: e.AcceptedAt, PublishedAt: e.PublishedAt,
		BudgetSeconds: int(e.Budget / time.Second), State: string(e.State), EvaluatedAt: e.EvaluatedAt,
		Consumers: make([]consumerEvidenceView, 0, len(e.Consumers)),
	}
	for _, c := range e.Consumers {
		view.Consumers = append(view.Consumers, consumerEvidenceView{
			ConsumerID: c.ConsumerID, Evidence: c.Evidence, RecordedAt: c.RecordedAt})
	}
	return view
}
