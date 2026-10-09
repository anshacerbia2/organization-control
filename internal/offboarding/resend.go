package offboarding

// Sending a failed deprovisioning again (TDD-organization-control-004 1.11.0 §Sending a Failed
// Deprovisioning Again).

import (
	"context"
	"errors"
	"fmt"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// ErrResendRefused reports a resend of a deprovisioning that did not fail. `requested` is in flight,
// `realized` is done, and `unresolved` is ambiguous: the target may have released the infrastructure,
// and sending the command again is the retry of an unknown outcome SAD-004 §7.5 forbids. Only a
// refusal the provisioning system reported is retried.
var ErrResendRefused = errors.New("offboarding: only a failed deprovisioning is sent again")

// ResendDeprovisioning records a new deprovisioning command for an offboarding in release whose
// latest one failed, and publishes it, in one transaction.
//
// The new request carries the offboarding's correlation identifier, as the first did, so the
// provisioning system's report correlates to it the same way; it is the most recent request, which is
// the one retirement and the report both read. The earlier one keeps its `failed` record. The command
// is the `released` event, published again with a new event identifier, which the provisioning
// system acts on as it acted on the first. The stage does not move: release is where an offboarding
// waits for its deprovisioning.
func (s *Service) ResendDeprovisioning(ctx context.Context, offboardingID id.UUID, reason string) (Offboarding, error) {
	if offboardingID.IsNil() {
		return Offboarding{}, fmt.Errorf("%w: an offboarding identifier is required", ErrInvalid)
	}
	requestID, err := s.newID()
	if err != nil {
		return Offboarding{}, fmt.Errorf("offboarding: mint deprovisioning identifier: %w", err)
	}
	at := s.now().UTC()

	var record Offboarding
	if err := db.WithProviderScope(ctx, s.provider, reason, func(ctx context.Context, tx db.Tx) error {
		loaded, err := load(ctx, tx, offboardingID)
		if err != nil {
			return err
		}
		if loaded.Stage != StageRelease {
			return fmt.Errorf("%w: %s is at %s; a deprovisioning is sent again only in release",
				ErrStageRefused, offboardingID, loaded.Stage)
		}
		// The two gates release passed, checked again: a legal hold placed since, or an obligation
		// reopened, holds the command as it would have held the release.
		if err := s.releaseGates(ctx, tx, loaded); err != nil {
			return err
		}
		var state, detail string
		if err := tx.QueryRow(ctx, deprovisioningStatement,
			loaded.TenantID.String(), loaded.OffboardingID.String()).Scan(&state, &detail); err != nil {
			return fmt.Errorf("%w: no deprovisioning command is recorded for %s",
				ErrAmbiguousOutcome, offboardingID)
		}
		if state != "failed" {
			return fmt.Errorf("%w: the deprovisioning of %s is %s", ErrResendRefused, offboardingID, state)
		}
		if _, err := tx.Exec(ctx, insertDeprovisioning,
			requestID.String(), loaded.TenantID.String(), loaded.OffboardingID.String(),
			loaded.CorrelationID.String(), at); err != nil {
			return fmt.Errorf("offboarding: record deprovisioning command: %w", err)
		}
		if err := s.publish(ctx, tx, "released", offboardingID, StagePayload{
			OffboardingID: offboardingID, TenantID: loaded.TenantID,
			Stage: StageRelease, LegalHold: loaded.LegalHold,
		}, at); err != nil {
			return err
		}
		if err := derive(ctx, tx, &loaded); err != nil {
			return err
		}
		record = loaded
		return db.Respond(ctx, tx, record)
	}); err != nil {
		return Offboarding{}, err
	}
	return record, nil
}
