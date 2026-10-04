package authority

// Tenant administration grants (ADR-ORG-003), against the real engine as the real login roles: the
// provider role records nothing here and the tenant role does the work under the Tenant's policy, so
// an owning connection would pass for the wrong reason. The owner only arranges Tenants and
// Memberships and reads back what the roles wrote.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/membership"
)

type tenantAdminFixture struct {
	admin    *TenantAdministration
	records  *TenantRecords
	tenants  *db.TenantPool
	owner    *fdb.Pool
	ctx      context.Context // acting as the provider
	base     context.Context
	provider id.UUID
}

func tenantAdministration(t *testing.T) tenantAdminFixture {
	t.Helper()
	provider, owner, ctx := pools(t)

	rest := os.Getenv("TEST_DATABASE_URL")
	if index := strings.Index(rest, "://"); index >= 0 {
		rest = rest[index+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	runtime, err := fdb.Open(ctx, fdb.Config{Name: "authority-test-tenant", MaxConns: 4,
		DSN: fmt.Sprintf("postgres://organization_app:%s@%s", os.Getenv("TEST_RUNTIME_PASSWORD"), rest)})
	if err != nil {
		t.Fatalf("open the tenant pool: %v", err)
	}
	t.Cleanup(runtime.Close)

	providerPool, err := db.NewProviderPool(provider, noEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	tenantPool, err := db.NewTenantPool(runtime)
	if err != nil {
		t.Fatal(err)
	}
	memberships, err := membership.New(tenantPool)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := NewTenantAdministration(providerPool, tenantPool, memberships)
	if err != nil {
		t.Fatal(err)
	}
	records, err := NewTenantRecords(tenantPool)
	if err != nil {
		t.Fatal(err)
	}
	acting := newID(t)
	return tenantAdminFixture{admin: admin, records: records, tenants: tenantPool, owner: owner,
		ctx: actingAs(t, ctx, acting), base: ctx, provider: acting}
}

// newTenant arranges an Organization and a Tenant in the status given, as the owner.
func (f tenantAdminFixture) newTenant(t *testing.T, status string) id.UUID {
	t.Helper()
	organization, tenant := newID(t), newID(t)
	exec(t, f.base, f.owner, `INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'Admin grant org', 'customer', 'active')`, organization.String())
	exec(t, f.base, f.owner, `INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, 'Admin grant tenant', $3, 'pooled')`, tenant.String(), organization.String(), status)
	return tenant
}

func (f tenantAdminFixture) standing(t *testing.T, principal, tenant id.UUID) TenantStanding {
	t.Helper()
	standing, err := f.records.TenantStanding(f.base, principal, tenant, newID(t))
	if err != nil {
		t.Fatalf("TenantStanding: %v", err)
	}
	return standing
}

func (f tenantAdminFixture) count(t *testing.T, statement string, args ...any) int {
	t.Helper()
	var n int
	if err := f.owner.InTx(f.base, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, statement, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	return n
}

func TestAProviderMakesATenantsFirstAdministrator(t *testing.T) {
	f := tenantAdministration(t)
	tenant, principal := f.newTenant(t, "active"), newID(t)

	if standing := f.standing(t, principal, tenant); standing.Administers() || standing.Member || standing.Granted {
		t.Fatalf("a stranger stands %+v before any grant", standing)
	}
	made, err := f.admin.Grant(f.ctx, tenant, principal, "the customer's first administrator")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !made.MembershipCreated || made.Grant.GrantedBy != f.provider || made.Grant.Principal != principal ||
		made.Grant.TenantID != tenant || made.Grant.RevokedAt != nil {
		t.Errorf("the grant answered %+v, want a new Membership and a grant by the acting provider", made)
	}
	if standing := f.standing(t, principal, tenant); !standing.Administers() {
		t.Errorf("the new administrator stands %+v, want all three", standing)
	}
	if n := f.count(t, `SELECT count(*) FROM membership.membership
	    WHERE membership_id = $1 AND status = 'active' AND workspace_id IS NULL AND subject_type = 'human'
	      AND provenance = $2`, made.MembershipID.String(), "tenant administration grant "+made.Grant.ID.String()); n != 1 {
		t.Errorf("the Membership the grant made: %d rows, want one Tenant-wide, naming the grant", n)
	}
	if n := f.count(t, `SELECT count(*) FROM membership.membership_event WHERE membership_id = $1`,
		made.MembershipID.String()); n != 1 {
		t.Errorf("the Membership's events: %d, want its grant event in the same transaction", n)
	}

	if _, err := f.admin.Grant(f.ctx, tenant, principal, "again"); !errors.Is(err, ErrAlreadyAdministrator) {
		t.Errorf("a second grant in force answered %v, want ErrAlreadyAdministrator", err)
	}
	listed, err := f.admin.List(f.ctx, tenant, "an access review")
	if err != nil || len(listed) != 1 || listed[0].ID != made.Grant.ID {
		t.Errorf("List answered %+v, %v; want the one grant", listed, err)
	}
}

func TestARevokedGrantStopsAdministrationAndKeepsTheMembership(t *testing.T) {
	f := tenantAdministration(t)
	tenant, principal := f.newTenant(t, "active"), newID(t)
	made, err := f.admin.Grant(f.ctx, tenant, principal, "first administrator")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	revoked, err := f.admin.Revoke(f.ctx, tenant, made.Grant.ID, "left the customer")
	if err != nil || revoked.RevokedAt == nil || revoked.RevokedBy == nil || *revoked.RevokedBy != f.provider ||
		revoked.RevokeReason != "left the customer" {
		t.Fatalf("Revoke answered %+v, %v", revoked, err)
	}
	if standing := f.standing(t, principal, tenant); standing.Administers() || !standing.Member || standing.Granted {
		t.Errorf("after the revocation the Principal stands %+v, want a member who administers nothing", standing)
	}
	if _, err := f.admin.Revoke(f.ctx, tenant, made.Grant.ID, "again"); !errors.Is(err, ErrAdminGrantRevoked) {
		t.Errorf("a second revocation answered %v, want ErrAdminGrantRevoked", err)
	}
	if _, err := f.admin.Revoke(f.ctx, tenant, newID(t), "unknown"); !errors.Is(err, ErrAdminGrantNotFound) {
		t.Errorf("an unknown grant answered %v, want ErrAdminGrantNotFound", err)
	}
	// Another Tenant's grant is not found here: the revocation is bound to the Tenant it names.
	other := f.newTenant(t, "active")
	if _, err := f.admin.Revoke(f.ctx, other, made.Grant.ID, "wrong Tenant"); !errors.Is(err, ErrAdminGrantNotFound) {
		t.Errorf("a grant revoked through another Tenant answered %v, want ErrAdminGrantNotFound", err)
	}

	again, err := f.admin.Grant(f.ctx, tenant, principal, "back again")
	if err != nil || again.MembershipCreated || again.MembershipID != made.MembershipID {
		t.Errorf("a grant after a revocation answered %+v, %v; want a new grant on the same Membership", again, err)
	}
	if listed, err := f.admin.List(f.ctx, tenant, "an access review"); err != nil || len(listed) != 2 {
		t.Errorf("List answered %d grants, %v; want the revoked one kept beside the new one", len(listed), err)
	}
}

func TestEachFactIsReadAtTheNextRequest(t *testing.T) {
	f := tenantAdministration(t)
	tenant, principal := f.newTenant(t, "active"), newID(t)
	made, err := f.admin.Grant(f.ctx, tenant, principal, "first administrator")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	exec(t, f.base, f.owner, `UPDATE tenant.tenant SET status = 'suspended' WHERE tenant_id = $1`, tenant.String())
	if standing := f.standing(t, principal, tenant); standing.Administers() || standing.TenantActive {
		t.Errorf("in a suspended Tenant the Principal stands %+v", standing)
	}
	exec(t, f.base, f.owner, `UPDATE tenant.tenant SET status = 'active' WHERE tenant_id = $1`, tenant.String())

	exec(t, f.base, f.owner, `UPDATE membership.membership SET status = 'revoked' WHERE membership_id = $1`,
		made.MembershipID.String())
	if standing := f.standing(t, principal, tenant); standing.Administers() || standing.Member {
		t.Errorf("with its Membership revoked the Principal stands %+v", standing)
	}
}

func TestAGrantIsRefusedWhereItCannotStand(t *testing.T) {
	f := tenantAdministration(t)

	if _, err := f.admin.Grant(f.ctx, newID(t), newID(t), "no such Tenant"); !errors.Is(err, ErrAdminTenantNotFound) {
		t.Errorf("an unknown Tenant answered %v, want ErrAdminTenantNotFound", err)
	}
	suspended := f.newTenant(t, "suspended")
	if _, err := f.admin.Grant(f.ctx, suspended, newID(t), "a suspended Tenant"); !errors.Is(err, ErrAdminTenantNotActive) {
		t.Errorf("a suspended Tenant answered %v, want ErrAdminTenantNotActive", err)
	}

	// A suspended Membership is refused rather than replaced, and the grant rolls back with it.
	tenant, principal := f.newTenant(t, "active"), newID(t)
	exec(t, f.base, f.owner, `INSERT INTO membership.membership
	    (membership_id, principal_id, tenant_id, subject_type, status, valid_from, provenance)
	    VALUES ($1, $2, $3, 'human', 'suspended', now(), 'migration')`,
		newID(t).String(), principal.String(), tenant.String())
	if _, err := f.admin.Grant(f.ctx, tenant, principal, "over a suspension"); !errors.Is(err, ErrMembershipSuspended) {
		t.Errorf("a suspended Membership answered %v, want ErrMembershipSuspended", err)
	}
	if n := f.count(t, `SELECT count(*) FROM membership.tenant_admin_grant WHERE tenant_id = $1`, tenant.String()); n != 0 {
		t.Errorf("a refused grant left %d rows", n)
	}

	// A tenant scope cannot reach the provider's act at all.
	scope, err := db.TenantScope(tenant, principal, newID(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Grant(db.WithScope(f.base, scope), tenant, newID(t), "a tenant caller"); !errors.Is(err, db.ErrWrongScope) {
		t.Errorf("a tenant scope answered %v, want ErrWrongScope", err)
	}
}

// The restrictive policies (TDD-organization-control-001 §The Tenant Administration Grant): the tenant
// role holds the privileges, and only a provider's act on the Tenant may use them.
func TestOnlyAProvidersActWritesAGrant(t *testing.T) {
	f := tenantAdministration(t)
	tenant, principal := f.newTenant(t, "active"), newID(t)
	made, err := f.admin.Grant(f.ctx, tenant, principal, "first administrator")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	scope, err := db.TenantScope(tenant, principal, newID(t))
	if err != nil {
		t.Fatal(err)
	}
	asTenant := db.WithScope(f.base, scope)

	err = db.WithTenantScope(asTenant, f.tenants, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO membership.tenant_admin_grant (grant_id, tenant_id, principal_id, granted_by, reason)
		    VALUES ($1, $2, $3, $3, 'self-granted')`, newID(t).String(), tenant.String(), newID(t).String())
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("a tenant transaction inserting a grant answered %v, want a row-level security refusal", err)
	}

	err = db.WithTenantScope(asTenant, f.tenants, func(ctx context.Context, tx db.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE membership.tenant_admin_grant
		    SET revoked_at = now(), revoked_by = $2, revoke_reason = 'self-revoked' WHERE grant_id = $1`,
			made.Grant.ID.String(), principal.String())
		if err == nil && tag.RowsAffected() != 0 {
			return fmt.Errorf("revoked %d grants", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Errorf("a tenant transaction revoking a grant: %v, want it to touch nothing", err)
	}
	if standing := f.standing(t, principal, tenant); !standing.Granted {
		t.Error("a tenant transaction revoked the grant")
	}

	err = db.WithTenantScope(asTenant, f.tenants, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM membership.tenant_admin_grant WHERE grant_id = $1`, made.Grant.ID.String())
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("a delete answered %v, want permission denied", err)
	}
	err = db.WithTenantScope(asTenant, f.tenants, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE membership.tenant_admin_grant SET reason = 'rewritten' WHERE grant_id = $1`,
			made.Grant.ID.String())
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("rewriting the reason answered %v, want permission denied", err)
	}
}
