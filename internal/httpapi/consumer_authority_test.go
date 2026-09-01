package httpapi

// The third authority: a registered projection consumer.
//
// It exists because the two context checks were gated behind provider authority, which meant a
// product performing a fresh check had to hold the authority to administer every Tenant. These
// tests state the boundary as assertions, because the failure mode of getting it wrong is a
// privilege that works — nothing is refused, so nothing reports it.
//
// Tokens are signed and verified for real, for the reason authentication_test.go already gives:
// `verify.Claims` populates its non-registered claims only by decoding a token, so a stub could
// not carry the realm role or the consumer claim this mapping exists to read.

import (
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/verify"
)

const (
	testConsumerRole  = "projection-consumer"
	testConsumerClaim = "https://scnehaux.com/consumer_id"
	testConsumerName  = "foundation-reference"
	testSubject       = "01a05800-0000-7000-8000-00000000000c"
)

func consumerAuthConfig() AuthenticationConfig {
	return AuthenticationConfig{
		TenantClaim:   testClaim,
		ProviderRole:  testRole,
		ConsumerRole:  testConsumerRole,
		ConsumerClaim: testConsumerClaim,
	}
}

// consumerCaller signs the supplied claims, verifies them through a verifier configured for
// consumer authority, and returns the caller the mapping produced.
func consumerCaller(t *testing.T, cfg AuthenticationConfig, claims map[string]any) (Caller, error) {
	t.Helper()

	s := newSigner(t)
	verifier, err := verify.New(verify.Config{
		Issuer:   testIssuer,
		Audience: testAudience,
		Keys:     verify.StaticKeys{testKeyID: &s.key.PublicKey},
		// The requirement is the same mapping under test, so a token this configuration refuses is
		// refused at verification rather than reaching callerFromClaims. Both are refusals, and the
		// tests below assert on the pair rather than on which layer answered.
		Requirement: Requirement(cfg),
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	verified, err := verifier.Verify(s.sign(t, claims))
	if err != nil {
		return Caller{}, err
	}
	return callerFromClaims(verified, cfg)
}

func TestAConsumerTokenResolvesToAConsumerCaller(t *testing.T) {
	caller, err := consumerCaller(t, consumerAuthConfig(), map[string]any{
		"sub":             testSubject,
		"realm_access":    map[string]any{"roles": []string{testConsumerRole}},
		testConsumerClaim: testConsumerName,
	})
	if err != nil {
		t.Fatalf("a consumer token was refused: %v", err)
	}

	switch {
	case caller.Consumer != testConsumerName:
		t.Errorf("Consumer = %q, want %q", caller.Consumer, testConsumerName)
	case caller.Provider:
		t.Error("a consumer token produced a provider caller")
	case !caller.Tenant.IsNil():
		t.Errorf("a consumer caller carries Tenant %s", caller.Tenant)
	}
}

// TestAConsumerTokenNamingNoConsumerIsRefused is the assertion behind the meter: a consumer whose
// identity cannot be read is one whose fresh checks nobody can be held to, and metering them per
// consumer exists for attribution.
func TestAConsumerTokenNamingNoConsumerIsRefused(t *testing.T) {
	for _, value := range []string{"", "   "} {
		_, err := consumerCaller(t, consumerAuthConfig(), map[string]any{
			"sub":             testSubject,
			"realm_access":    map[string]any{"roles": []string{testConsumerRole}},
			testConsumerClaim: value,
		})
		if err == nil {
			t.Errorf("a consumer token naming %q was accepted", value)
		}
	}
}

// TestAConsumerTokenIsRefusedWhenTheAuthorityIsNotConfigured covers the deployment that has not
// opted in. With ConsumerRole unset the authority does not exist, and a token carrying the role
// name is an ordinary token conferring no scope.
func TestAConsumerTokenIsRefusedWhenTheAuthorityIsNotConfigured(t *testing.T) {
	_, err := consumerCaller(t, testAuthConfig(), map[string]any{
		"sub":             testSubject,
		"realm_access":    map[string]any{"roles": []string{testConsumerRole}},
		testConsumerClaim: testConsumerName,
	})
	if err == nil {
		t.Error("a consumer token was accepted by a deployment configuring no consumer authority")
	}
}

// TestConsumerAuthorityIsExclusive keeps the third authority as exclusive as the first two. A token
// carrying two authorities has two readings that differ in the permissive direction, and resolving
// to either silently means the caller and the service disagree about what the request did.
func TestConsumerAuthorityIsExclusive(t *testing.T) {
	if _, err := consumerCaller(t, consumerAuthConfig(), map[string]any{
		"sub":             testSubject,
		"realm_access":    map[string]any{"roles": []string{testConsumerRole, testRole}},
		testConsumerClaim: testConsumerName,
	}); err == nil {
		t.Error("a token carrying consumer and provider authority was accepted")
	}

	if _, err := consumerCaller(t, consumerAuthConfig(), map[string]any{
		"sub":             testSubject,
		"realm_access":    map[string]any{"roles": []string{testConsumerRole}},
		testConsumerClaim: testConsumerName,
		testClaim:         "11111111-1111-4111-8111-11111111111a",
	}); err == nil {
		t.Error("a token carrying consumer authority and a Tenant was accepted")
	}
}

// TestAConsumerCallerResolvesToAProviderScope records what is not yet fixed, so the debt lives in a
// test rather than only in a comment.
//
// A consumer reads across Tenants, so the isolation scope it receives today is the provider one.
// The narrowing is a database change: the fresh check reads two tables and writes one counter, so a
// role granted exactly those three privileges would fit. This assertion states the current truth,
// and it fails the day the narrowing lands — which is the point.
func TestAConsumerCallerResolvesToAProviderScope(t *testing.T) {
	scope, err := resolve(Caller{Subject: mustParse(t, testSubject), Consumer: testConsumerName},
		mustParse(t, "01a05800-0000-7000-8000-0000000000c1"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !scope.IsProvider() {
		t.Error("a consumer caller no longer resolves to a provider scope; update this test and ROADMAP.md")
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

func TestAConsumerRoleWithoutAClaimIsRefusedAtConstruction(t *testing.T) {
	s := newSigner(t)
	_, err := Authenticate(s.verifier(t), AuthenticationConfig{
		TenantClaim:  testClaim,
		ProviderRole: testRole,
		ConsumerRole: testConsumerRole,
	})
	if err == nil {
		t.Error("Authenticate accepted a consumer role with no claim naming the consumer")
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
