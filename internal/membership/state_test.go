package membership_test

import (
	"errors"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/membership"
)

// TestEveryTransitionOutsideTheMachineIsRefused walks the whole cross product rather than a list
// of interesting cases.
//
// The permitted set is small and the refused set is not, so enumerating what is allowed and
// asserting everything else fails is the only shape that stays complete when an action is added.
// A test listing refusals by hand goes stale the moment the table grows.
func TestEveryTransitionOutsideTheMachineIsRefused(t *testing.T) {
	states := []membership.State{membership.StateActive, membership.StateSuspended, membership.StateRevoked}

	permitted := map[string]membership.State{
		"suspend|active":    membership.StateSuspended,
		"revoke|active":     membership.StateRevoked,
		"revoke|suspended":  membership.StateRevoked,
		"restore|suspended": membership.StateActive,
	}

	for _, action := range membership.Actions() {
		if action == membership.ActionGrant {
			// Grant has no source state; it is asserted separately below.
			continue
		}
		for _, from := range states {
			key := string(action) + "|" + string(from)
			next, _, err := membership.Resolve(action, from)

			want, allowed := permitted[key]
			if allowed {
				if err != nil {
					t.Errorf("%s from %s was refused: %v", action, from, err)
				}
				if next != want {
					t.Errorf("%s from %s went to %s, want %s", action, from, next, want)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s from %s was permitted and went to %s", action, from, next)
			}
		}
	}

	// Grant produces an active Membership from no prior state.
	if next, _, err := membership.Resolve(membership.ActionGrant, ""); err != nil || next != membership.StateActive {
		t.Errorf("grant = (%s, %v), want (active, nil)", next, err)
	}
}

// TestRevokedIsTerminalAndSaysSo separates "not yet" from "never again".
//
// A caller retrying a suspension has a different problem from one retrying a revocation, and a
// single refusal error would make the two indistinguishable at the HTTP edge — where the first
// deserves a retry and the second deserves a different request entirely.
func TestRevokedIsTerminalAndSaysSo(t *testing.T) {
	for _, action := range []membership.Action{
		membership.ActionSuspend, membership.ActionRevoke, membership.ActionRestore,
	} {
		_, _, err := membership.Resolve(action, membership.StateRevoked)
		if !errors.Is(err, membership.ErrRevoked) {
			t.Errorf("%s from revoked returned %v, want ErrRevoked", action, err)
		}
	}
}

func TestUnknownActionIsAProgrammingError(t *testing.T) {
	if _, _, err := membership.Resolve(membership.Action("archive"), membership.StateActive); !errors.Is(err, membership.ErrUnknownAction) {
		t.Errorf("error = %v, want ErrUnknownAction", err)
	}
	if _, err := membership.EventType(membership.Action("archive")); !errors.Is(err, membership.ErrUnknownAction) {
		t.Errorf("EventType error = %v, want ErrUnknownAction", err)
	}
}

// TestOnlyAccessWithdrawalTakesThePriorityLane is the classification, asserted rather than trusted
// to the spelling of an event name.
//
// A grant arriving a minute late costs a retry. A revocation arriving a minute late is a minute of
// access after the decision to remove it, and ADR-GLB-003 §5 reserves a separate topic and
// consumer group for exactly that difference — so a lifecycle backlog cannot delay one.
func TestOnlyAccessWithdrawalTakesThePriorityLane(t *testing.T) {
	priority := map[membership.Action]bool{
		membership.ActionGrant:   false,
		membership.ActionRestore: false,
		membership.ActionSuspend: true,
		membership.ActionRevoke:  true,
	}

	for _, action := range membership.Actions() {
		want, known := priority[action]
		if !known {
			t.Errorf("action %q is in the state machine and not in this test; classify its lane", action)
			continue
		}
		if got := membership.Priority(action); got != want {
			t.Errorf("Priority(%s) = %v, want %v", action, got, want)
		}
	}
}

// TestEveryActionHasAValidEventType is the other half of the same concern. `event.ParseType`
// enforces the segment count and shape, so a malformed type would only surface when an event was
// published — which for the priority lane is during an incident.
func TestEveryActionHasAValidEventType(t *testing.T) {
	seen := map[string]membership.Action{}

	for _, action := range membership.Actions() {
		eventType, err := membership.EventType(action)
		if err != nil {
			t.Errorf("EventType(%s): %v", action, err)
			continue
		}
		if previous, duplicate := seen[string(eventType)]; duplicate {
			t.Errorf("%s and %s publish the same type %q", previous, action, eventType)
		}
		seen[string(eventType)] = action

		// The fifth segment carries the classification, and it must agree with the lane. An event
		// named `security` that travels the standard lane is the failure this catches: the name
		// tells a consumer it is urgent and the transport does not.
		wantClass := "lifecycle"
		if membership.Priority(action) {
			wantClass = "security"
		}
		if !containsSegment(string(eventType), wantClass) {
			t.Errorf("%s publishes %q, which does not classify as %s", action, eventType, wantClass)
		}
	}
}

func containsSegment(eventType, segment string) bool {
	for _, part := range splitDots(eventType) {
		if part == segment {
			return true
		}
	}
	return false
}

func splitDots(value string) []string {
	var parts []string
	current := ""
	for _, r := range value {
		if r == '.' {
			parts = append(parts, current)
			current = ""
			continue
		}
		current += string(r)
	}
	return append(parts, current)
}

// TestPayloadCarriesTheCompleteSecurityState is what makes out-of-order delivery safe.
//
// The priority lane may deliver a revocation before an older grant, so a consumer decides which
// desired state is newer by comparing versions. It cannot compare what the event did not carry,
// and a payload holding a delta would leave the consumer guessing from arrival order — which
// ADR-GLB-003 §5 states is not a guarantee the broker provides across partitions.
func TestPayloadCarriesTheCompleteSecurityState(t *testing.T) {
	workspace := mustUUID(t)
	record := membership.Membership{
		MembershipID: mustUUID(t),
		PrincipalID:  mustUUID(t),
		TenantID:     mustUUID(t),
		WorkspaceID:  workspace,
		Status:       membership.StateRevoked,
		Version:      14,
	}

	payload := membership.NewPayload(record, 3)

	if payload.MembershipVersion != 14 || payload.TenantSecurityVersion != 3 {
		t.Errorf("versions = (%d, %d), want (14, 3)", payload.MembershipVersion, payload.TenantSecurityVersion)
	}
	if payload.MembershipStatus != membership.StateRevoked {
		t.Errorf("status = %s", payload.MembershipStatus)
	}
	if payload.WorkspaceID == nil || *payload.WorkspaceID != workspace {
		t.Error("the Workspace is absent from the payload")
	}
	for _, empty := range []id.UUID{payload.MembershipID, payload.PrincipalID, payload.TenantID} {
		if empty.IsNil() {
			t.Error("the payload omits an identifier a consumer keys on")
		}
	}
}

// TestATenantWideMembershipSerialisesWorkspaceAsNull keeps two different facts distinguishable on
// the wire. A consumer telling "Tenant-wide" from "Workspace 00000000-…" needs the difference to
// survive marshalling, which a value type would not preserve.
func TestATenantWideMembershipSerialisesWorkspaceAsNull(t *testing.T) {
	payload := membership.NewPayload(membership.Membership{
		MembershipID: mustUUID(t),
		PrincipalID:  mustUUID(t),
		TenantID:     mustUUID(t),
		Status:       membership.StateActive,
		Version:      1,
	}, 1)

	if payload.WorkspaceID != nil {
		t.Errorf("a Tenant-wide Membership carries workspace_id %v, want null", *payload.WorkspaceID)
	}
}

func mustUUID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value
}
