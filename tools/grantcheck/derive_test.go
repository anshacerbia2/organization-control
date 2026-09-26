package main

// The derivation, against a fixture module shaped like this repository: the same wrapper names,
// the same declared boundaries, and one case for every way the matrix could be wrong. Each refusal
// class is here because a gate whose red has never been seen cannot be told from one that cannot
// turn red.

import (
	"strings"
	"testing"
)

func fixtureDerivation(t *testing.T) *Derivation {
	t.Helper()
	d, err := derive("testdata/fixture", "./cmd/organization-control")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	return d
}

func holds(d *Derivation, role, fragment string) bool {
	for sql := range d.ByRole[role] {
		if strings.Contains(sql, fragment) {
			return true
		}
	}
	return false
}

func TestEachStatementIsAttributedToTheRoleThatRunsIt(t *testing.T) {
	d := fixtureDerivation(t)

	cases := []struct {
		what     string
		fragment string
		role     string
	}{
		{"a statement in a resolution body", "UPDATE platform.dead_letter", resolutionRole},
		{"a statement reached through a function value the body captured", "platform.delivery_receipt", resolutionRole},
		{"evidence written on the caller's transaction", "INSERT INTO audit.privileged_access", resolutionRole},
		{"a statement passed as a parameter by every caller", "WHERE token_hash", tenantRole},
		{"a statement in a tenant body", "INSERT INTO membership.membership", tenantRole},
		{"a nested provider scope", "INSERT INTO tenant.tenant", providerRole},
		{"the wrapper's own binding", "set_config('app.tenant_id'", tenantRole},
		{"a declared boundary", "FROM platform.outbox", providerRole},
		{"the recorder boundary", "INSERT INTO audit.privileged_access", providerRole},
		{"the claim store boundary", "UPDATE platform.idempotency_key", tenantRole},
	}
	for _, c := range cases {
		if !holds(d, c.role, c.fragment) {
			t.Errorf("%s: %q is not attributed to %s", c.what, c.fragment, c.role)
		}
	}
}

// Attribution that would be wrong. Each of these is a grant the tool would demand of a role that
// never runs the statement, or -- worse -- a role it would clear to lose a grant it needs.
func TestNoStatementIsAttributedToARoleThatDoesNotRunIt(t *testing.T) {
	d := fixtureDerivation(t)

	cases := []struct {
		what     string
		fragment string
		role     string
	}{
		// Accept's tenant body calls provision, which opens its own provider scope.
		{"a nested scope's statement leaking into the outer role", "INSERT INTO tenant.tenant", tenantRole},
		// The recorder runs on its own connection, not on the wrapper that calls it.
		{"the recorder's connection leaking into the tenant role", "INSERT INTO audit.privileged_access", tenantRole},
		// Freshness calls the frontier reader inside a tenant body; the reader is a declared boundary.
		{"a boundary's statement leaking into the role that called it", "FROM platform.outbox", tenantRole},
		// withRecordedScope is shared by the provider and resolution wrappers; its body parameter
		// must not carry one wrapper's bodies into the other's role.
		{"a resolution body leaking into the provider role", "UPDATE platform.dead_letter", providerRole},
		{"a provider body leaking into the resolution role", "INSERT INTO tenant.tenant", resolutionRole},
	}
	for _, c := range cases {
		if holds(d, c.role, c.fragment) {
			t.Errorf("%s: %q is attributed to %s", c.what, c.fragment, c.role)
		}
	}
}

func TestEveryUnreadablePathIsAProblem(t *testing.T) {
	d := fixtureDerivation(t)
	all := strings.Join(d.Problems, "\n")

	for _, want := range []string{
		"internal/projection/projection.go:", // position of the dynamic statement
		"a SQL call whose statement is not a constant",
		"SQL reached from no scope wrapper or declared boundary, so no role is known for it: DELETE FROM workspace.workspace",
		"projection.Rogue).Do opens a raw transaction outside a scope wrapper and is not a declared boundary",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no problem mentions %q; got:\n%s", want, all)
		}
	}
	if strings.Contains(all, "declared but does not exist") {
		t.Errorf("the fixture declares every wrapper and boundary, yet one is reported stale:\n%s", all)
	}
}

func TestIsSQL(t *testing.T) {
	for text, want := range map[string]bool{
		"SELECT 1":                             true,
		"  -- a comment\n  UPDATE t SET a = 1": true,
		"WITH x AS (SELECT 1) SELECT * FROM x": true,
		"selection of rows":                    false,
		"SELECTED":                             false,
		"projection: reading receipts":         false,
		"":                                     false,
	} {
		if got := isSQL(text); got != want {
			t.Errorf("isSQL(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestSequenceCallsNameTheSequenceAndTheAcceptedPrivileges(t *testing.T) {
	calls := sequenceCalls(`WITH p AS (SELECT nextval('platform.outbox_sequence')::bigint) SELECT 1`)
	accepted := calls["platform.outbox_sequence"]
	if len(accepted) != 2 || accepted[0] != "USAGE" || accepted[1] != "UPDATE" {
		t.Errorf("nextval accepts %v, want [USAGE UPDATE]", accepted)
	}
	if len(sequenceCalls(`SELECT 1`)) != 0 {
		t.Error("a statement with no sequence call reported one")
	}
}
