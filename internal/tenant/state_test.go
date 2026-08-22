package tenant_test

import (
	"errors"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/tenant"
)

func mustUUID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value
}

var everyState = []tenant.State{
	tenant.StateRequested, tenant.StateProvisioning, tenant.StateActive, tenant.StateFailed,
	tenant.StateSuspended, tenant.StateOffboarding, tenant.StateRetired,
}

// TestEveryTransitionOutsideTheMachineIsRefused walks the whole cross product rather than a list of
// interesting cases.
//
// The permitted set is small and the refused set is not, so enumerating what is allowed and
// asserting everything else fails is the only shape that stays complete when the table grows. A
// test listing refusals by hand goes stale the moment an action is added.
func TestEveryTransitionOutsideTheMachineIsRefused(t *testing.T) {
	permitted := map[string]tenant.State{
		"provision|requested":         tenant.StateProvisioning,
		"provision|failed":            tenant.StateProvisioning,
		"fail|provisioning":           tenant.StateFailed,
		"activate|provisioning":       tenant.StateActive,
		"suspend|active":              tenant.StateSuspended,
		"restore|suspended":           tenant.StateActive,
		"begin-offboarding|active":    tenant.StateOffboarding,
		"begin-offboarding|suspended": tenant.StateOffboarding,
		"retire|offboarding":          tenant.StateRetired,
	}

	covered := map[string]bool{}
	for _, action := range tenant.Actions() {
		for _, from := range everyState {
			key := string(action) + "|" + string(from)
			next, err := tenant.Resolve(action, from)

			want, allowed := permitted[key]
			if allowed {
				covered[key] = true
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

	// The permitted map is the specification here, so a stale entry in it must fail rather than
	// pass silently. Without this, deleting a transition from the machine would leave this test
	// green with one fewer assertion than it appears to make.
	for key := range permitted {
		if !covered[key] {
			t.Errorf("%q is expected to be permitted and is not in the state machine", key)
		}
	}
}

// TestRetiredIsTerminalAndSaysSo separates "not yet" from "never again". A retired Tenant's
// identifiers have been released to consumers as retired, and reviving one would make a downstream
// projection wrong in a way no reconciliation would detect.
func TestRetiredIsTerminalAndSaysSo(t *testing.T) {
	for _, action := range tenant.Actions() {
		if _, err := tenant.Resolve(action, tenant.StateRetired); !errors.Is(err, tenant.ErrRetired) {
			t.Errorf("%s from retired returned %v, want ErrRetired", action, err)
		}
	}
}

func TestUnknownActionIsAProgrammingError(t *testing.T) {
	if _, err := tenant.Resolve(tenant.Action("archive"), tenant.StateActive); !errors.Is(err, tenant.ErrUnknownAction) {
		t.Errorf("Resolve error = %v, want ErrUnknownAction", err)
	}
	if _, _, err := tenant.EventType(tenant.Action("archive")); !errors.Is(err, tenant.ErrUnknownAction) {
		t.Errorf("EventType error = %v, want ErrUnknownAction", err)
	}
}

// TestEveryStateIsReachableAndNothingIsOrphaned is what a table buys over a switch.
//
// A state declared in the machine and reachable by no transition is either dead or a transition
// somebody forgot to add, and the two look identical in a code review. Walking the table settles
// it. `requested` is excluded as the entry state: a Tenant is created in it.
func TestEveryStateIsReachableAndNothingIsOrphaned(t *testing.T) {
	reachable := map[tenant.State]bool{tenant.StateRequested: true}
	for _, action := range tenant.Actions() {
		for _, from := range everyState {
			if next, err := tenant.Resolve(action, from); err == nil {
				reachable[next] = true
			}
		}
	}
	for _, state := range everyState {
		if !reachable[state] {
			t.Errorf("state %s is declared and no transition reaches it", state)
		}
		if !state.Valid() {
			t.Errorf("state %s is in the machine and Valid() rejects it", state)
		}
	}
}

// TestTheSecurityVersionIncrementsExactlyWhereContextIsInvalidated is
// TDD-organization-control-003 §"Security Version Increments" as an assertion.
//
// Both directions matter. A consumer that cached "suspended" and never sees a version change keeps
// denying a Tenant that has been restored, and that symptom arrives as a support ticket rather than
// as a projection failure — which is why restore increments too.
func TestTheSecurityVersionIncrementsExactlyWhereContextIsInvalidated(t *testing.T) {
	want := map[tenant.Action]bool{
		tenant.ActionProvision:        false, // no context existed to invalidate
		tenant.ActionFail:             false, // likewise
		tenant.ActionActivate:         false, // likewise
		tenant.ActionSuspend:          true,  // every cached context is now invalid
		tenant.ActionRestore:          true,  // every cached denial is now wrong
		tenant.ActionBeginOffboarding: true,  // context is frozen
		tenant.ActionRetire:           true,  // every context is permanently invalid
	}

	for _, action := range tenant.Actions() {
		expected, known := want[action]
		if !known {
			t.Errorf("action %q is in the machine and not in this test; decide whether it invalidates context", action)
			continue
		}
		if got := tenant.IncrementsSecurityVersion(action); got != expected {
			t.Errorf("IncrementsSecurityVersion(%s) = %v, want %v", action, got, expected)
		}
	}
}

// TestEveryActionEitherPublishesOrIsDeclaredSilent closes the gap between "publishes nothing on
// purpose" and "publishes nothing because nobody noticed".
//
// An action in neither set would return an error from EventType at the moment it was first
// exercised, which for a lifecycle transition means in production rather than here.
func TestEveryActionEitherPublishesOrIsDeclaredSilent(t *testing.T) {
	silent := map[tenant.Action]bool{
		tenant.ActionProvision: true,
		tenant.ActionFail:      true,
	}

	for _, action := range tenant.Actions() {
		eventType, publishes, err := tenant.EventType(action)
		if err != nil {
			t.Errorf("EventType(%s): %v", action, err)
			continue
		}
		if publishes == silent[action] {
			t.Errorf("%s publishes = %v and is declared silent = %v; exactly one must hold",
				action, publishes, silent[action])
		}
		if publishes && eventType == "" {
			t.Errorf("%s publishes and carries no type", action)
		}
		if !publishes && eventType != "" {
			t.Errorf("%s is silent and carries type %q", action, eventType)
		}
	}
}

// TestTheLaneAgreesWithTheEventName asserts the transport and the name cannot disagree.
//
// An event named `security` that travelled the standard lane would tell a consumer it is urgent
// while sitting behind a lifecycle backlog — and the consumer has no way to see the difference.
func TestTheLaneAgreesWithTheEventName(t *testing.T) {
	for _, action := range tenant.Actions() {
		eventType, publishes, err := tenant.EventType(action)
		if err != nil {
			t.Fatalf("EventType(%s): %v", action, err)
		}

		priority := tenant.Priority(action)
		if !publishes {
			if priority {
				t.Errorf("%s publishes nothing and claims the priority lane", action)
			}
			continue
		}

		isSecurity := segment(string(eventType), 4) == "security"
		if priority != isSecurity {
			t.Errorf("%s publishes %q and takes the priority lane = %v", action, eventType, priority)
		}
	}
}

// TestSuspensionAndTheOffboardingFreezeShareOneConsequenceEvent is
// TDD-organization-control-003 §"Published Events" as written, and it is deliberate rather than a
// copy-paste: the event is the security consequence, not a mirror of the state name. Both stop
// every existing Tenant context, and a consumer that must tell them apart reads the status out of
// the payload.
func TestSuspensionAndTheOffboardingFreezeShareOneConsequenceEvent(t *testing.T) {
	suspend, _, err := tenant.EventType(tenant.ActionSuspend)
	if err != nil {
		t.Fatalf("EventType: %v", err)
	}
	offboard, _, err := tenant.EventType(tenant.ActionBeginOffboarding)
	if err != nil {
		t.Fatalf("EventType: %v", err)
	}
	if suspend != offboard {
		t.Errorf("suspend publishes %q and the offboarding freeze publishes %q", suspend, offboard)
	}
	if !tenant.Priority(tenant.ActionSuspend) || !tenant.Priority(tenant.ActionBeginOffboarding) {
		t.Error("a transition that stops every context in a Tenant is not on the priority lane")
	}
}

// TestRetirementIsNotOnThePriorityLane records a decision that looks like an inconsistency and is
// not, so a later reader does not "fix" it.
//
// Retirement increments the security version and travels the standard lane. The only way into
// retired is from offboarding, which already published a priority event and already froze context,
// so by the time a Tenant retires there is no access left to withdraw. The urgency was discharged
// one transition earlier.
func TestRetirementIsNotOnThePriorityLane(t *testing.T) {
	if !tenant.IncrementsSecurityVersion(tenant.ActionRetire) {
		t.Error("retirement does not increment the security version")
	}
	if tenant.Priority(tenant.ActionRetire) {
		t.Error("retirement takes the priority lane; see this test's comment before changing it")
	}
	if _, err := tenant.Resolve(tenant.ActionRetire, tenant.StateActive); err == nil {
		t.Error("an active Tenant retires without offboarding, which is what makes the lane above wrong")
	}
}

// TestPayloadCarriesBothVersions is what makes out-of-order delivery safe.
//
// The two versions answer different questions: one orders two events about this row, the other
// decides whether a held token is stale. Carrying only the security version would leave a pair of
// events with the same value and no ordering between them, because a transition that does not
// increment it publishes the previous value again.
func TestPayloadCarriesBothVersions(t *testing.T) {
	payload := tenant.NewPayload(tenant.Tenant{
		TenantID:        mustUUID(t),
		OrganizationID:  mustUUID(t),
		Status:          tenant.StateSuspended,
		Version:         9,
		SecurityVersion: 4,
	})

	if payload.TenantVersion != 9 || payload.TenantSecurityVersion != 4 {
		t.Errorf("versions = (%d, %d), want (9, 4)", payload.TenantVersion, payload.TenantSecurityVersion)
	}
	if payload.TenantStatus != tenant.StateSuspended {
		t.Errorf("status = %s", payload.TenantStatus)
	}
	if payload.TenantID.IsNil() || payload.OrganizationID.IsNil() {
		t.Error("the payload omits an identifier a consumer keys on")
	}
}

func segment(value string, index int) string {
	parts := []string{}
	current := ""
	for _, r := range value {
		if r == '.' {
			parts = append(parts, current)
			current = ""
			continue
		}
		current += string(r)
	}
	parts = append(parts, current)
	if index >= len(parts) {
		return ""
	}
	return parts[index]
}
