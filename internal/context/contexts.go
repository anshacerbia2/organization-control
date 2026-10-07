package context

// The context list: where a person may work (ADR-ORG-005, TDD-organization-control-002 §The Context
// List). It is the set TDD-002 says is "retrieved through the context API and never placed in a
// token", and it is read here rather than through internal/membership for the reason the fresh check
// is: this package reads the authoritative tables and reaches no mutation path.

import (
	stdcontext "context"
	"errors"
	"fmt"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// Context is one place a Principal may work: an active Membership in an active Tenant.
//
// An item selects and does not grant (ADR-ORG-005 §5.3). Choosing it is a sign-in for the Tenant, and
// every request there checks the Membership, the Tenant and the administration grant again.
type Context struct {
	MembershipID      id.UUID
	TenantID          id.UUID
	TenantDisplayName string
	TenantStatus      string

	// WorkspaceID is the Workspace a Workspace-scoped Membership names; nil for a Tenant-wide one.
	WorkspaceID *id.UUID

	// Administers is whether the Principal holds an unrevoked tenant administration grant in the
	// Tenant (ADR-ORG-003).
	Administers bool
}

// ContextQuery selects one page of a Principal's contexts (STD-GLB-001 1.3.0 §Pagination).
type ContextQuery struct {
	// After is the membership_id of the last item of the previous page; nil starts at the first.
	After id.UUID

	// Limit is the page size, 1 to db.MaxListLimit; zero takes db.DefaultListLimit.
	Limit int
}

// ContextPage is one page in membership_id order. Next is the After of the following page, and nil
// on the last.
type ContextPage struct {
	Contexts []Context
	Next     *id.UUID
}

// Contexts reads a Principal's contexts: the Principal's own through the self pool, anyone's through
// the provider pool.
//
// Two entry points rather than one that picks a pool, as ConsumerAccess is two: the pool decides the
// database role, and tools/grantcheck derives each role's privileges from the wrapper a body is passed
// to. The body is shared, so both roles are judged on the same statement.
type Contexts struct {
	provider *db.ProviderPool
	self     *db.SelfPool
}

// NewContexts constructs the context list.
func NewContexts(provider *db.ProviderPool, self *db.SelfPool) (*Contexts, error) {
	switch {
	case provider == nil:
		return nil, errors.New("context: a provider-scoped pool is required")
	case self == nil:
		return nil, errors.New("context: a self pool is required")
	}
	return &Contexts{provider: provider, self: self}, nil
}

// contextsStatement is one keyset page of a Principal's active Memberships in active Tenants, with
// whether it administers each.
//
// Under the self role the policies already confine every table to the bound Principal; the
// principal_id predicate is stated anyway, because the provider role reads every row and the
// statement is the same for both.
const contextsStatement = `SELECT m.membership_id::text,
       m.tenant_id::text,
       t.display_name,
       t.status,
       m.workspace_id::text,
       EXISTS (SELECT 1 FROM membership.tenant_admin_grant g
                WHERE g.principal_id = m.principal_id
                  AND g.tenant_id = m.tenant_id
                  AND g.revoked_at IS NULL)
FROM membership.membership m
JOIN tenant.tenant t ON t.tenant_id = m.tenant_id
WHERE m.principal_id = $1::uuid
  AND m.status = 'active'
  AND t.status = 'active'
  AND ($2::uuid IS NULL OR m.membership_id > $2::uuid)
ORDER BY m.membership_id
LIMIT $3`

// Own reads the calling Principal's contexts, as organization_self_rt. The Principal is the self
// scope's actor, never an argument, so no request value names whose rows are read. No privileged
// access is recorded: a person reading their own rows is not provider access (ADR-ORG-005 §5.1).
func (c *Contexts) Own(ctx stdcontext.Context, query ContextQuery) (ContextPage, error) {
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return ContextPage{}, db.ErrNoScope
	}
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return ContextPage{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	page := ContextPage{Contexts: []Context{}}
	if err := db.WithSelfRead(ctx, c.self, func(ctx stdcontext.Context, tx db.Tx) error {
		return contextsIn(ctx, tx, scope.Actor(), query.After, limit, &page)
	}); err != nil {
		return ContextPage{}, err
	}
	return page, nil
}

// Of reads any Principal's contexts as a provider, with the caller's reason recorded before the page
// is read.
func (c *Contexts) Of(ctx stdcontext.Context, principal id.UUID, query ContextQuery, reason string) (ContextPage, error) {
	if principal.IsNil() {
		return ContextPage{}, fmt.Errorf("%w: a principal identifier is required", ErrInvalid)
	}
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return ContextPage{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	page := ContextPage{Contexts: []Context{}}
	if err := db.WithProviderScope(ctx, c.provider, reason, func(ctx stdcontext.Context, tx db.Tx) error {
		return contextsIn(ctx, tx, principal, query.After, limit, &page)
	}); err != nil {
		return ContextPage{}, err
	}
	return page, nil
}

// contextsIn reads one page into page, and sets Next when another follows.
func contextsIn(ctx stdcontext.Context, tx db.Tx, principal, after id.UUID, limit int, page *ContextPage) error {
	rows, err := tx.Query(ctx, contextsStatement, principal.String(), db.Keyset(after), limit+1)
	if err != nil {
		return fmt.Errorf("context: list contexts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			item                  Context
			rawMembership, rawTen string
			rawWorkspace          *string
		)
		if err := rows.Scan(&rawMembership, &rawTen, &item.TenantDisplayName, &item.TenantStatus,
			&rawWorkspace, &item.Administers); err != nil {
			return fmt.Errorf("context: scan contexts: %w", err)
		}
		if item.MembershipID, err = id.Parse(rawMembership); err != nil {
			return fmt.Errorf("context: stored membership identifier %q: %w", rawMembership, err)
		}
		if item.TenantID, err = id.Parse(rawTen); err != nil {
			return fmt.Errorf("context: stored tenant identifier %q: %w", rawTen, err)
		}
		if rawWorkspace != nil {
			workspace, err := id.Parse(*rawWorkspace)
			if err != nil {
				return fmt.Errorf("context: stored workspace identifier %q: %w", *rawWorkspace, err)
			}
			item.WorkspaceID = &workspace
		}
		page.Contexts = append(page.Contexts, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("context: read contexts: %w", err)
	}
	if len(page.Contexts) > limit {
		page.Contexts = page.Contexts[:limit]
		next := page.Contexts[limit-1].MembershipID
		page.Next = &next
	}
	return nil
}
