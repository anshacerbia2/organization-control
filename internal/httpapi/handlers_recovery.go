package httpapi

// The operator's recovery routes: the dead-letter list (TDD-organization-control-005 2.5.0), sending
// a failed deprovisioning again (TDD-organization-control-004 1.11.0), the pause of Tenant
// administration (TDD-organization-control-001 1.22.0) and the version advance after a restore to an
// older point (TDD-organization-control-002 1.15.0). Each is a provider route with
// X-Administrative-Reason; the reason is the access record's, and for the pause the decision's own.

import (
	stdcontext "context"
	"net/http"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/projection"
)

type deadLetterView struct {
	EventID             string     `json:"event_id"`
	Consumer            string     `json:"consumer"`
	EventType           string     `json:"event_type"`
	AuthorityBearing    bool       `json:"authority_bearing"`
	AggregateID         *string    `json:"aggregate_id"`
	Version             *int64     `json:"version"`
	Lane                *string    `json:"lane"`
	FailureClass        string     `json:"failure_class"`
	FailureDetail       string     `json:"failure_detail"`
	Attempts            int        `json:"attempts"`
	FirstFailedAt       time.Time  `json:"first_failed_at"`
	DeadLetteredAt      time.Time  `json:"dead_lettered_at"`
	ResolvedAt          *time.Time `json:"resolved_at"`
	ResolutionType      *string    `json:"resolution_type"`
	ResolvedBy          *string    `json:"resolved_by"`
	ResolutionReference *string    `json:"resolution_reference"`
	WaivedAt            *time.Time `json:"waived_at"`
	WaivedUntil         *time.Time `json:"waived_until"`
	WaivedBy            *string    `json:"waived_by"`
	WaiverReason        *string    `json:"waiver_reason"`
}

type deadLetterPageView struct {
	DeadLetters []deadLetterView `json:"dead_letters"`
	Next        *string          `json:"next"`
}

func viewDeadLetter(d projection.DeadLetter) deadLetterView {
	view := deadLetterView{
		EventID: d.EventID.String(), Consumer: d.Consumer, EventType: d.EventType,
		AuthorityBearing: d.AuthorityBearing, Version: d.Version, Lane: d.Priority,
		FailureClass: d.FailureClass, FailureDetail: d.FailureDetail, Attempts: d.Attempts,
		FirstFailedAt: d.FirstFailedAt, DeadLetteredAt: d.DeadLettered,
		ResolvedAt: d.ResolvedAt, ResolutionType: d.ResolutionType, ResolvedBy: d.ResolvedBy,
		ResolutionReference: d.ResolutionReference, WaivedAt: d.WaivedAt, WaivedUntil: d.WaivedUntil,
		WaivedBy: d.WaivedBy, WaiverReason: d.WaiverReason,
	}
	if d.AggregateID != nil {
		aggregate := d.AggregateID.String()
		view.AggregateID = &aggregate
	}
	return view
}

// listDeadLetters serves `GET /v1/projections/consumers/{consumer_id}/dead-letters`.
func (h *handlers) listDeadLetters(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"state"}, nil)
	if !ok {
		return
	}
	page, err := h.services.DeadLetters.List(r.Context(), projection.DeadLetterQuery{
		Consumer: r.PathValue("consumer_id"), After: params.After, Limit: params.Limit,
		State: projection.DeadLetterState(params.Filters["state"]),
	}, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := deadLetterPageView{DeadLetters: make([]deadLetterView, 0, len(page.DeadLetters))}
	for _, item := range page.DeadLetters {
		view.DeadLetters = append(view.DeadLetters, viewDeadLetter(item))
	}
	if page.Next != nil {
		next := page.Next.String()
		view.Next = &next
	}
	respond(w, http.StatusOK, view)
}

// resendDeprovisioning serves `POST /v1/offboardings/{offboarding_id}/deprovisioning/resend`.
func (h *handlers) resendDeprovisioning(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (offboarding.Offboarding, error) {
		return h.services.Offboardings.ResendDeprovisioning(ctx, offboardingID, reason(r))
	}, viewOffboarding)
}

