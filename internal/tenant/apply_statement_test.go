package tenant

import (
	"strings"
	"testing"
)

// assemble builds the UPDATE a rule implies. It is the drift guard for the written-out constants in
// service.go: the parts that vary between actions -- the security-version increment and the
// lifecycle timestamp -- are the parts a hand-written statement could forget, and the omission
// reads as a missing line rather than as a wrong one.
func assemble(r rule) string {
	sets := []string{"status = $2", "version = version + 1", "updated_at = now()"}
	if r.securityVersion {
		sets = append(sets, "tenant_security_version = tenant_security_version + 1")
	}
	if r.stamp != "" {
		sets = append(sets, r.stamp+" = $3")
	}
	if r.clear != "" {
		sets = append(sets, r.clear+" = NULL")
	}
	return `UPDATE tenant.tenant SET ` + strings.Join(sets, ", ") +
		` WHERE tenant_id = $1 RETURNING version, tenant_security_version`
}

// TestEveryTransitionStatementMatchesItsRule walks the whole state machine, so an action added to
// `transitions` without a statement, or a statement edited away from its rule, fails here.
func TestEveryTransitionStatementMatchesItsRule(t *testing.T) {
	for action, r := range transitions {
		got := applyStatement(action)
		if got == "" {
			t.Errorf("%s has no statement", action)
			continue
		}
		if want := assemble(r); got != want {
			t.Errorf("%s:\n got  %s\n want %s", action, got, want)
		}
	}
}
