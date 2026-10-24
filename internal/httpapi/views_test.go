package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/membership"
)

// A Membership's absent Workspace and end are written as null, never as the nil UUID or
// 0001-01-01T00:00:00Z; a present one is written as itself (STD-GLB-001 2.0.0, ROADMAP item 67).
func TestMembershipViewWritesAbsentValuesAsNull(t *testing.T) {
	bare, err := json.Marshal(viewMembership(membership.Membership{
		MembershipID: mustID(t), PrincipalID: mustID(t), TenantID: mustID(t), ValidFrom: time.Now(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"workspace_id":null`, `"valid_until":null`} {
		if !strings.Contains(string(bare), want) {
			t.Errorf("want %s in %s", want, bare)
		}
	}
	for _, sentinel := range []string{id.Nil.String(), "0001-01-01T00:00:00Z"} {
		if strings.Contains(string(bare), sentinel) {
			t.Errorf("a sentinel %s in %s", sentinel, bare)
		}
	}

	workspace, end := mustID(t), time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	scoped, err := json.Marshal(viewMembership(membership.Membership{WorkspaceID: workspace, ValidUntil: end}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"workspace_id":"` + workspace.String() + `"`, `"valid_until":"2027-01-02T03:04:05Z"`} {
		if !strings.Contains(string(scoped), want) {
			t.Errorf("want %s in %s", want, scoped)
		}
	}
}
