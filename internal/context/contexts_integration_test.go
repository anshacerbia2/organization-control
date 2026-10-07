package context

// The context list against the real engine (ADR-ORG-005, TDD-organization-control-002 1.12.0): a
// person's own read runs on the tenant login as organization_self_rt and records nothing; a
// provider's runs on the provider login and records its reason.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// selfPool connects as the tenant login, organization_app, the connections a self read runs on.
func (f *fixture) selfPool(t *testing.T) *db.SelfPool {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	rest := base
	if index := strings.Index(base, "://"); index >= 0 {
		rest = base[index+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	conns, err := fdb.Open(f.ctx, fdb.Config{Name: "context-self-test",
		DSN: fmt.Sprintf("postgres://organization_app:%s@%s", os.Getenv("TEST_RUNTIME_PASSWORD"), rest), MaxConns: 2})
	if err != nil {
		t.Fatalf("open the tenant login: %v", err)
	}
	t.Cleanup(conns.Close)
	pool, err := db.NewSelfPool(conns)
	if err != nil {
		t.Fatalf("NewSelfPool: %v", err)
	}
	return pool
}

func (f *fixture) seedTenant(t *testing.T, status string) id.UUID {
	t.Helper()
	organizationID, tenantID := mustID(t), mustID(t)
	f.exec(t, `INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'contexts sponsor', 'customer', 'active')`, organizationID.String())
	f.exec(t, `INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, $3, $4, 'pooled')`, tenantID.String(), organizationID.String(), "contexts "+status, status)
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM membership.tenant_admin_grant WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM membership.membership WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM workspace.workspace WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})
	return tenantID
}

func (f *fixture) seedMembership(t *testing.T, principal, tenantID id.UUID, workspaceID *id.UUID, status string) id.UUID {
	t.Helper()
	membershipID := mustID(t)
	var workspace any
	if workspaceID != nil {
		workspace = workspaceID.String()
	}
	f.exec(t, `INSERT INTO membership.membership
	    (membership_id, principal_id, tenant_id, workspace_id, subject_type, status, membership_version, valid_from, provenance)
	    VALUES ($1, $2, $3, $4, 'human', $5, 1, now(), 'contexts suite')`,
		membershipID.String(), principal.String(), tenantID.String(), workspace, status)
	return membershipID
}

func TestAPersonListsTheirOwnContextsAndNoOneElses(t *testing.T) {
	f := newFixture(t)
	contexts, err := NewContexts(f.provider, f.selfPool(t))
	if err != nil {
		t.Fatalf("NewContexts: %v", err)
	}

	person, other := mustID(t), mustID(t)
	administered := f.seedTenant(t, "active")
	member := f.seedTenant(t, "active")
	suspendedTenant := f.seedTenant(t, "suspended")
	lapsed := f.seedTenant(t, "active")

	workspaceID := mustID(t)
	f.exec(t, `INSERT INTO workspace.workspace (workspace_id, tenant_id, display_name, workspace_type, status)
	    VALUES ($1, $2, 'contexts workspace', 'team', 'active')`, workspaceID.String(), member.String())

	first := f.seedMembership(t, person, administered, nil, "active")
	second := f.seedMembership(t, person, member, &workspaceID, "active")
	f.seedMembership(t, person, suspendedTenant, nil, "active")
	f.seedMembership(t, person, lapsed, nil, "suspended")
	otherMembership := f.seedMembership(t, other, administered, nil, "active")
	f.exec(t, `INSERT INTO membership.tenant_admin_grant (grant_id, tenant_id, principal_id, granted_by, reason)
	    VALUES ($1, $2, $3, $4, 'contexts suite')`, mustID(t).String(), administered.String(), person.String(), mustID(t).String())
	// A revoked grant confers nothing: the other person does not administer.
	f.exec(t, `INSERT INTO membership.tenant_admin_grant
	    (grant_id, tenant_id, principal_id, granted_by, reason, revoked_at, revoked_by, revoke_reason)
	    VALUES ($1, $2, $3, $4, 'contexts suite', now(), $4, 'left')`,
		mustID(t).String(), administered.String(), other.String(), mustID(t).String())

	selfScope, err := db.SelfScope(person, mustID(t))
	if err != nil {
		t.Fatalf("SelfScope: %v", err)
	}
	selfCtx := db.WithScope(f.ctx, selfScope)

	before := f.recorder.calls
	page, err := contexts.Own(selfCtx, ContextQuery{})
	if err != nil {
		t.Fatalf("Own: %v", err)
	}
	if f.recorder.calls != before {
		t.Errorf("a self read recorded %d privileged accesses, want none", f.recorder.calls-before)
	}
	if len(page.Contexts) != 2 || page.Next != nil {
		t.Fatalf("own contexts = %+v (next %v); want the two active Memberships in active Tenants", page.Contexts, page.Next)
	}
	if got := page.Contexts[0]; got.MembershipID != first || got.TenantID != administered || !got.Administers ||
		got.WorkspaceID != nil || got.TenantStatus != "active" || got.TenantDisplayName != "contexts active" {
		t.Errorf("the administered context reads %+v", got)
	}
	if got := page.Contexts[1]; got.MembershipID != second || got.Administers ||
		got.WorkspaceID == nil || *got.WorkspaceID != workspaceID {
		t.Errorf("the Workspace context reads %+v", got)
	}

	paged, err := contexts.Own(selfCtx, ContextQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Own limit 1: %v", err)
	}
	if len(paged.Contexts) != 1 || paged.Next == nil || *paged.Next != first {
		t.Fatalf("first page = %+v next %v; want one item and next %s", paged.Contexts, paged.Next, first)
	}
	rest, err := contexts.Own(selfCtx, ContextQuery{Limit: 1, After: *paged.Next})
	if err != nil {
		t.Fatalf("Own after: %v", err)
	}
	if len(rest.Contexts) != 1 || rest.Contexts[0].MembershipID != second || rest.Next != nil {
		t.Errorf("second page = %+v next %v; want %s and no next", rest.Contexts, rest.Next, second)
	}

	// The provider reads the other person's, with its reason recorded.
	before = f.recorder.calls
	theirs, err := contexts.Of(f.ctx, other, ContextQuery{}, "a support request")
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if f.recorder.calls != before+1 {
		t.Errorf("a provider read recorded %d accesses, want 1", f.recorder.calls-before)
	}
	if len(theirs.Contexts) != 1 || theirs.Contexts[0].MembershipID != otherMembership || theirs.Contexts[0].Administers {
		t.Errorf("the other person's contexts = %+v; want one, not administered (the grant is revoked)", theirs.Contexts)
	}

	// The self pool refuses a provider scope, and the provider path a self scope.
	if _, err := contexts.Own(f.ctx, ContextQuery{}); err == nil {
		t.Error("a provider scope opened the self read")
	}
	if _, err := contexts.Of(selfCtx, other, ContextQuery{}, "x"); err == nil {
		t.Error("a self scope read another person's contexts through the provider pool")
	}
}
