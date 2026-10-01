package httpapi

// The third authority: a registered projection consumer.
//
// It exists because the two context checks were gated behind provider authority, which meant a
// product performing a fresh check had to hold the authority to administer every Tenant. A consumer
// is now a workload Principal registered by principal_id (ADR-ORG-001 §5.11); the token half is in
// authentication_test.go. These assert the scope half, because the failure mode of getting it wrong
// is a privilege that works — nothing is refused, so nothing reports it.

import (
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"
)

const testSubject = "01a05800-0000-7000-8000-00000000000c"

// TestAConsumerCallerResolvesToAConsumerScope is the transport half of the narrowing. A consumer
// reads across Tenants, and it used to receive the provider scope for that, which ran it as the
// provider role. It now receives a scope of its own, which only the consumer pool opens.
func TestAConsumerCallerResolvesToAConsumerScope(t *testing.T) {
	scope, err := resolve(Caller{Subject: mustParse(t, testSubject), Consumer: testConsumerName},
		mustParse(t, "01a05800-0000-7000-8000-0000000000c1"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !scope.IsConsumer() || scope.IsProvider() {
		t.Errorf("a consumer caller resolved to consumer=%v provider=%v, want a consumer scope only",
			scope.IsConsumer(), scope.IsProvider())
	}
}

// TestAConsumerCallerCarryingAnotherAuthorityIsRefusedAtScope keeps the rule from depending on
// callerFromClaims being the only place a Caller is built — tests and future middleware build them
// too.
func TestAConsumerCallerCarryingAnotherAuthorityIsRefusedAtScope(t *testing.T) {
	correlation := mustParse(t, "01a05800-0000-7000-8000-0000000000c1")
	subject := mustParse(t, testSubject)

	if _, err := resolve(Caller{Subject: subject, Consumer: "c", Provider: true}, correlation); err == nil {
		t.Error("a consumer caller with provider authority resolved to a scope")
	}
	if _, err := resolve(Caller{
		Subject:  subject,
		Consumer: "c",
		Tenant:   mustParse(t, "11111111-1111-4111-8111-11111111111a"),
	}, correlation); err == nil {
		t.Error("a consumer caller with a Tenant resolved to a scope")
	}
}

func mustParse(t *testing.T, value string) id.UUID {
	t.Helper()
	parsed, err := id.Parse(value)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return parsed
}
