package offboarding

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/system"
	"github.com/anshacerbia2/organization-control/internal/tenant"
)

// Service coordinates the offboarding of one Tenant.
//
// It holds both pools, and the split is the authority model rather than an accident. Beginning and
// retiring an offboarding are provider decisions on a Tenant — `organization_rt` cannot even read
// the sponsoring Organization — while the freeze mutates Memberships inside one Tenant, which is
// exactly what the tenant-scoped role and its policy exist for. Using the provider pool for the
// freeze would run a bulk write across a policy that does not constrain it to the Tenant being
// offboarded.
type Service struct {
	provider   *db.ProviderPool
	tenantPool *db.TenantPool

	tenants     *tenant.Service
	memberships *membership.Service

	now   func() time.Time
	newID func() (id.UUID, error)

	// beforeAdvance runs after a stage's work and before the stage column moves, and is nil
	// outside tests. Resumability is the exit criterion of this design, and the only honest way to
	// assert it is to fail between the work and the record of the work.
	beforeAdvance func(context.Context) error
}

// New constructs the service.
func New(provider *db.ProviderPool, tenantPool *db.TenantPool,
	tenants *tenant.Service, memberships *membership.Service) (*Service, error) {
	switch {
	case provider == nil:
		return nil, errors.New("offboarding: a provider-scoped pool is required")
	case tenantPool == nil:
		return nil, errors.New("offboarding: a tenant-scoped pool is required")
	case tenants == nil:
		return nil, errors.New("offboarding: a tenant service is required")
	case memberships == nil:
		return nil, errors.New("offboarding: a membership service is required")
	}
	return &Service{
		provider: provider, tenantPool: tenantPool,
		tenants: tenants, memberships: memberships,
		now: time.Now, newID: id.NewV7,
	}, nil
}

// BeginRequest starts an offboarding.
type BeginRequest struct {
	TenantID id.UUID

	// ExpectedVersion is the Tenant `version` the operator was shown. Required for the same
	// reason as every other Tenant mutation: two operators acting from two stale views would
	// otherwise have the second write win silently, and here the write is the start of an
	// irreversible process.
	ExpectedVersion int64

	Reason    string
	LegalHold bool
}

const insertOffboarding = `INSERT INTO operation.offboarding
    (offboarding_id, tenant_id, stage, initiated_by, reason, legal_hold, correlation_id, started_at, prior_status)
VALUES ($1, $2, 'freeze', $3, $4, $5, $6, $7, $8)`

// lockTenantStatus reads the status the offboarding records as prior_status, under the row lock the
// transition then takes, so the status recorded is the one the transition moved from.
const lockTenantStatus = `SELECT status FROM tenant.tenant WHERE tenant_id = $1 FOR UPDATE`

// Begin transitions the Tenant into offboarding and creates the record the process resumes from,
// in one transaction.
//
// Both or neither. A Tenant in `offboarding` with no offboarding record has frozen access with
// nothing to resume from and no stage to read; an offboarding record against a Tenant that never
// transitioned would freeze Memberships in a Tenant that consumers still consider active.
func (s *Service) Begin(ctx context.Context, req BeginRequest) (Offboarding, error) {
	switch {
	case req.TenantID.IsNil():
		return Offboarding{}, fmt.Errorf("%w: a tenant identifier is required", ErrInvalid)
	case strings.TrimSpace(req.Reason) == "":
		return Offboarding{}, fmt.Errorf("%w: a reason is required", ErrInvalid)
	}

	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Offboarding{}, db.ErrNoScope
	}

	offboardingID, err := s.newID()
	if err != nil {
		return Offboarding{}, fmt.Errorf("offboarding: mint identifier: %w", err)
	}

	record := Offboarding{
		OffboardingID: offboardingID,
		TenantID:      req.TenantID,
		Stage:         StageFreeze,
		InitiatedBy:   scope.Actor(),
		Reason:        req.Reason,
		LegalHold:     req.LegalHold,
		CorrelationID: scope.Correlation(),
		StartedAt:     s.now().UTC(),
	}

	if err := db.WithProviderScope(ctx, s.provider, req.Reason,
		func(ctx context.Context, tx db.Tx) error {
			// The status a cancellation returns the Tenant to (ADR-ORG-006 §5.2). An absent Tenant
			// reads nothing here and is refused by the transition below, as before.
			var prior string
			if err := tx.QueryRow(ctx, lockTenantStatus, req.TenantID.String()).Scan(&prior); err == nil {
				record.PriorStatus = prior
			}

			// The Tenant transition runs first, so its refusals are the ones the caller sees. A
			// Tenant already offboarding or retired is refused by the state machine, and no record
			// is written for a transition that did not happen.
			if _, err := s.tenants.TransitionWithin(ctx, tx, tenant.ActionBeginOffboarding, tenant.Command{
				TenantID:        req.TenantID,
				Reason:          req.Reason,
				ExpectedVersion: req.ExpectedVersion,
			}); err != nil {
				return err
			}

			if _, err := tx.Exec(ctx, insertOffboarding,
				record.OffboardingID.String(), record.TenantID.String(), record.InitiatedBy.String(),
				record.Reason, record.LegalHold, record.CorrelationID.String(), record.StartedAt,
				nullableText(record.PriorStatus)); err != nil {
				return fmt.Errorf("offboarding: insert record: %w", err)
			}
			if err := derive(ctx, tx, &record); err != nil {
				return err
			}

			if err := s.publish(ctx, tx, "started", record.OffboardingID, StagePayload{
				OffboardingID: record.OffboardingID, TenantID: record.TenantID,
				Stage: StageFreeze, LegalHold: record.LegalHold,
			}, record.StartedAt); err != nil {
				return err
			}
			return db.Respond(ctx, tx, record)
		}); err != nil {
		return Offboarding{}, err
	}

	return record, nil
}

