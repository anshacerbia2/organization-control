package httpapi

// The frontier's dead-letter debt is scoped to a copied list of event types, and this is where the
// copy is kept honest.
//
// projection.AuthorityEventTypes names the published events a Membership projection's authority
// depends on: the Membership state machine's and the Tenant state machine's. internal/projection may
// not import either to derive them: arch.json gives that package edges to internal/db and
// internal/system only, because a read-only publisher with a path into a state machine is a mutation
// path sitting behind a snapshot.
//
// So the list is a copy, and a copy that drifts is worse than no list at all -- a transition added to
// either state machine would publish an authority-bearing event whose dead letter the frontier
// silently declines to count, and every consumer would read "no debt" while a withdrawal sat
// unresolved. This package already imports all three, so this is where they meet.

import (
	"testing"

	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
)

func TestTheFrontierDebtCoversEveryAuthorityEvent(t *testing.T) {
	counted := make(map[string]bool, len(projection.AuthorityEventTypes))
	for _, eventType := range projection.AuthorityEventTypes {
		if counted[eventType] {
			t.Errorf("%q is listed twice, so the debt count would be right by accident", eventType)
		}
		counted[eventType] = true
	}

	published := map[string]bool{}
	for _, action := range membership.Actions() {
		eventType, err := membership.EventType(action)
		if err != nil {
			t.Fatalf("membership.EventType(%q): %v", action, err)
		}
		published[string(eventType)] = true
		if !counted[string(eventType)] {
			t.Errorf("the Membership state machine publishes %q on %q and the frontier does not "+
				"count its dead letters: a withdrawal could sit unresolved while every consumer "+
				"reads no debt", eventType, action)
		}
	}

	// Every Tenant transition that publishes. The silent ones -- the provisioning transitions --
	// publish nothing a consumer could miss.
	for _, action := range tenant.Actions() {
		eventType, publishes, err := tenant.EventType(action)
		if err != nil {
			t.Fatalf("tenant.EventType(%q): %v", action, err)
		}
		if !publishes {
			continue
		}
		published[string(eventType)] = true
		if !counted[string(eventType)] {
			t.Errorf("the Tenant state machine publishes %q on %q and the frontier does not count "+
				"its dead letters: a Tenant suspension could sit unresolved while every consumer "+
				"reads no debt and serves the suspended Tenant's members", eventType, action)
		}
	}

	// And the other direction. An event type listed here that nothing publishes is not harmless: it
	// is a line that looks like coverage, and it hides that the transition it was written for was
	// renamed rather than removed.
	for _, eventType := range projection.AuthorityEventTypes {
		if !published[eventType] {
			t.Errorf("the frontier counts dead letters for %q and no Membership or Tenant action "+
				"publishes it", eventType)
		}
	}
}
