package offboarding

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// RaiseRequest asks a domain for something before a Tenant's data may be released.
type RaiseRequest struct {
	OffboardingID id.UUID

	// Domain is the accountable domain: product, hcm, billing, audit. Named rather than inferred,
	// because the domain is who may later resolve it.
	Domain string

	Type string

	// DueAt is when the obligation is expected to be met. Optional: an obligation with no due date
	// still holds the process, so an absent date delays nothing — it only removes the ability to
	// report which domain is late.
	DueAt *time.Time
}

const insertObligation = `INSERT INTO operation.offboarding_obligation
    (obligation_id, offboarding_id, tenant_id, domain, obligation_type, state, due_at)
VALUES ($1, $2, $3, $4, $5, 'open', $6)`

// Raise records one obligation and publishes it.
//
// Permitted only while the offboarding is in `obligations`. Raising during the freeze would let an
// obligation exist against a Tenant whose Memberships are still active, and raising after release
// would ask a domain for something whose subject has already been deprovisioned.
//
// The Tenant identifier is copied onto the child from the parent row rather than taken from the
// caller. `TDD-organization-control-001` requires the column, and the composite foreign key means a
// caller-supplied value would be refused by the constraint anyway — reading it from the parent
// turns a possible constraint violation into an impossible one.
func (s *Service) Raise(ctx context.Context, req RaiseRequest) (Obligation, error) {
	switch {
	case req.OffboardingID.IsNil():
		return Obligation{}, fmt.Errorf("%w: an offboarding identifier is required", ErrInvalid)
	case strings.TrimSpace(req.Domain) == "":
		return Obligation{}, fmt.Errorf("%w: an accountable domain is required", ErrInvalid)
	case strings.TrimSpace(req.Type) == "":
		return Obligation{}, fmt.Errorf("%w: an obligation type is required", ErrInvalid)
	}

	obligationID, err := s.newID()
	if err != nil {
		return Obligation{}, fmt.Errorf("offboarding: mint identifier: %w", err)
	}
	at := s.now().UTC()

	obligation := Obligation{
		ObligationID:  obligationID,
		OffboardingID: req.OffboardingID,
		Domain:        req.Domain,
		Type:          req.Type,
		State:         ObligationOpen,
		DueAt:         req.DueAt,
	}

	if err := db.WithProviderScope(ctx, s.provider,
		"raise obligation "+req.Domain+"/"+req.Type,
		func(ctx context.Context, tx db.Tx) error {
			record, err := load(ctx, tx, req.OffboardingID)
			if err != nil {
				return err
			}
			if record.Stage != StageObligations {
				return fmt.Errorf("%w: obligations may be raised only at %s, and this is at %s",
					ErrStageRefused, StageObligations, record.Stage)
			}
			obligation.TenantID = record.TenantID

			if _, err := tx.Exec(ctx, insertObligation,
				obligation.ObligationID.String(), obligation.OffboardingID.String(),
				obligation.TenantID.String(), obligation.Domain, obligation.Type,
				nullableTime(obligation.DueAt)); err != nil {
				return fmt.Errorf("offboarding: insert obligation: %w", err)
			}

			return s.publish(ctx, tx, "obligation-raised", req.OffboardingID, ObligationPayload{
				OffboardingID: obligation.OffboardingID, TenantID: obligation.TenantID,
				ObligationID: obligation.ObligationID, Domain: obligation.Domain,
				Type: obligation.Type, DueAt: obligation.DueAt,
			}, at)
		}); err != nil {
		return Obligation{}, err
	}

	return obligation, nil
}

// Resolution is a domain reporting what happened to its obligation.
type Resolution struct {
	ObligationID id.UUID

	// Domain is the domain claiming to resolve it, checked against the obligation. Without this
	// one domain could close another's obligation, and the registry would record a consent that
	// was never given.
	Domain string

	// State must be a resolving or failing state. `open` is refused: an obligation cannot be
	// reported back into the state it started in, and accepting it would look like progress.
	State ObligationState

	// Detail is required for a waiver and a failure, and ignored for a completion. A waiver
	// without a reason records that somebody decided, and not what they decided or why — which is
	// the half an audit needs.
	Detail string
}

const selectObligation = `SELECT obligation_id::text,
       offboarding_id::text,
       tenant_id::text,
       domain,
       obligation_type,
       state,
       due_at,
       completed_at,
       coalesce(detail, '')
FROM operation.offboarding_obligation
WHERE obligation_id = $1
FOR UPDATE`

const resolveObligation = `UPDATE operation.offboarding_obligation
SET state = $2, completed_at = $3, detail = $4
WHERE obligation_id = $1`