type pauseView struct {
	Paused        bool       `json:"paused"`
	PauseID       *string    `json:"pause_id"`
	Reason        *string    `json:"reason"`
	ActorID       *string    `json:"actor_id"`
	CorrelationID *string    `json:"correlation_id"`
	RecordedAt    *time.Time `json:"recorded_at"`
}

func viewPause(p authority.Pause) pauseView {
	view := pauseView{Paused: p.Paused, RecordedAt: p.RecordedAt}
	if !p.ID.IsNil() {
		pauseID, why := p.ID.String(), p.Reason
		view.PauseID, view.Reason = &pauseID, &why
	}
	for _, field := range []struct {
		from *id.UUID
		into **string
	}{{p.ActorID, &view.ActorID}, {p.CorrelationID, &view.CorrelationID}} {
		if field.from != nil {
			text := field.from.String()
			*field.into = &text
		}
	}
	return view
}

// tenantAdministrationPause serves `GET /v1/tenant-administration-pause`: the latest decision.
func (h *handlers) tenantAdministrationPause(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	pause, err := h.services.ProviderGrants.CurrentPause(r.Context(), reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewPause(pause))
}

type setPauseRequest struct {
	Paused *bool `json:"paused"`
}

// setTenantAdministrationPause serves `POST /v1/tenant-administration-pause` `{"paused": bool}`.
func (h *handlers) setTenantAdministrationPause(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[setPauseRequest](w, r)
	if !ok {
		return
	}
	if body.Paused == nil {
		platform.Problem(w, r, platform.ValidationFailed, "paused is required: true to pause, false to lift")
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (authority.Pause, error) {
		return h.services.ProviderGrants.SetPause(ctx, *body.Paused, reason(r))
	}, viewPause)
}

type advancedView struct {
	MembershipID     string  `json:"membership_id"`
	TenantID         string  `json:"tenant_id"`
	FromVersion      int64   `json:"from_version"`
	ToVersion        int64   `json:"to_version"`
	MembershipStatus string  `json:"membership_status"`
	EventID          *string `json:"event_id"`
}

type advanceView struct {
	ConsumerID string         `json:"consumer_id"`
	Mark       int64          `json:"mark"`
	Advanced   []advancedView `json:"advanced"`
}

// advanceVersions serves `POST /v1/projections/advance-versions`, with a consumer's report as its body.
//
// The report is reconciled first, and only the Memberships the consumer reports at a version above
// authority's, that authority still holds, are advanced (TDD-organization-control-002 1.15.0 §After a
// Restore to an Older Point). The rest of the findings are left to the ordinary reconciliation.
// Several Tenants mean several transactions, so the response is written after them rather than
// recorded inside one.
func (h *handlers) advanceVersions(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[reconcileRequest](w, r)
	if !ok {
		return
	}
	result, err := h.services.Reconciler.Reconcile(r.Context(), projection.Report{
		ConsumerID: body.ConsumerID, Mark: body.Mark, Rows: body.Rows,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	var advances []membership.Advance
	for _, finding := range result.Findings {
		if finding.State == nil || finding.ProjectedVersion <= finding.AuthoritativeVersion {
			continue
		}
		advances = append(advances, membership.Advance{
			MembershipID: finding.MembershipID, TenantID: finding.TenantID, Above: finding.ProjectedVersion,
		})
	}
	view := advanceView{ConsumerID: result.ConsumerID, Mark: result.Mark, Advanced: []advancedView{}}
	if len(advances) > 0 {
		done, err := h.services.Memberships.AdvanceVersions(r.Context(), advances, reason(r))
		if err != nil {
			writeError(w, r, err)
			return
		}
		for _, a := range done {
			item := advancedView{MembershipID: a.MembershipID.String(), TenantID: a.TenantID.String(),
				FromVersion: a.FromVersion, ToVersion: a.ToVersion, MembershipStatus: string(a.Status)}
			if a.EventID != nil {
				event := a.EventID.String()
				item.EventID = &event
			}
			view.Advanced = append(view.Advanced, item)
		}
	}
	respond(w, http.StatusOK, view)
}
