// Package db is the only package permitted to bind a transaction's isolation scope.
//
// TDD-organization-control-001 requires `SET LOCAL app.` to appear in exactly one package, and
// `binding_test.go` asserts that by walking the repository. The reason is not tidiness: a policy
// is only as strong as the value it reads, and the value is trustworthy only if every path that
// sets it is in one file a reviewer can hold in their head.
//
// # Two pool types, not one pool with a flag
//
// TenantPool and ProviderPool are distinct types so that handing a tenant-scoped handler the
// cross-Tenant pool is a compile error rather than a review finding. A single type with a boolean
// would put the decision at the call site, which is where defects live: any policy permissive
// enough for provider work is permissive enough for a mistake in a tenant-scoped path.
//
// # The scope comes from the context, never from an argument
//
// Neither function takes a Tenant identifier. SAD-004 §8.3 makes a Tenant identifier arriving
// from a client a *requested* scope, and the authoritative scope is resolved from the
// authenticated administrative context. A parameter would let a handler pass the value a caller
// supplied, which is exactly the substitution the RLS layer is a second line of defence against.
package db

import (
	"context"
	"errors"
	"fmt"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
)

// DEPARTURE from TDD-organization-control-001, which writes both signatures as
// `fn func(pgx.Tx) error`.
//
// arch.json denies this repository any import of pgx, and foundation-platform's db package exists
// so a driver type never reaches a domain signature — replacing the driver is then one module's
// change rather than every consumer's. `fdb.Tx` carries the same handle without naming the
// driver, so the departure is in the type name and not in the shape.
type (
	// Tx is the transaction handle a scoped body receives.
	Tx = fdb.Tx

	// Body is the work performed inside a bound transaction.
	Body func(context.Context, Tx) error
)

// Transactor is the transaction source these pools need.
//
// An interface rather than *fdb.Pool so the scope-resolution rules can be tested without a
// database, and so each pool type depends on the one method it uses. *fdb.Pool satisfies it.
// It is also what keeps this package from being able to do anything to a connection except open
// a transaction on it.
type Transactor interface {
	InTx(ctx context.Context, fn func(context.Context, Tx) error) error
}

var (
	// ErrNoScope means the context carried no resolved scope. It is a programming error rather
	// than a caller error: reaching a repository without having resolved scope means the
	// authorization layer was skipped.
	ErrNoScope = errors.New("db: the context carries no resolved isolation scope")

	// ErrWrongScope means a provider scope reached a tenant-scoped path or the reverse.
	ErrWrongScope = errors.New("db: the resolved scope does not match the pool it was used with")

	// ErrReasonRequired means a provider transaction was opened without one. PAD-PLT-002 §5.2
	// requires cross-tenant administration to carry a reason, and a blank one is evidence
	// naming nobody's intent.
	ErrReasonRequired = errors.New("db: a provider-scoped transaction requires a reason")
)

// Scope is the resolved isolation scope for one request.
//
// Its fields are unexported and it is constructed only by the two functions below, so a scope
// cannot be assembled field by field from request input.
type Scope struct {
	tenantID    id.UUID
	provider    bool
	actor       id.UUID
	correlation id.UUID
}

// TenantScope resolves to exactly one Tenant.
func TenantScope(tenantID, actor, correlation id.UUID) (Scope, error) {
	if tenantID.IsNil() {
		return Scope{}, errors.New("db: a tenant scope requires a tenant identifier")
	}
	if actor.IsNil() {
		return Scope{}, errors.New("db: a tenant scope requires an acting subject")
	}
	return Scope{tenantID: tenantID, actor: actor, correlation: correlation}, nil
}

// ProviderScope resolves to deliberately cross-Tenant provider authority.
func ProviderScope(actor, correlation id.UUID) (Scope, error) {
	if actor.IsNil() {
		return Scope{}, errors.New("db: a provider scope requires an acting subject")
	}
	if correlation.IsNil() {
		// Required rather than optional, unlike the tenant case. A cross-tenant action is
		// reviewed after the fact, and a review that cannot correlate the database work with the
		// request that caused it has an actor and no trail.
		return Scope{}, errors.New("db: a provider scope requires a correlation identifier")
	}
	return Scope{provider: true, actor: actor, correlation: correlation}, nil
}

// IsProvider reports whether this scope is the cross-Tenant one.
func (s Scope) IsProvider() bool { return s.provider }

// TenantID returns the bound Tenant, or the nil identifier for a provider scope.
func (s Scope) TenantID() id.UUID { return s.tenantID }

// Actor returns the acting subject.
func (s Scope) Actor() id.UUID { return s.actor }

// Correlation returns the correlation identifier.
func (s Scope) Correlation() id.UUID { return s.correlation }

type scopeKey struct{}

// WithScope places a resolved scope in the context. The authorization layer calls this after
// resolving the scope from the authenticated administrative context; nothing else should.
func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeFrom reads the resolved scope.
func ScopeFrom(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(scopeKey{}).(Scope)
	return scope, ok
}