const selectFreezeBatch = `SELECT membership_id::text, membership_version
FROM membership.membership
WHERE tenant_id = $1
  AND status = 'active'
ORDER BY membership_id
LIMIT $2
FOR UPDATE SKIP LOCKED`

// FreezeBatch suspends up to size active Memberships in the Tenant and reports how many it
// changed. Zero means the freeze is complete.
//
// Batched and idempotent rather than one sweep. A Tenant with a hundred thousand Memberships in one
// transaction is a lock held for minutes and a rollback that undoes every suspension; the batch
// boundary is what makes a restart continue instead of repeating. The predicate is the resume
// token: only `active` rows are selected, so a batch that already committed is simply not seen
// again, and no cursor has to survive the restart.
//
// `SKIP LOCKED` so two workers can freeze one Tenant without blocking on each other. Neither
// double-suspends: the second sees the row locked and moves on, and if it did see it the state
// machine refuses a suspension of a suspended Membership.
//
// The freeze is a provider's act inside the one Tenant (db.WithProviderInTenant): the access is
// recorded with the reason first, and the suspensions run on the tenant pool under that Tenant's
// policy. A provider scope reaching WithTenantScope is refused, so the route could not freeze
// before this.
//
// Each suspension is recorded in membership.offboarding_freeze in the same transaction, so a
// cancellation restores exactly what the freeze suspended (ADR-ORG-006 §5.2). A batch is refused once
// the Tenant is no longer `offboarding`: after a cancellation, a freeze would suspend Memberships in a
// Tenant that is back.
func (s *Service) FreezeBatch(ctx context.Context, offboardingID, tenantID id.UUID, size int, reason string) (int, error) {
	if offboardingID.IsNil() || tenantID.IsNil() {
		return 0, fmt.Errorf("%w: an offboarding and a tenant identifier are required", ErrInvalid)
	}
	if size <= 0 {
		return 0, fmt.Errorf("%w: a positive batch size is required", ErrInvalid)
	}

	var frozen int
	if err := db.WithProviderInTenant(ctx, s.provider, s.tenantPool, tenantID, reason, func(ctx context.Context, tx db.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, tenantStatusStatement, tenantID.String()).Scan(&status); err != nil {
			return fmt.Errorf("offboarding: read the Tenant's status: %w", err)
		}
		if status != string(tenant.StateOffboarding) {
			return fmt.Errorf("%w: the Tenant is %s, not offboarding; there is nothing to freeze",
				ErrStageRefused, status)
		}
		rows, err := tx.Query(ctx, selectFreezeBatch, tenantID.String(), size)
		if err != nil {
			return fmt.Errorf("offboarding: select freeze batch: %w", err)
		}
		// Each with the version the batch locked, which is the version its suspension names: the
		// freeze is held to the same optimistic check as a single suspension, and under the lock
		// it cannot be stale.
		var batch []membership.Command
		for rows.Next() {
			var (
				raw     string
				version int64
			)
			if err := rows.Scan(&raw, &version); err != nil {
				rows.Close()
				return fmt.Errorf("offboarding: scan freeze batch: %w", err)
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
			return fmt.Errorf("offboarding: read freeze batch: %w", err)
		}

		// Every suspension in this batch, and every priority event it produces, commit together.
		// A Membership suspended without its event is a context authority has withdrawn that no
		// consumer will ever hear about.
		for _, cmd := range batch {
			if _, err := s.memberships.TransitionWithin(ctx, tx, membership.ActionSuspend, cmd); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, insertFrozen,
				offboardingID.String(), tenantID.String(), cmd.MembershipID.String()); err != nil {
				return fmt.Errorf("offboarding: record the frozen Membership: %w", err)
			}
			frozen++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return frozen, nil
}

// tenantStatusStatement reads the Tenant's status under its own policy, as the tenant role may.
const tenantStatusStatement = `SELECT status FROM tenant.tenant WHERE tenant_id = $1`

// insertFrozen records one Membership the freeze suspended.
const insertFrozen = `INSERT INTO membership.offboarding_freeze (offboarding_id, tenant_id, membership_id)
VALUES ($1, $2, $3)`

const selectOffboarding = `SELECT offboarding_id::text,
       tenant_id::text,
       stage,
       initiated_by::text,
       reason,
       legal_hold,
       correlation_id::text,
       started_at,
       frozen_at,
       released_at,
       retired_at,
       coalesce(prior_status, ''),
       cancelled_by::text,
       cancel_reason,
       cancelled_at
FROM operation.offboarding
WHERE offboarding_id = $1
FOR UPDATE`

const advanceStage = `UPDATE operation.offboarding SET stage = $2 WHERE offboarding_id = $1`

const stampFrozen = `UPDATE operation.offboarding SET stage = $2, frozen_at = $3 WHERE offboarding_id = $1`

const stampReleased = `UPDATE operation.offboarding SET stage = $2, released_at = $3 WHERE offboarding_id = $1`

// CompleteFreeze advances from freeze to obligations once no active Membership remains.
//
// The check is a count rather than the caller's word for it. A caller that stopped batching early —
// a crash, a cancelled context, a miscounted loop — would otherwise advance a Tenant into
// obligations with Memberships still active, and the projection would keep serving them.
func (s *Service) CompleteFreeze(ctx context.Context, offboardingID id.UUID) (Offboarding, error) {
	return s.advance(ctx, offboardingID, StageFreeze, func(ctx context.Context, tx db.Tx, record Offboarding) error {
		var remaining int
		if err := tx.QueryRow(ctx, countActiveMemberships, record.TenantID.String()).Scan(&remaining); err != nil {
			return fmt.Errorf("offboarding: count active memberships: %w", err)
		}
		if remaining > 0 {
			// ErrStageRefused, not ErrInvalid. The request is well formed and the freeze is simply
			// not finished, so the caller's move is to run more freeze batches and try again —
			// which is what 409 tells them and what 400 would not.
			return fmt.Errorf("%w: %d active Memberships remain in the Tenant",
				ErrStageRefused, remaining)
		}
		return nil
	})
}

// insertDeprovisioning records the command sent outward, so the realized status can be correlated
// back to it.
//
// It reuses `tenant.provisioning_request`, which `TDD-organization-control-003` describes as the
// desired provisioning state sent outward and the realized state reported back. A deprovisioning is
// exactly that, and its `state` enum already carries the distinction this stage turns on:
// `unresolved` is an outcome that is neither success nor failure. A second table would duplicate
// the correlation machinery and then need its own ambiguity vocabulary.
//
// `desired_profile.operation` separates the two directions. Without it, a failed deprovisioning
// would be the most recent request for the Tenant and would refuse a later provisioning attempt on
// a flow that has nothing to do with this one.
const insertDeprovisioning = `INSERT INTO tenant.provisioning_request
    (request_id, tenant_id, desired_profile, state, correlation_id, requested_at)
VALUES ($1, $2, jsonb_build_object('operation', 'deprovision', 'offboarding_id', $3::text),
        'requested', $4, $5)`

// Release advances from obligations to release and publishes the deprovisioning command.
//
// Refused while any obligation is unresolved and while a legal hold is set. Those are the two gates
// that make completion something the registry states rather than something a caller asserts.
//
// The command is recorded in the same transaction that advances the stage. Recorded afterwards, a
// crash in between would leave an offboarding at `release` with nothing to correlate against — and
// the ambiguity gate below would then hold it forever with no way to tell that from a genuinely
// slow deprovisioning.
func (s *Service) Release(ctx context.Context, offboardingID id.UUID) (Offboarding, error) {
	requestID, err := s.newID()
	if err != nil {
		return Offboarding{}, fmt.Errorf("offboarding: mint deprovisioning identifier: %w", err)
	}

	return s.advance(ctx, offboardingID, StageObligations, func(ctx context.Context, tx db.Tx, record Offboarding) error {
		if err := s.releaseGates(ctx, tx, record); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, insertDeprovisioning,
			requestID.String(), record.TenantID.String(), record.OffboardingID.String(),
			record.CorrelationID.String(), s.now().UTC()); err != nil {
			return fmt.Errorf("offboarding: record deprovisioning command: %w", err)
		}
		return nil
	})
}

// Retire advances from release to retired, and transitions the Tenant with it.
//
// Three gates, all rechecked at the moment of the act rather than trusted from the stage that
// permitted release. An obligation can be reopened and a hold can be placed in between, and
// retirement is the irreversible half.
//
// The third gate is the deprovisioning outcome. Retirement is the point at which the estate stops
// tracking the Tenant, so retiring on an unconfirmed deprovisioning would release the last record
// of infrastructure that may still exist.
func (s *Service) Retire(ctx context.Context, offboardingID id.UUID, expectedTenantVersion int64) (Offboarding, error) {
	return s.advance(ctx, offboardingID, StageRelease, func(ctx context.Context, tx db.Tx, record Offboarding) error {
		if err := s.releaseGates(ctx, tx, record); err != nil {
			return err
		}
		if err := deprovisioningConfirmed(ctx, tx, record); err != nil {
			return err
		}
		_, err := s.tenants.TransitionWithin(ctx, tx, tenant.ActionRetire, tenant.Command{
			TenantID:        record.TenantID,
			Reason:          "offboarding " + record.OffboardingID.String() + " retiring the Tenant",
			ExpectedVersion: expectedTenantVersion,
		})
		return err
	})
}

// releaseGates are the two conditions shared by release and retirement.
//
// Shared rather than written twice, because a gate that exists at one stage and not the next is a
// gate somebody can wait out.
func (s *Service) releaseGates(ctx context.Context, tx db.Tx, record Offboarding) error {
	if record.LegalHold {
		return fmt.Errorf("%w: %s", ErrLegalHold, record.OffboardingID)
	}
	outstanding, err := outstandingObligations(ctx, tx, record.OffboardingID)
	if err != nil {
		return err
	}
	if len(outstanding) > 0 {
		return fmt.Errorf("%w: %s", ErrObligationsOutstanding, strings.Join(outstanding, ", "))
	}
	return nil
}

// deprovisioningStatement reads the most recent deprovisioning attempt for this offboarding.
//
// The most recent rather than any: a failed attempt followed by a successful retry must retire, and
// a realized attempt followed by a later failure must not.
const deprovisioningStatement = `SELECT state, coalesce(detail, '')
FROM tenant.provisioning_request
WHERE tenant_id = $1
  AND desired_profile->>'operation' = 'deprovision'
  AND desired_profile->>'offboarding_id' = $2
ORDER BY requested_at DESC, request_id DESC
LIMIT 1`

// deprovisioningConfirmed holds retirement until the deprovisioning is realized.
//
// `unresolved` is reported as ambiguous and is the case this gate exists for.
// `TDD-organization-control-003` produces it from a timeout rather than from a rejection, and a
// timeout is not proof the target did nothing — the infrastructure may have been released, or may
// not, and retiring on it would destroy the only record that could tell an operator which.
//
// `requested` and `failed` hold too, with their own message. A caller does not need them
// distinguished to know it cannot proceed, but an operator reading why does.
func deprovisioningConfirmed(ctx context.Context, tx db.Tx, record Offboarding) error {
	var state, detail string
	if err := tx.QueryRow(ctx, deprovisioningStatement,
		record.TenantID.String(), record.OffboardingID.String()).Scan(&state, &detail); err != nil {
		// No command recorded at all. Reported as ambiguous rather than as an internal error: from
		// the caller's side nothing has confirmed, and an offboarding at `release` with no command
		// is a coordination failure an operator must look at either way.
		return fmt.Errorf("%w: no deprovisioning command is recorded for %s",
			ErrAmbiguousOutcome, record.OffboardingID)
	}

	switch state {
	case "realized":
		return nil
	case "unresolved":
		return fmt.Errorf("%w: the deprovisioning of %s is unresolved (%s)",
			ErrAmbiguousOutcome, record.TenantID, detail)
	default:
		return fmt.Errorf("%w: the deprovisioning of %s is %s (%s)",
			ErrDeprovisioningIncomplete, record.TenantID, state, detail)
	}
}

// DeprovisioningOutcome is the realized status reported back for a deprovisioning command.
type DeprovisioningOutcome struct {
	OffboardingID id.UUID

	// State is `realized`, `failed`, or `unresolved`. `requested` is refused: it is the state the
	// command was recorded in, and reporting it back would be an outcome that says nothing.
	State string

	Detail string
}

const resolveDeprovisioning = `UPDATE tenant.provisioning_request
SET state = $3, resolved_at = $4, detail = $5
WHERE request_id = (
    SELECT request_id FROM tenant.provisioning_request
    WHERE tenant_id = $1
      AND desired_profile->>'operation' = 'deprovision'
      AND desired_profile->>'offboarding_id' = $2
    ORDER BY requested_at DESC, request_id DESC
    LIMIT 1
)`

// RecordDeprovisioning correlates a realized status back to the command Release sent.
//
// It records and never advances. An outcome arriving here does not retire a Tenant — retirement
// stays a deliberate act, because the alternative is infrastructure reporting success and a Tenant
// disappearing from the estate with nobody having decided that it should.
func (s *Service) RecordDeprovisioning(ctx context.Context, outcome DeprovisioningOutcome) error {
	switch outcome.State {
	case "realized", "failed", "unresolved":
	default:
		return fmt.Errorf("%w: %q is not a deprovisioning outcome", ErrInvalid, outcome.State)
	}
	if outcome.State != "realized" && strings.TrimSpace(outcome.Detail) == "" {
		return fmt.Errorf("%w: a %s deprovisioning outcome requires a detail", ErrInvalid, outcome.State)
	}

	at := s.now().UTC()
	return db.WithProviderScope(ctx, s.provider,
		"record deprovisioning outcome for "+outcome.OffboardingID.String(),
		func(ctx context.Context, tx db.Tx) error {
			record, err := load(ctx, tx, outcome.OffboardingID)
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, resolveDeprovisioning,
				record.TenantID.String(), record.OffboardingID.String(),
				outcome.State, at, outcome.Detail)
			if err != nil {
				return fmt.Errorf("offboarding: record deprovisioning outcome: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("%w: no deprovisioning command is recorded for %s",
					ErrNotFound, outcome.OffboardingID)
			}
			return nil
		})
}

