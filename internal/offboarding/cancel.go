package offboarding

// Cancelling an offboarding before release (ADR-ORG-006, TDD-organization-control-004 1.8.0
// §Cancellation).
//
// The freeze is reversible because nothing has been destroyed by then, and this is what makes it so:
// the Tenant returns to the status it held when the offboarding began, the Memberships the freeze
// suspended are restored, and nothing else is undone.

import (
	"context"
	"fmt"
	"strings"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/tenant"
)

// RestoreBatchSize is how many Memberships one restoring transaction restores, the freeze's bound
// for the same reason: a Tenant's Memberships in one transaction is a lock held for minutes.
const RestoreBatchSize = 100

// CancelRequest cancels one offboarding.
type CancelRequest struct {
	OffboardingID id.UUID

	// ExpectedVersion is the Tenant `version` the operator was shown, which the Tenant transition is
	// held to. Not checked on a resume, which makes no Tenant transition.
	ExpectedVersion int64

	// Reason is the X-Administrative-Reason, recorded as cancel_reason and on every transition.
	Reason string
}

const cancelStatement = `UPDATE operation.offboarding
SET stage = 'cancelled', cancelled_by = $2, cancel_reason = $3, cancelled_at = $4
WHERE offboarding_id = $1`

// cancelObligations closes what is still open. Completed, waived and failed rows keep their record.
const cancelObligations = `UPDATE operation.offboarding_obligation
SET state = 'cancelled', resolved_by = $2, resolved_at = $3
WHERE offboarding_id = $1 AND state = 'open'`

// Cancel ends an offboarding in `freeze` or `obligations` and restores what it removed.
//
// One provider transaction moves the Tenant back to its prior status, stamps the offboarding
// cancelled with who, why and when, closes open obligations as cancelled, and publishes the
// process event. The restorations follow, after that commit, in batches on the tenant pool like the
// freeze: a restored Membership's event carries the Tenant's security version, and one restored
// before the Tenant transition would carry the offboarding's and be superseded by it.
//
// Sent again for a cancelled offboarding, it makes no transition and resumes the restorations, so a
// request that failed part way is finished by repeating it.
func (s *Service) Cancel(ctx context.Context, req CancelRequest) (Offboarding, error) {
	switch {
	case req.OffboardingID.IsNil():
		return Offboarding{}, fmt.Errorf("%w: an offboarding identifier is required", ErrInvalid)
	case strings.TrimSpace(req.Reason) == "":
		return Offboarding{}, fmt.Errorf("%w: a reason is required to cancel an offboarding", ErrInvalid)
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Offboarding{}, db.ErrNoScope
	}
	at := s.now().UTC()

	var tenantID id.UUID
	if err := db.WithProviderScope(ctx, s.provider, req.Reason, func(ctx context.Context, tx db.Tx) error {
		loaded, err := load(ctx, tx, req.OffboardingID)
		if err != nil {
			return err
		}
		tenantID = loaded.TenantID
		if loaded.Stage == StageCancelled {
			return nil
		}
		if !loaded.Stage.Cancellable() {
			return fmt.Errorf("%w: %s is at %s; an offboarding is cancelled before release, which is irreversible",
				ErrStageRefused, req.OffboardingID, loaded.Stage)
		}
		var action tenant.Action
		switch loaded.PriorStatus {
		case string(tenant.StateActive):
			action = tenant.ActionCancelOffboardingToActive
		case string(tenant.StateSuspended):
			action = tenant.ActionCancelOffboardingToSuspended
		default:
			return fmt.Errorf("%w: %s began before offboardings recorded the Tenant's prior status and the "+
				"Memberships their freeze suspended, so a cancellation could only guess what to restore; "+
				"restore the Tenant and its Memberships by hand", ErrNotReversible, req.OffboardingID)
		}

		if _, err := s.tenants.TransitionWithin(ctx, tx, action, tenant.Command{
			TenantID:        loaded.TenantID,
			Reason:          req.Reason,
			ExpectedVersion: req.ExpectedVersion,
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, cancelStatement,
			req.OffboardingID.String(), scope.Actor().String(), req.Reason, at); err != nil {
			return fmt.Errorf("offboarding: record the cancellation: %w", err)
		}
		if _, err := tx.Exec(ctx, cancelObligations,
			req.OffboardingID.String(), scope.Actor().String(), at); err != nil {
			return fmt.Errorf("offboarding: close the open obligations: %w", err)
		}
		return s.publish(ctx, tx, "cancelled", req.OffboardingID, StagePayload{
			OffboardingID: req.OffboardingID, TenantID: loaded.TenantID,
			Stage: StageCancelled, LegalHold: loaded.LegalHold,
		}, at)
	}); err != nil {
		return Offboarding{}, err
	}

	for {
		restored, err := s.restoreBatch(ctx, req.OffboardingID, tenantID, RestoreBatchSize, req.Reason)
		if err != nil {
			return Offboarding{}, err
		}
		if restored == 0 {
			break
		}
	}
	return s.Get(ctx, req.OffboardingID)
}

// selectRestoreBatch locks the frozen Memberships of one offboarding that are not yet restored and
// are still suspended. The predicate is the resume token, as the freeze's is: a batch that committed
// stamped restored_at and is not selected again. A frozen Membership no longer suspended is left as
// it is.
const selectRestoreBatch = `SELECT m.membership_id::text, m.membership_version
FROM membership.offboarding_freeze f
JOIN membership.membership m ON m.membership_id = f.membership_id
WHERE f.offboarding_id = $1
  AND f.restored_at IS NULL
  AND m.status = 'suspended'
ORDER BY f.membership_id
LIMIT $2
FOR UPDATE OF m SKIP LOCKED`

const stampRestored = `UPDATE membership.offboarding_freeze
SET restored_at = $3
WHERE offboarding_id = $1 AND membership_id = $2`

// restoreBatch restores up to size of the Memberships the freeze suspended, each through the
// ordinary restore transition held to the version it locked, and reports how many it restored.
func (s *Service) restoreBatch(ctx context.Context, offboardingID, tenantID id.UUID, size int, reason string) (int, error) {
	var restored int
	if err := db.WithProviderInTenant(ctx, s.provider, s.tenantPool, tenantID, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, selectRestoreBatch, offboardingID.String(), size)
		if err != nil {
			return fmt.Errorf("offboarding: select restore batch: %w", err)
		}
		var batch []membership.Command
		for rows.Next() {
			var (
				raw     string
				version int64
			)
			if err := rows.Scan(&raw, &version); err != nil {
				rows.Close()
				return fmt.Errorf("offboarding: scan restore batch: %w", err)
			}
			parsed, err := id.Parse(raw)
			if err != nil {
				rows.Close()
				return fmt.Errorf("offboarding: stored membership id %q: %w", raw, err)
			}
			batch = append(batch, membership.Command{MembershipID: parsed, ExpectedVersion: version, Reason: reason})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("offboarding: read restore batch: %w", err)
		}

		at := s.now().UTC()
		for _, cmd := range batch {
			if _, err := s.memberships.TransitionWithin(ctx, tx, membership.ActionRestore, cmd); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, stampRestored, offboardingID.String(), cmd.MembershipID.String(), at); err != nil {
				return fmt.Errorf("offboarding: stamp the restored Membership: %w", err)
			}
			restored++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return restored, nil
}
