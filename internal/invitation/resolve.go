package invitation

import (
	"context"
	"errors"
	"fmt"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// Resolver answers identity-control's question for an enrolment: which invitation a token names
// (ADR-IAM-011 §5.1, §5.7; TDD-organization-control-004 1.18.0 §API / Interface).
//
// It reads through its own pool and role, never the self pool or the provider pool: the token is
// resolved before any Tenant is known and with no person signed in (§5.7 (c)). It changes nothing.
type Resolver struct {
	pool *db.InvitationResolverPool
}

// NewResolver constructs the resolver on the invitation resolver pool.
func NewResolver(pool *db.InvitationResolverPool) (*Resolver, error) {
	if pool == nil {
		return nil, errors.New("invitation: an invitation resolver pool is required")
	}
	return &Resolver{pool: pool}, nil
}

// Resolved is what the resolve answers: the invitation's identifier and its target address, and no
// Tenant (ADR-IAM-011 §5.1).
type Resolved struct {
	InvitationID     id.UUID
	TargetIdentifier string
}

// selectResolvable is the resolve's one statement. It names no row: invitation_resolver_read admits
// only the pending, unexpired row whose token hash the transaction bound, and the role reads these
// two columns alone, so the database returns that row or nothing (ADR-IAM-011 §5.7 (c)).
const selectResolvable = `SELECT invitation_id::text, target_identifier FROM invitation.invitation`

// Resolve returns the pending, unexpired invitation the token names, or ErrNotAcceptable for every
// other outcome: unknown, expired, revoked or accepted (ADR-IAM-011 §5.7 (b)). One statement runs
// whatever the token, and one access record is written before it, so neither the work done nor the
// answer separates the cases. A token that is not well formed is ErrToken, before anything runs.
func (r *Resolver) Resolve(ctx context.Context, token Token) (Resolved, error) {
	if !token.WellFormed() {
		return Resolved{}, ErrToken
	}
	var (
		resolved Resolved
		found    int
	)
	if err := db.WithInvitationResolver(ctx, r.pool, token.Hash(), func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, selectResolvable)
		if err != nil {
			return fmt.Errorf("invitation: resolve by token: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			found++
			var rawID, target string
			if err := rows.Scan(&rawID, &target); err != nil {
				return fmt.Errorf("invitation: scan resolve: %w", err)
			}
			invitationID, err := id.Parse(rawID)
			if err != nil {
				return fmt.Errorf("invitation: stored invitation identifier %q: %w", rawID, err)
			}
			resolved = Resolved{InvitationID: invitationID, TargetIdentifier: target}
		}
		return rows.Err()
	}); err != nil {
		return Resolved{}, err
	}
	switch {
	case found == 0:
		return Resolved{}, ErrNotAcceptable
	case found > 1:
		// token_hash is unique across the table and the policy admits one hash, so this is a policy
		// that admits too much. Refused as an internal fault rather than answered with one of the rows.
		return Resolved{}, fmt.Errorf("invitation: the resolve read %d rows; the policy admits one", found)
	}
	return resolved, nil
}