// gate is a precondition evaluated inside the advancing transaction, after the row is locked.
type gate func(ctx context.Context, tx db.Tx, record Offboarding) error

func (s *Service) advance(ctx context.Context, offboardingID id.UUID, from Stage, check gate) (Offboarding, error) {
	if offboardingID.IsNil() {
		return Offboarding{}, fmt.Errorf("%w: an offboarding identifier is required", ErrInvalid)
	}
	next, ok := Next(from)
	if !ok {
		return Offboarding{}, fmt.Errorf("%w: %s is terminal", ErrStageRefused, from)
	}

	var record Offboarding
	at := s.now().UTC()

	if err := db.WithProviderScope(ctx, s.provider,
		"advance offboarding "+offboardingID.String()+" to "+string(next),
		func(ctx context.Context, tx db.Tx) error {
			loaded, err := load(ctx, tx, offboardingID)
			if err != nil {
				return err
			}
			// The recorded stage decides, not the caller's expectation. This is what makes a
			// restart safe: a process that crashed after advancing and before reporting sees the
			// new stage here and is refused rather than advancing twice.
			if loaded.Stage != from {
				return fmt.Errorf("%w: %s is at %s, not %s",
					ErrStageRefused, offboardingID, loaded.Stage, from)
			}
			if err := check(ctx, tx, loaded); err != nil {
				return err
			}

			if s.beforeAdvance != nil {
				if err := s.beforeAdvance(ctx); err != nil {
					return err
				}
			}

			switch next {
			case StageObligations:
				if _, err := tx.Exec(ctx, stampFrozen, offboardingID.String(), string(next), at); err != nil {
					return fmt.Errorf("offboarding: advance stage: %w", err)
				}
				loaded.FrozenAt = &at
			case StageRelease:
				if _, err := tx.Exec(ctx, stampReleased, offboardingID.String(), string(next), at); err != nil {
					return fmt.Errorf("offboarding: advance stage: %w", err)
				}
				loaded.ReleasedAt = &at
			case StageRetired:
				if _, err := tx.Exec(ctx, `UPDATE operation.offboarding
				    SET stage = $2, retired_at = $3 WHERE offboarding_id = $1`,
					offboardingID.String(), string(next), at); err != nil {
					return fmt.Errorf("offboarding: advance stage: %w", err)
				}
				loaded.RetiredAt = &at
			default:
				if _, err := tx.Exec(ctx, advanceStage, offboardingID.String(), string(next)); err != nil {
					return fmt.Errorf("offboarding: advance stage: %w", err)
				}
			}
			loaded.Stage = next
			if err := derive(ctx, tx, &loaded); err != nil {
				return err
			}
			record = loaded

			name, publishes := stageEventName(next)
			if publishes {
				if err := s.publish(ctx, tx, name, offboardingID, StagePayload{
					OffboardingID: offboardingID, TenantID: loaded.TenantID,
					Stage: next, LegalHold: loaded.LegalHold,
				}, at); err != nil {
					return err
				}
			}
			return db.Respond(ctx, tx, record)
		}); err != nil {
		return Offboarding{}, err
	}

	return record, nil
}

