package membership

// Moving a Membership's version past a consumer's after a restore to an older point
// (TDD-organization-control-002 1.15.0 §After a Restore to an Older Point, SAD-004 §6.6).
//
// A restore loses every change made after the backup, and a consumer keeps what it applied: it holds
// some Memberships at versions authority no longer has. Authority's next version of such a
// Membership equals one the consumer already holds, and the consumer discards it as already applied,
// so a change made after the restore never arrives. Nothing repairs that by itself: a reconciliation
// repair carries authority's lower version, which the consumer discards for the same reason.
//
// The advance moves the version past the consumer's and publishes authority's state at it, so the
// consumer applies authority's state and every later change. It is applied only to a Membership the
// consumer reports, and a consumer reports only active Memberships, so what it publishes is never
// wider than what the consumer holds: active over active, or a withdrawal over active. A Membership
// the consumer holds withdrawn at a higher version is not reported and is not advanced, because
// publishing authority's active state over it would grant again access a lost revocation withdrew.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// ErrAdvanceReasonRequired refuses an advance that does not say why: it publishes authority's state
// over a consumer's, and its record must answer what restore it repaired.
var ErrAdvanceReasonRequired = errors.New("membership: a version advance requires a reason")

// Advance asks for one Membership's version to move past Above, the version a consumer holds.
type Advance struct {
	MembershipID id.UUID
	TenantID     id.UUID
	Above        int64
}

// Advanced is what one advance did.
type Advanced struct {
	MembershipID id.UUID
	TenantID     id.UUID
	FromVersion  int64
	ToVersion    int64
	Status       State

	// EventID is the event that published the state at ToVersion; nil when the Membership was already
	// past Above and nothing was written.
	EventID *id.UUID
}

const advanceStatement = `UPDATE membership.membership
SET membership_version = $2,
    updated_at = now()
WHERE membership_id = $1
  AND membership_version = $3
RETURNING membership_version`

// actionFor names the transition whose event type carries a state. The consumers apply a Membership
// event by its payload's state and version, whatever its type, and the type sets the lane: a
// withdrawal takes the priority lane, as the transition that made it did.
func actionFor(status State) Action {
	switch status {
	case StateSuspended:
		return ActionSuspend
	case StateRevoked:
		return ActionRevoke
	default:
		return ActionRestore
	}
}

// AdvanceVersions moves each Membership past its Above, one Tenant per transaction, each a provider's
// act inside that Tenant with the access recorded under reason. A Membership already past its Above
// is reported and left alone, so sending the same report twice changes nothing the second time.
func (s *Service) AdvanceVersions(ctx context.Context, advances []Advance, reason string) ([]Advanced, error) {
	if s.provider == nil {
		return nil, errors.New("membership: a provider pool is required for a version advance")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, ErrAdvanceReasonRequired
	}
	byTenant := map[id.UUID][]Advance{}
	var tenants []id.UUID
	for _, a := range advances {
		if a.MembershipID.IsNil() || a.TenantID.IsNil() || a.Above < 1 {
			return nil, fmt.Errorf("%w: an advance names a Membership, its Tenant and a version", ErrInvalid)
		}
		if _, seen := byTenant[a.TenantID]; !seen {
			tenants = append(tenants, a.TenantID)
		}
		byTenant[a.TenantID] = append(byTenant[a.TenantID], a)
	}
	// One order for every run, so two operators advancing the same report lock in the same order.
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].String() < tenants[j].String() })

	done := []Advanced{}
	for _, tenantID := range tenants {
		batch := byTenant[tenantID]
		sort.Slice(batch, func(i, j int) bool { return batch[i].MembershipID.String() < batch[j].MembershipID.String() })
		var applied []Advanced
		if err := db.WithProviderInTenant(ctx, s.provider, s.pool, tenantID, reason, func(ctx context.Context, tx db.Tx) error {
			applied = applied[:0]
			for _, a := range batch {
				result, err := s.advanceWithin(ctx, tx, a, reason)
				if err != nil {
					return err
				}
				applied = append(applied, result)
			}
			return nil
		}); err != nil {
			return done, err
		}
		done = append(done, applied...)
	}
	return done, nil
}

func (s *Service) advanceWithin(ctx context.Context, tx db.Tx, a Advance, reason string) (Advanced, error) {
	current, err := load(ctx, tx, selectForUpdate, a.MembershipID)
	if err != nil {
		return Advanced{}, err
	}
	result := Advanced{MembershipID: a.MembershipID, TenantID: a.TenantID, FromVersion: current.Version,
		ToVersion: current.Version, Status: current.Status}
	if current.Version > a.Above {
		return result, nil
	}

	acceptedAt := s.now().UTC()
	securityVersion, err := tenantSecurityVersion(ctx, tx, current.TenantID)
	if err != nil {
		return Advanced{}, err
	}
	var updated int64
	if err := tx.QueryRow(ctx, advanceStatement, a.MembershipID.String(), a.Above+1, current.Version).
		Scan(&updated); err != nil {
		return Advanced{}, fmt.Errorf("membership: advance version: %w", err)
	}
	current.Version = updated

	eventID, err := s.appendEvent(ctx, tx, actionFor(current.Status), current, securityVersion, acceptedAt,
		"version advanced past "+fmt.Sprint(a.Above)+" after a restore: "+reason)
	if err != nil {
		return Advanced{}, err
	}
	result.ToVersion, result.EventID = updated, &eventID
	return result, nil
}
