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
    (offboarding_id, tenant_id, stage, initiated_by, reason, legal_hold, correlation_id, started_at)
VALUES ($1, $2, 'freeze', $3, $4, $5, $6, $7)`

// Begin transitions the Tenant into offboarding and creates the record the process resumes from,
// in one transaction.
//
// Both or neither. A Tenant in `offboarding` with no offboarding record has frozen access with
// nothing to resume from and no stage to read; an offboarding record against a Tenant that never
// transitioned would freeze Memberships in a Tenant that consumers still consider active.
func (s *Service) Begin(ctx context.Context, req BeginRequest) (Offboarding, error) {
	switch {
	case req.TenantID.IsNil():
		return Offboarding{}, errors.New("offboarding: a tenant identifier is required")
	case strings.TrimSpace(req.Reason) == "":
		return Offboarding{}, errors.New("offboarding: a reason is required")
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
				record.Reason, record.LegalHold, record.CorrelationID.String(), record.StartedAt); err != nil {
				return fmt.Errorf("offboarding: insert record: %w", err)
			}

			return s.publish(ctx, tx, "started", record.OffboardingID, StagePayload{
				OffboardingID: record.OffboardingID, TenantID: record.TenantID,
				Stage: StageFreeze, LegalHold: record.LegalHold,
			}, record.StartedAt)
		}); err != nil {
		return Offboarding{}, err
	}

	return record, nil
}

const selectFreezeBatch = `SELECT membership_id::text
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
func (s *Service) FreezeBatch(ctx context.Context, tenantID id.UUID, size int) (int, error) {
	if tenantID.IsNil() {
		return 0, errors.New("offboarding: a tenant identifier is required")
	}
	if size <= 0 {
		return 0, errors.New("offboarding: a positive batch size is required")
	}

	var frozen int
	if err := db.WithTenantScope(ctx, s.tenantPool, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, selectFreezeBatch, tenantID.String(), size)
		if err != nil {
			return fmt.Errorf("offboarding: select freeze batch: %w", err)
		}
		var ids []id.UUID
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return fmt.Errorf("offboarding: scan freeze batch: %w", err)
			}
			parsed, err := id.Parse(raw)
			if err != nil {
				rows.Close()
				return fmt.Errorf("offboarding: stored membership id %q: %w", raw, err)
			}
			ids = append(ids, parsed)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("offboarding: read freeze batch: %w", err)
		}

		// Every suspension in this batch, and every priority event it produces, commit together.
		// A Membership suspended without its event is a context authority has withdrawn that no
		// consumer will ever hear about.
		for _, membershipID := range ids {
			if _, err := s.memberships.TransitionWithin(ctx, tx, membership.ActionSuspend, membershipID); err != nil {
				return err
			}
			frozen++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return frozen, nil
}

const selectOffboarding = `SELECT offboarding_id::text,
       tenant_id::text,
       stage,
       initiated_by::text,
       reason,
       legal_hold,
       correlation_id::text,
       started_at,
       frozen_at,
       retired_at
FROM operation.offboarding
WHERE offboarding_id = $1
FOR UPDATE`

const advanceStage = `UPDATE operation.offboarding SET stage = $2 WHERE offboarding_id = $1`

const stampFrozen = `UPDATE operation.offboarding SET stage = $2, frozen_at = $3 WHERE offboarding_id = $1`