// stageEventName maps an entered stage to its event, and reports whether there is one.
//
// Retirement publishes nothing here: the Tenant transition in the same transaction publishes
// `tenant.lifecycle.retired`, and a second event for one fact would give a consumer two things to
// deduplicate and no rule for which is authoritative.
func stageEventName(entered Stage) (string, bool) {
	switch entered {
	case StageObligations:
		return "frozen", true
	case StageRelease:
		return "released", true
	default:
		return "", false
	}
}

const outstandingStatement = `SELECT domain, obligation_type, state
FROM operation.offboarding_obligation
WHERE offboarding_id = $1
  AND state IN ('open', 'failed')
ORDER BY domain, obligation_type`

// outstandingObligations names what is outstanding rather than counting it.
//
// `open` and `failed` both hold. A failure is not a resolution, and folding it into "resolved"
// would release data whose obligations are known to be unmet.
func outstandingObligations(ctx context.Context, tx db.Tx, offboardingID id.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, outstandingStatement, offboardingID.String())
	if err != nil {
		return nil, fmt.Errorf("offboarding: read outstanding obligations: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var domain, obligationType, state string
		if err := rows.Scan(&domain, &obligationType, &state); err != nil {
			return nil, fmt.Errorf("offboarding: scan obligation: %w", err)
		}
		out = append(out, fmt.Sprintf("%s/%s (%s)", domain, obligationType, state))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("offboarding: read outstanding obligations: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

func load(ctx context.Context, tx db.Tx, offboardingID id.UUID) (Offboarding, error) {
	var (
		record                                 Offboarding
		rawOffboarding, rawTenant              string
		rawInitiator, rawCorrelation, rawStage string
	)
	var rawCanceller *string
	if err := tx.QueryRow(ctx, selectOffboarding, offboardingID.String()).Scan(
		&rawOffboarding, &rawTenant, &rawStage, &rawInitiator, &record.Reason,
		&record.LegalHold, &rawCorrelation, &record.StartedAt,
		&record.FrozenAt, &record.ReleasedAt, &record.RetiredAt,
		&record.PriorStatus, &rawCanceller, &record.CancelReason, &record.CancelledAt); err != nil {
		return Offboarding{}, fmt.Errorf("%w: offboarding %s", ErrNotFound, offboardingID)
	}
	if err := decodeCanceller(&record, rawCanceller); err != nil {
		return Offboarding{}, err
	}
	return record, decodeRecord(&record, rawOffboarding, rawTenant, rawInitiator, rawCorrelation, rawStage)
}

// decodeCanceller parses who cancelled the offboarding, when anyone did.
func decodeCanceller(record *Offboarding, raw *string) error {
	if raw == nil {
		return nil
	}
	parsed, err := id.Parse(*raw)
	if err != nil {
		return fmt.Errorf("offboarding: stored cancelled_by %q: %w", *raw, err)
	}
	record.CancelledBy = &parsed
	return nil
}

// decodeRecord parses the identifiers and the stage a statement read as text.
func decodeRecord(record *Offboarding, rawOffboarding, rawTenant, rawInitiator, rawCorrelation, rawStage string) error {
	for target, raw := range map[*id.UUID]string{
		&record.OffboardingID: rawOffboarding,
		&record.TenantID:      rawTenant,
		&record.InitiatedBy:   rawInitiator,
		&record.CorrelationID: rawCorrelation,
	} {
		parsed, err := id.Parse(raw)
		if err != nil {
			return fmt.Errorf("offboarding: stored identifier %q: %w", raw, err)
		}
		*target = parsed
	}

	record.Stage = Stage(rawStage)
	if !record.Stage.Valid() {
		return fmt.Errorf("offboarding: stored stage %q is not a stage", rawStage)
	}
	return nil
}

// countActiveMemberships is what the freeze has left: the Tenant's Memberships still `active`.
const countActiveMemberships = `SELECT count(*) FROM membership.membership
WHERE tenant_id = $1 AND status = 'active'`

// The two derived parts of the view, as the columns and the join every read of it shares.
//
// The count is the one CompleteFreeze gates on and the deprovisioning is the one retirement gates
// on (deprovisioningStatement), so the view and the gates cannot disagree about either. Read in the
// statement rather than per row, so a page of a hundred is one statement and not two hundred.
const (
	derivedColumns = `(SELECT count(*) FROM membership.membership m
        WHERE m.tenant_id = o.tenant_id AND m.status = 'active'),
       (SELECT count(*) FROM membership.offboarding_freeze f
        WHERE f.offboarding_id = o.offboarding_id),
       (SELECT count(*) FROM membership.offboarding_freeze f
        JOIN membership.membership m ON m.membership_id = f.membership_id
        WHERE f.offboarding_id = o.offboarding_id AND o.stage = 'cancelled'
          AND f.restored_at IS NULL AND m.status = 'suspended'),
       d.state,
       d.detail,
       d.requested_at,
       d.resolved_at`

	deprovisioningJoin = `LEFT JOIN LATERAL (
    SELECT r.state, r.detail, r.requested_at, r.resolved_at
    FROM tenant.provisioning_request r
    WHERE r.tenant_id = o.tenant_id
      AND r.desired_profile->>'operation' = 'deprovision'
      AND r.desired_profile->>'offboarding_id' = o.offboarding_id::text
    ORDER BY r.requested_at DESC, r.request_id DESC
    LIMIT 1
) d ON true`

	recordColumns = `o.offboarding_id::text,
       o.tenant_id::text,
       o.stage,
       o.initiated_by::text,
       o.reason,
       o.legal_hold,
       o.correlation_id::text,
       o.started_at,
       o.frozen_at,
       o.released_at,
       o.retired_at,
       coalesce(o.prior_status, ''),
       o.cancelled_by::text,
       o.cancel_reason,
       o.cancelled_at,
       ` + derivedColumns
)

// derivedStatement reads the derived parts for a record a mutation already holds.
const derivedStatement = `SELECT ` + derivedColumns + `
FROM operation.offboarding o
` + deprovisioningJoin + `
WHERE o.offboarding_id = $1`

// viewStatement reads one whole offboarding without locking it.
const viewStatement = `SELECT ` + recordColumns + `
FROM operation.offboarding o
` + deprovisioningJoin + `
WHERE o.offboarding_id = $1`

// listStatement is one keyset page of offboardings, in the order they began.
const listStatement = `SELECT ` + recordColumns + `
FROM operation.offboarding o
` + deprovisioningJoin + `
WHERE ($1::text = '' OR o.stage = $1::text)
  AND ($2::uuid IS NULL OR o.tenant_id = $2::uuid)
  AND ($3::uuid IS NULL OR o.offboarding_id > $3::uuid)
ORDER BY o.offboarding_id
LIMIT $4`

// derived holds the columns derivedColumns reads.
type derived struct {
	active, frozen, pending int
	state, detail           *string
	requestedAt, resolvedAt *time.Time
}

func (d derived) apply(record *Offboarding) {
	record.ActiveMemberships = d.active
	record.FrozenMemberships = d.frozen
	record.RestorePending = d.pending
	record.Deprovisioning = nil
	if d.state == nil || d.requestedAt == nil {
		return
	}
	record.Deprovisioning = &Deprovisioning{
		State: *d.state, Detail: d.detail, RequestedAt: *d.requestedAt, ResolvedAt: d.resolvedAt,
	}
}

// derive fills the derived parts of a record a mutation holds, in the mutation's transaction, so
// the answer describes the state the mutation produced.
func derive(ctx context.Context, tx db.Tx, record *Offboarding) error {
	var d derived
	if err := tx.QueryRow(ctx, derivedStatement, record.OffboardingID.String()).Scan(
		&d.active, &d.frozen, &d.pending, &d.state, &d.detail, &d.requestedAt, &d.resolvedAt); err != nil {
		return fmt.Errorf("offboarding: read derived view: %w", err)
	}
	d.apply(record)
	return nil
}

// errScan marks a row that could not be read, which for a single read is an absent one.
var errScan = errors.New("offboarding: scan")

// scanView reads one row of recordColumns.
func scanView(row interface{ Scan(dest ...any) error }) (Offboarding, error) {
	var (
		record                                 Offboarding
		rawOffboarding, rawTenant              string
		rawInitiator, rawCorrelation, rawStage string
		rawCanceller                           *string
		d                                      derived
	)
	if err := row.Scan(&rawOffboarding, &rawTenant, &rawStage, &rawInitiator, &record.Reason,
		&record.LegalHold, &rawCorrelation, &record.StartedAt,
		&record.FrozenAt, &record.ReleasedAt, &record.RetiredAt,
		&record.PriorStatus, &rawCanceller, &record.CancelReason, &record.CancelledAt,
		&d.active, &d.frozen, &d.pending, &d.state, &d.detail, &d.requestedAt, &d.resolvedAt); err != nil {
		return Offboarding{}, fmt.Errorf("%w: %w", errScan, err)
	}
	if err := decodeRecord(&record, rawOffboarding, rawTenant, rawInitiator, rawCorrelation, rawStage); err != nil {
		return Offboarding{}, err
	}
	if err := decodeCanceller(&record, rawCanceller); err != nil {
		return Offboarding{}, err
	}
	d.apply(&record)
	return record, nil
}

// Get reads one offboarding, which is how a restart discovers where to resume.
//
// It locks nothing. A read that took the row lock the stage advances take would queue an operator's
// screen behind a freeze, and the stage it reports is the one committed when it ran either way.
func (s *Service) Get(ctx context.Context, offboardingID id.UUID) (Offboarding, error) {
	if offboardingID.IsNil() {
		return Offboarding{}, fmt.Errorf("%w: an offboarding identifier is required", ErrInvalid)
	}
	var record Offboarding
	if err := db.WithProviderScope(ctx, s.provider,
		"read offboarding "+offboardingID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var err error
			record, err = scanView(tx.QueryRow(ctx, viewStatement, offboardingID.String()))
			if errors.Is(err, errScan) {
				return fmt.Errorf("%w: offboarding %s", ErrNotFound, offboardingID)
			}
			return err
		}); err != nil {
		return Offboarding{}, err
	}
	return record, nil
}

// ListQuery selects one page of offboardings (STD-GLB-001 1.3.0 §Pagination).
type ListQuery struct {
	// After is the last offboarding_id of the previous page; the nil identifier starts at the first.
	After id.UUID

	// Limit is the page size, 1 to db.MaxListLimit; zero takes db.DefaultListLimit.
	Limit int

	// Stage narrows the list to one stage; empty is every stage, retired included.
	Stage Stage

	// TenantID narrows it to one Tenant's offboardings; nil is every Tenant.
	TenantID id.UUID
}

// Page is one page of offboardings in the order they began. Next is the After of the following
// page, and nil on the last.
type Page struct {
	Offboardings []Offboarding
	Next         *id.UUID
}

// List reads one page of offboardings across the estate.
//
// Provider-scoped like every read here, with the caller's reason recorded before the page is read.
func (s *Service) List(ctx context.Context, query ListQuery, reason string) (Page, error) {
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return Page{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if query.Stage != "" && !query.Stage.Valid() {
		return Page{}, fmt.Errorf("%w: stage must be freeze, obligations, release or retired", ErrInvalid)
	}

	page := Page{Offboardings: []Offboarding{}}
	if err := db.WithProviderScope(ctx, s.provider, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, listStatement, string(query.Stage),
			db.Keyset(query.TenantID), db.Keyset(query.After), limit+1)
		if err != nil {
			return fmt.Errorf("offboarding: list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			record, err := scanView(rows)
			if err != nil {
				return fmt.Errorf("offboarding: scan list: %w", err)
			}
			page.Offboardings = append(page.Offboardings, record)
		}
		return rows.Err()
	}); err != nil {
		return Page{}, err
	}

	if len(page.Offboardings) > limit {
		page.Offboardings = page.Offboardings[:limit]
		next := page.Offboardings[limit-1].OffboardingID
		page.Next = &next
	}
	return page, nil
}

func (s *Service) publish(ctx context.Context, tx db.Tx, name string, aggregate id.UUID,
	payload any, at time.Time) error {
	eventType, err := EventType(name)
	if err != nil {
		return err
	}
	envelope, err := event.New(system.Source, eventType, at, payload)
	if err != nil {
		return fmt.Errorf("offboarding: build %s envelope: %w", name, err)
	}
	// The standard lane. None of these stops access — the events that do are published by the
	// Tenant transition and by each frozen Membership, and putting process progress on the
	// reserved lane would let a long offboarding delay a live revocation.
	if err := outbox.Append(ctx, tx, aggregate, envelope); err != nil {
		return fmt.Errorf("offboarding: append %s: %w", name, err)
	}
	return nil
}

// nullableText stores an empty string as NULL.
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