// Resolve records a domain's outcome for one obligation.
//
// A failure is recorded and still holds the process. That is the point of separating it from a
// waiver: a domain that cannot meet an obligation has said so, an operator can see it, and nothing
// advances until somebody with the authority to waive it decides to.
func (s *Service) Resolve(ctx context.Context, res Resolution) (Obligation, error) {
	switch {
	case res.ObligationID.IsNil():
		return Obligation{}, fmt.Errorf("%w: an obligation identifier is required", ErrInvalid)
	case strings.TrimSpace(res.Domain) == "":
		return Obligation{}, fmt.Errorf("%w: the resolving domain is required", ErrInvalid)
	case !res.State.Valid() || res.State == ObligationOpen:
		return Obligation{}, fmt.Errorf("%w: %q is not a resolution", ErrInvalid, res.State)
	case res.State != ObligationCompleted && strings.TrimSpace(res.Detail) == "":
		return Obligation{}, fmt.Errorf("%w: %s requires a detail", ErrInvalid, res.State)
	}

	var obligation Obligation
	at := s.now().UTC()

	if err := db.WithProviderScope(ctx, s.provider,
		"resolve obligation "+res.ObligationID.String()+" as "+string(res.State),
		func(ctx context.Context, tx db.Tx) error {
			loaded, err := loadObligation(ctx, tx, res.ObligationID)
			if err != nil {
				return err
			}
			if loaded.Domain != res.Domain {
				return fmt.Errorf("%w: %s was raised against %s", ErrWrongDomain,
					res.ObligationID, loaded.Domain)
			}
			// A failed obligation may be retried or waived; a resolved one is final. The first
			// resolution records who decided, and a second would overwrite that record.
			if loaded.State.Resolved() {
				return fmt.Errorf("%w: %s is %s", ErrAlreadyResolved, res.ObligationID, loaded.State)
			}

			var completedAt any
			if res.State.Resolved() {
				completedAt = at
			}
			if _, err := tx.Exec(ctx, resolveObligation,
				res.ObligationID.String(), string(res.State), completedAt, res.Detail); err != nil {
				return fmt.Errorf("offboarding: resolve obligation: %w", err)
			}

			loaded.State = res.State
			loaded.Detail = res.Detail
			if res.State.Resolved() {
				stamped := at
				loaded.CompletedAt = &stamped
			}
			obligation = loaded
			return nil
		}); err != nil {
		return Obligation{}, err
	}

	return obligation, nil
}

// Outstanding names what still holds the process, for an operator rather than for a gate.
func (s *Service) Outstanding(ctx context.Context, offboardingID id.UUID) ([]string, error) {
	var out []string
	if err := db.WithProviderScope(ctx, s.provider,
		"read outstanding obligations for "+offboardingID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var err error
			out, err = outstandingObligations(ctx, tx, offboardingID)
			return err
		}); err != nil {
		return nil, err
	}
	return out, nil
}

// SetLegalHold places or lifts a hold.
//
// Permitted at any stage before retirement, because a hold arrives when the legal position changes
// rather than when the process is convenient. It blocks release and retirement and nothing else:
// a hold that also blocked the freeze would keep access open on a Tenant that is leaving.
func (s *Service) SetLegalHold(ctx context.Context, offboardingID id.UUID, hold bool, reason string) (Offboarding, error) {
	if strings.TrimSpace(reason) == "" {
		return Offboarding{}, fmt.Errorf("%w: a reason is required to change a legal hold", ErrInvalid)
	}

	var record Offboarding
	if err := db.WithProviderScope(ctx, s.provider, reason,
		func(ctx context.Context, tx db.Tx) error {
			loaded, err := load(ctx, tx, offboardingID)
			if err != nil {
				return err
			}
			if loaded.Stage == StageRetired {
				return fmt.Errorf("%w: %s is retired", ErrStageRefused, offboardingID)
			}
			if _, err := tx.Exec(ctx, `UPDATE operation.offboarding
			    SET legal_hold = $2 WHERE offboarding_id = $1`,
				offboardingID.String(), hold); err != nil {
				return fmt.Errorf("offboarding: set legal hold: %w", err)
			}
			loaded.LegalHold = hold
			record = loaded
			return nil
		}); err != nil {
		return Offboarding{}, err
	}
	return record, nil
}

func loadObligation(ctx context.Context, tx db.Tx, obligationID id.UUID) (Obligation, error) {
	var (
		obligation                               Obligation
		rawObligation, rawOffboarding, rawTenant string
		rawState                                 string
	)
	if err := tx.QueryRow(ctx, selectObligation, obligationID.String()).Scan(
		&rawObligation, &rawOffboarding, &rawTenant, &obligation.Domain,
		&obligation.Type, &rawState, &obligation.DueAt, &obligation.CompletedAt,
		&obligation.Detail); err != nil {
		return Obligation{}, fmt.Errorf("%w: obligation %s", ErrNotFound, obligationID)
	}

	for target, raw := range map[*id.UUID]string{
		&obligation.ObligationID:  rawObligation,
		&obligation.OffboardingID: rawOffboarding,
		&obligation.TenantID:      rawTenant,
	} {
		parsed, err := id.Parse(raw)
		if err != nil {
			return Obligation{}, fmt.Errorf("offboarding: stored identifier %q: %w", raw, err)
		}
		*target = parsed
	}

	obligation.State = ObligationState(rawState)
	if !obligation.State.Valid() {
		return Obligation{}, fmt.Errorf("offboarding: stored state %q is not an obligation state", rawState)
	}
	return obligation, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}