// CompleteFreeze advances from freeze to obligations once no active Membership remains.
//
// The check is a count rather than the caller's word for it. A caller that stopped batching early —
// a crash, a cancelled context, a miscounted loop — would otherwise advance a Tenant into
// obligations with Memberships still active, and the projection would keep serving them.
func (s *Service) CompleteFreeze(ctx context.Context, offboardingID id.UUID) (Offboarding, error) {
	return s.advance(ctx, offboardingID, StageFreeze, func(ctx context.Context, tx db.Tx, record Offboarding) error {
		var remaining int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM membership.membership
		    WHERE tenant_id = $1 AND status = 'active'`, record.TenantID.String()).Scan(&remaining); err != nil {
			return fmt.Errorf("offboarding: count active memberships: %w", err)
		}
		if remaining > 0 {
			return fmt.Errorf("offboarding: %d active Memberships remain in the Tenant", remaining)
		}
		return nil
	})
}

// Release advances from obligations to release.
//
// Refused while any obligation is unresolved and while a legal hold is set. Those are the two gates
// that make completion something the registry states rather than something a caller asserts.
func (s *Service) Release(ctx context.Context, offboardingID id.UUID) (Offboarding, error) {
	return s.advance(ctx, offboardingID, StageObligations, func(ctx context.Context, tx db.Tx, record Offboarding) error {
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
	})
}

// Retire advances from release to retired, and transitions the Tenant with it.
//
// Both gates are rechecked. An obligation can be reopened and a hold can be placed between release
// and retirement, and retirement is the irreversible half — so the checks run against the state at
// the moment of the act rather than the state that permitted the previous stage.
func (s *Service) Retire(ctx context.Context, offboardingID id.UUID, expectedTenantVersion int64) (Offboarding, error) {
	return s.advance(ctx, offboardingID, StageRelease, func(ctx context.Context, tx db.Tx, record Offboarding) error {
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
		_, err = s.tenants.TransitionWithin(ctx, tx, tenant.ActionRetire, tenant.Command{
			TenantID:        record.TenantID,
			Reason:          "offboarding " + record.OffboardingID.String() + " retiring the Tenant",
			ExpectedVersion: expectedTenantVersion,
		})
		return err
	})
}

// gate is a precondition evaluated inside the advancing transaction, after the row is locked.
type gate func(ctx context.Context, tx db.Tx, record Offboarding) error

func (s *Service) advance(ctx context.Context, offboardingID id.UUID, from Stage, check gate) (Offboarding, error) {
	if offboardingID.IsNil() {
		return Offboarding{}, errors.New("offboarding: an offboarding identifier is required")
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
			record = loaded

			name, publishes := stageEventName(next)
			if !publishes {
				return nil
			}
			return s.publish(ctx, tx, name, offboardingID, StagePayload{
				OffboardingID: offboardingID, TenantID: loaded.TenantID,
				Stage: next, LegalHold: loaded.LegalHold,
			}, at)
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
	if err := tx.QueryRow(ctx, selectOffboarding, offboardingID.String()).Scan(
		&rawOffboarding, &rawTenant, &rawStage, &rawInitiator, &record.Reason,
		&record.LegalHold, &rawCorrelation, &record.StartedAt,
		&record.FrozenAt, &record.RetiredAt); err != nil {
		return Offboarding{}, fmt.Errorf("%w: offboarding %s", ErrNotFound, offboardingID)
	}

	for target, raw := range map[*id.UUID]string{
		&record.OffboardingID: rawOffboarding,
		&record.TenantID:      rawTenant,
		&record.InitiatedBy:   rawInitiator,
		&record.CorrelationID: rawCorrelation,
	} {
		parsed, err := id.Parse(raw)
		if err != nil {
			return Offboarding{}, fmt.Errorf("offboarding: stored identifier %q: %w", raw, err)
		}
		*target = parsed
	}

	record.Stage = Stage(rawStage)
	if !record.Stage.Valid() {
		return Offboarding{}, fmt.Errorf("offboarding: stored stage %q is not a stage", rawStage)
	}
	return record, nil
}

// Get reads one offboarding, which is how a restart discovers where to resume.
func (s *Service) Get(ctx context.Context, offboardingID id.UUID) (Offboarding, error) {
	var record Offboarding
	if err := db.WithProviderScope(ctx, s.provider,
		"read offboarding "+offboardingID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var err error
			record, err = load(ctx, tx, offboardingID)
			return err
		}); err != nil {
		return Offboarding{}, err
	}
	return record, nil
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
