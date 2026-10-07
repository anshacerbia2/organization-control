package httpapi

// Which routes require an `Idempotency-Key` (TDD-organization-control-003 1.10.0 §The
// Idempotency-Key Is Required on Commands).
//
// A command -- a request a person or an operator sends to change authoritative state -- must carry
// one. A retried command without a key is a second command: a second Workspace, a second grant, a
// second offboarding begun. The IETF draft that standardises the header says what to answer: "If the
// `Idempotency-Key` request header is missing for a documented idempotent operation requiring this
// header, the resource SHOULD reply with an HTTP `400` status code" (draft-ietf-httpapi-idempotency-
// key-header-07 §2.7).
//
// Every other POST on the API surface is named in keyOptional with the reason the key is honoured
// there and not required. Routes registers each POST either through command or as a keyOptional
// pattern, and TestEveryPostRouteIsClassified fails on one that is neither, so a new route cannot
// be added without deciding.

import (
	stdcontext "context"
	"net/http"
	"strings"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
)

type keyRequiredKey struct{}

// command marks a route whose request must carry an Idempotency-Key.
//
// The refusal itself is made by the caller check every handler opens with -- requireTenant,
// requireProvider, requireGrantHolder -- after the caller's authority and reason are checked and
// before anything is decoded or read. A wrong caller is told it is the wrong caller rather than that
// a header is missing, and no command reaches a transaction without its key.
func command(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(stdcontext.WithValue(r.Context(), keyRequiredKey{}, true)))
	}
}

// missingKey is the 400 detail. It says what the key is for, because a client that only learns the
// header is missing tends to send a constant -- which turns every later command into a replay of the
// first one.
const missingKey = "This route changes state and requires an " + IdempotencyHeader + " header: a value " +
	"unique to this command, sent again unchanged when the same command is retried"

// requireKey refuses a command that carries no Idempotency-Key. A blank value is no key: the claim
// would not be made, and the caller would believe its retries were safe.
func requireKey(w http.ResponseWriter, r *http.Request) bool {
	if required, _ := r.Context().Value(keyRequiredKey{}).(bool); !required {
		return true
	}
	if strings.TrimSpace(r.Header.Get(IdempotencyHeader)) != "" {
		return true
	}
	platform.Problem(w, r, platform.ValidationFailed, missingKey)
	return false
}

// keyOptional names every POST on the API surface on which an Idempotency-Key is honoured and not
// required, with the reason. None of them is a person's command: each is a read carried in a POST
// body, a report from a machine whose payload carries its own identity, a sweep, or a dead-letter act
// whose design (TDD-organization-control-005) keeps the key optional.
var keyOptional = map[string]string{
	"POST /v1/projections/snapshot": "a read: one page of the projection, in a POST because the " +
		"continuation carries the mark; it changes nothing",
	"POST /v1/projections/provider-authority/snapshot": "a read, as the Organization snapshot",
	"POST /v1/context/verify":                          "a read: the authoritative fresh check",
	"POST /v1/context/switch-eligible":                 "a read: whether a context switch is permitted",
	"POST /v1/context/rate": "a consumer's measurement report; a repeat closes an empty interval, " +
		"and the meter is a signal, not authority",
	"POST /v1/projections/consumers/{consumer_id}/progress": "a consumer's position report: the same " +
		"position is accepted again by design (TDD-organization-control-002 §Projection)",
	"POST /v1/projections/consumers/{consumer_id}/bootstrap": "a consumer's snapshot mark: it moves " +
		"forward only, so the same mark sent again changes nothing",
	"POST /v1/projections/reconcile": "a comparison: a repeat against the same report finds the same " +
		"findings, and the repair it publishes is applied by version (TDD-organization-control-002 " +
		"§Reconciliation)",
	"POST /v1/provisioning/realized": "the provisioning system's report, identified by its " +
		"correlation identifier: a duplicate is answered as a replay (TDD-organization-control-003)",
	"POST /v1/provisioning/failed": "as realized",
	"POST /v1/offboardings/{offboarding_id}/deprovisioning": "the provisioning system's report for the " +
		"other direction: the same outcome sent again records the same state",
	"POST /v1/provisioning/sweep-unresolved": "a sweep: a repeat finds nothing left to age",
	"POST /v1/invitations/expire-lapsed":     "a sweep: a repeat finds nothing left to expire",
	"POST /v1/dead-letters/{event_id}/consumers/{consumer}/replay": "honoured, not required, by " +
		"TDD-organization-control-005: a replay re-sends a delivery the consumer deduplicates by event_id",
	"POST /v1/dead-letters/{event_id}/consumers/{consumer}/resolve": "honoured, not required, by " +
		"TDD-organization-control-005: a second resolve is refused 409 by the incident's own state",
	"POST /v1/dead-letters/{event_id}/consumers/{consumer}/waive": "as resolve: a second waiver is " +
		"refused 409",
}
