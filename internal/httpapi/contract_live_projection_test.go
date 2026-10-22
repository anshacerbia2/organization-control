package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	fevent "github.com/anshacerbia2/foundation-platform/event"
)

// A projection consumer registered, bootstrapped, reporting progress, reconciling and asking the
// fresh checks, its dead letters replayed, resolved and waived, and the consumer retired.
func TestLiveProjectionConsumer(t *testing.T) {
	l := liveSurface(t)
	provider := l.emergencyProvider()
	name := fmt.Sprintf("contract-%d", time.Now().UnixNano())
	principal := mustID(t)

	registered := l.call(provider, http.MethodPost, "/v1/projections/consumers", map[string]any{
		"consumer_id": name, "principal_id": principal.String(), "projection_version": "v1",
		"max_accepted_age_seconds": 300, "stale_behavior": "fail_closed",
		"event_types": []string{"com.scnehaux.organization.membership.security.revoked", "com.scnehaux.organization.provider.lifecycle.granted"},
	}, http.StatusCreated)
	if str(t, registered, "consumer_id") != name {
		t.Fatalf("registered %v", registered)
	}
	// Retired by the test's last exchange; retired here too when the test stops before it, so a failed
	// run leaves no subscriber on the shared database's later suites' events.
	t.Cleanup(func() {
		l.exec(`UPDATE platform.subscription SET retired_at = coalesce(retired_at, now()) WHERE consumer = $1`, name)
		l.exec(`UPDATE projection.consumer SET retired_at = coalesce(retired_at, now()) WHERE consumer_id = $1`, name)
	})
	consumerPath := "/v1/projections/consumers/" + name
	consumer := Caller{Subject: principal, Consumer: name}
	l.call(provider, http.MethodGet, "/v1/projections/consumers?limit=2", nil, http.StatusOK)
	l.call(consumer, http.MethodGet, consumerPath, nil, http.StatusOK)
	frontier := l.call(consumer, http.MethodGet, "/v1/projections/frontier", nil, http.StatusOK)
	mark := frontier["highest_committed_mark"]
	l.call(consumer, http.MethodPost, consumerPath+"/bootstrap", map[string]any{"mark": mark}, http.StatusOK)
	l.call(consumer, http.MethodPost, consumerPath+"/progress", map[string]any{"applied_mark": mark}, http.StatusOK)
	l.call(consumer, http.MethodPost, "/v1/projections/snapshot", map[string]any{"consumer_id": name, "page_size": 2}, http.StatusOK)
	l.call(consumer, http.MethodPost, "/v1/projections/provider-authority/snapshot", map[string]any{"consumer_id": name, "page_size": 2}, http.StatusOK)
	rows := []map[string]any{{"membership_id": fixtureMembership, "membership_version": 1}}
	l.call(consumer, http.MethodPost, "/v1/projections/reconcile", map[string]any{"consumer_id": name, "mark": mark, "rows": rows}, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/projections/advance-versions", map[string]any{"consumer_id": name, "mark": mark, "rows": rows}, http.StatusOK)

	asked := map[string]any{"tenant_id": fixtureTenant, "principal_id": fixturePrincipal}
	l.call(consumer, http.MethodPost, "/v1/context/verify", asked, http.StatusOK)
	l.call(consumer, http.MethodPost, "/v1/context/switch-eligible", asked, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/context/rate", map[string]any{"consumer_id": name, "requests": 1}, http.StatusOK)
	l.call(provider, http.MethodGet, "/v1/context/rate/over-threshold?threshold=1", nil, http.StatusOK)
	l.call(provider, http.MethodGet, "/v1/principals/"+fixturePrincipal+"/contexts", nil, http.StatusOK)

	replayed, resolved, waived := l.deadLetter(name), l.deadLetter(name), l.deadLetter(name)
	l.call(provider, http.MethodGet, consumerPath+"/dead-letters?limit=2", nil, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/dead-letters/"+replayed+"/consumers/"+name+"/replay", nil, http.StatusAccepted)
	// The consumer's applied-evidence receipt, which justifies closing the incident.
	l.exec(`INSERT INTO platform.delivery_receipt (event_id, consumer, event_type, evidence)
	    VALUES ($1::uuid, $2, 'com.scnehaux.organization.membership.security.revoked', 'consumer_applied')`, resolved, name)
	l.call(provider, http.MethodPost, "/v1/dead-letters/"+resolved+"/consumers/"+name+"/resolve", map[string]any{}, http.StatusOK)
	l.call(provider, http.MethodPost, consumerPath+"/retire", nil, http.StatusNoContent)
	l.call(provider, http.MethodPost, "/v1/dead-letters/"+waived+"/consumers/"+name+"/waive", map[string]any{
		"reason": "contract test", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, http.StatusOK)
}

// deadLetter files a delivery to consumer the dispatcher abandoned, and its incident, as
// internal/projection's replay test seeds them.
func (l *live) deadLetter(consumer string) string {
	l.t.Helper()
	envelope, err := fevent.New("//scnehaux.com/organization-control",
		"com.scnehaux.organization.membership.security.revoked", time.Now().UTC(),
		map[string]any{"membership_id": mustID(l.t).String(), "tenant_id": mustID(l.t).String(),
			"principal_id": mustID(l.t).String(), "membership_version": 4})
	if err != nil {
		l.t.Fatalf("mint an envelope: %v", err)
	}
	wire, err := json.Marshal(envelope)
	if err != nil {
		l.t.Fatalf("encode the envelope: %v", err)
	}
	aggregate := mustID(l.t).String()
	l.exec(`WITH e AS (
	        INSERT INTO platform.outbox (event_id, aggregate_id, event_type, payload, envelope, priority)
	        VALUES ($1::uuid, $2::uuid, $3, '{}'::jsonb, $4::jsonb, 0)
	        RETURNING created_at, event_id, sequence, event_type, priority)
	    INSERT INTO platform.outbox_delivery
	        (created_at, event_id, consumer, sequence, event_type, priority,
	         published, published_at, attempts, first_failed_at, failure_class, last_error)
	    SELECT created_at, event_id, $5, sequence, event_type, priority,
	           TRUE, NULL, 3, clock_timestamp(), 'poison', 'refused'
	      FROM e`, envelope.ID.String(), aggregate, envelope.Type.String(), string(wire), consumer)
	l.exec(`INSERT INTO platform.dead_letter
	        (event_id, event_type, envelope, payload, aggregate_id, priority,
	         failure_class, failure_detail, attempts, first_failed_at, consumer)
	    VALUES ($1::uuid, $2, $3::jsonb, '{}'::jsonb, $4::uuid, 0,
	            'poison', 'the consumer refused the envelope', 3, clock_timestamp(), $5)`,
		envelope.ID.String(), envelope.Type.String(), string(wire), aggregate, consumer)
	l.t.Cleanup(func() {
		for _, table := range []string{"platform.dead_letter", "platform.delivery_receipt", "platform.outbox_delivery"} {
			l.exec(`DELETE FROM `+table+` WHERE event_id = $1`, envelope.ID.String())
		}
	})
	return envelope.ID.String()
}

// The Tenant, Membership and Principal scripts/ci-fixture.sql seeds as Tenant A.
const (
	fixtureTenant     = "11111111-1111-4111-8111-11111111111a"
	fixtureMembership = "22222222-2222-4222-8222-22222222222a"
	fixturePrincipal  = "33333333-3333-4333-8333-33333333333a"
)