// ProviderAccess is the record written before a cross-Tenant transaction runs.
type ProviderAccess struct {
	Actor       id.UUID
	Correlation id.UUID
	Reason      string
}

// PrivilegedRecorder records provider access as evidence.
//
// An interface rather than a concrete writer so this package depends on the one method it needs,
// and so the ordering property below can be asserted without a database.
type PrivilegedRecorder interface {
	RecordProviderAccess(ctx context.Context, access ProviderAccess) error
}

// TenantPool carries ordinary tenant-scoped traffic. It authenticates as a login role inheriting
// `organization_rt`.
type TenantPool struct{ tx Transactor }

// NewTenantPool wraps a transaction source that authenticates as the tenant-scoped runtime role.
func NewTenantPool(tx Transactor) (*TenantPool, error) {
	if tx == nil {
		return nil, errors.New("db: a transaction source is required")
	}
	return &TenantPool{tx: tx}, nil
}

// ProviderPool carries deliberately cross-Tenant traffic. It authenticates as a login role
// inheriting `organization_provider_rt`, and it cannot be constructed without a recorder.
type ProviderPool struct {
	tx       Transactor
	recorder PrivilegedRecorder
}

// NewProviderPool wraps the provider pool.
//
// The recorder is mandatory. PAD-PLT-002 §3.3 invariant 22 requires cross-tenant administration
// to carry evidence, and an optional recorder is one a deployment forgets to supply — after which
// every cross-tenant read is unattributable and nothing reports that.
func NewProviderPool(tx Transactor, recorder PrivilegedRecorder) (*ProviderPool, error) {
	if tx == nil {
		return nil, errors.New("db: a transaction source is required")
	}
	if recorder == nil {
		return nil, errors.New("db: a provider pool requires a privileged-access recorder")
	}
	return &ProviderPool{tx: tx, recorder: recorder}, nil
}

// WithTenantScope runs fn inside a transaction bound to exactly one Tenant.
//
// The identifier is taken from the resolved scope in ctx. It is never taken from a request path,
// query parameter, header, or body.
func WithTenantScope(ctx context.Context, pool *TenantPool, fn Body) error {
	if pool == nil {
		return errors.New("db: a tenant pool is required")
	}
	scope, ok := ScopeFrom(ctx)
	if !ok {
		return ErrNoScope
	}
	// A provider scope reaching here would bind a Tenant onto the tenant policy's role while the
	// caller believed it was acting cross-Tenant. Refused rather than reinterpreted: the two
	// plausible readings differ in the wrong direction.
	if scope.IsProvider() {
		return fmt.Errorf("%w: a provider scope reached a tenant-scoped pool", ErrWrongScope)
	}

	return pool.tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		// SET LOCAL, not SET. It reverts at commit or rollback, so a pooled connection cannot
		// carry one request's Tenant into the next — the failure that makes connection pooling
		// and Row-Level Security interact badly, and one that is invisible under low
		// concurrency because the next request simply sees the previous Tenant.
		//
		// set_config with is_local = true rather than string interpolation: SET LOCAL takes no
		// parameters, and building the statement by concatenation would put a value from the
		// scope into SQL text.
		if _, err := tx.Exec(ctx,
			`SELECT set_config('app.tenant_id', $1, true)`, scope.tenantID.String()); err != nil {
			return fmt.Errorf("db: bind tenant scope: %w", err)
		}
		return fn(ctx, tx)
	})
}

// WithProviderScope runs fn inside a deliberately cross-Tenant transaction.
//
// It requires a provider scope, a reason, and a correlation identifier, and it records the access
// before fn executes. The order matters: evidence written afterwards is missing for exactly the
// cases that matter, because a transaction that panics or is killed mid-flight is the one an
// investigation asks about.
func WithProviderScope(ctx context.Context, pool *ProviderPool, reason string, fn Body) error {
	if pool == nil {
		return errors.New("db: a provider pool is required")
	}
	scope, ok := ScopeFrom(ctx)
	if !ok {
		return ErrNoScope
	}
	if !scope.IsProvider() {
		return fmt.Errorf("%w: a tenant scope reached the provider pool", ErrWrongScope)
	}
	if reason == "" {
		return ErrReasonRequired
	}

	// Recorded first, and a failure here stops the transaction. Proceeding without evidence
	// would make the access unattributable, which is the one property PAD-PLT-002 §3.3
	// invariant 22 does not treat as optional.
	if err := pool.recorder.RecordProviderAccess(ctx, ProviderAccess{
		Actor:       scope.actor,
		Correlation: scope.correlation,
		Reason:      reason,
	}); err != nil {
		return fmt.Errorf("db: record provider access: %w", err)
	}

	return pool.tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		// The provider policy reads this rather than granting unconditional access, so an
		// unbound provider connection fails closed the same way an unbound tenant one does.
		// BYPASSRLS would have been simpler and would have made the access indistinguishable in
		// the catalog from a role that never needed it.
		if _, err := tx.Exec(ctx,
			`SELECT set_config('app.provider_scope', 'true', true)`); err != nil {
			return fmt.Errorf("db: bind provider scope: %w", err)
		}
		return fn(ctx, tx)
	})
}
