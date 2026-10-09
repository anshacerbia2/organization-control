package membership

// The enforcement read: a transition's state derived from recorded evidence, never from the
// response that accepted it (ADR-ORG-004 §5.2, TDD-organization-control-002 1.10.0 §Enforcement
// Evidence).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// PropagationBudget is the propagation subtotal of TDD-organization-control-002 §Enforcement Budget.
const PropagationBudget = 10 * time.Second

// EnforcementState is how far a transition has reached.
type EnforcementState string

const (
	// EnforcementAccepted is committed and not yet published to any consumer.
	EnforcementAccepted EnforcementState = "accepted"

	// EnforcementPropagating is published and not yet applied by every subscribed consumer.
	EnforcementPropagating EnforcementState = "propagating"

	// EnforcementEnforced is applied by every subscribed consumer.
	EnforcementEnforced EnforcementState = "enforced"

	// EnforcementOverBudget is not enforced once the budget has elapsed since acceptance, or with a
	// subscribed consumer's delivery dead-lettered.
	EnforcementOverBudget EnforcementState = "over_budget"
)

// The evidence one consumer has for an event.
const (
	EvidenceConsumerApplied   = "consumer_applied"
	EvidenceTransportAccepted = "transport_accepted"
	EvidencePending           = "pending"
	EvidenceDeadLettered      = "dead_lettered"
)

// ErrNoTransition reports a Membership with no recorded event, which only a row written before the
// event history existed can be.
var ErrNoTransition = errors.New("membership: no transition is recorded for this Membership")

// ConsumerEvidence is what one subscribed consumer has recorded for the event.
type ConsumerEvidence struct {
	ConsumerID string
	Evidence   string
	RecordedAt *time.Time
}

// Enforcement is the evidence for a Membership's latest transition and the state it supports.
type Enforcement struct {
	MembershipID id.UUID
	EventID      id.UUID
	Transition   Action
	AcceptedAt   time.Time
	PublishedAt  *time.Time
	Budget       time.Duration
	Consumers    []ConsumerEvidence
	State        EnforcementState
	EvaluatedAt  time.Time
}

// latestEventStatement is the Membership's latest transition, read under the Tenant's policy.
const latestEventStatement = `SELECT event_id::text, event_type, recorded_at
FROM membership.membership_event
WHERE membership_id = $1
ORDER BY membership_version DESC
LIMIT 1`

// evidenceStatement reads, for one event, every delivery it was owed and what each consumer has
// recorded. The deliveries are the subscribed consumers: outbox.Append wrote one per consumer whose
// subscription named the type when the event committed. One abandoned at its consumer's retirement
// is neither evidence nor debt, and is left out.
const evidenceStatement = `SELECT d.consumer,
       d.published_at,
       r.evidence,
       r.recorded_at,
       (dl.event_id IS NOT NULL)
FROM platform.outbox_delivery d
LEFT JOIN platform.delivery_receipt r
       ON r.event_id = d.event_id AND r.consumer = d.consumer
LEFT JOIN platform.dead_letter dl
       ON dl.event_id = d.event_id AND dl.consumer = d.consumer AND dl.resolved_at IS NULL
WHERE d.event_id = $1
  AND (d.failure_class IS NULL OR d.failure_class <> 'abandoned')
ORDER BY d.consumer`

// Enforcement reads the evidence for the Membership's latest transition.
//
// Tenant-scoped, read-only: the event is found under the Tenant's policy, so another Tenant's
// Membership is absent, and the platform evidence is reached only through that event's identifier.
func (s *Service) Enforcement(ctx context.Context, membershipID id.UUID) (Enforcement, error) {
	if membershipID.IsNil() {
		return Enforcement{}, fmt.Errorf("%w: a membership identifier is required", ErrInvalid)
	}
	var report Enforcement
	if err := db.WithTenantRead(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		var err error
		report, err = s.enforcementWithin(ctx, tx, membershipID)
		return err
	}); err != nil {
		return Enforcement{}, err
	}
	return report, nil
}

// EnforcementInTenant is the same read for a provider, inside the one Tenant it names
// (TDD-organization-control-002 1.15.0 §Enforcement Evidence).
//
// db.WithProviderInTenant records the access with the provider's reason and that Tenant before it
// reads, so the Tenant's administrator sees the read in its provider-access record
// (ADR-ORG-002 §5.6). The read itself runs under the Tenant's policy, as the Tenant administrator's
// does: a Membership of another Tenant is absent, whatever identifier the path carries.
func (s *Service) EnforcementInTenant(ctx context.Context, tenantID, membershipID id.UUID, reason string) (Enforcement, error) {
	switch {
	case s.provider == nil:
		return Enforcement{}, errors.New("membership: a provider pool is required for a provider's read")
	case membershipID.IsNil() || tenantID.IsNil():
		return Enforcement{}, fmt.Errorf("%w: a tenant and a membership identifier are required", ErrInvalid)
	}
	var report Enforcement
	if err := db.WithProviderInTenant(ctx, s.provider, s.pool, tenantID, reason, func(ctx context.Context, tx db.Tx) error {
		var err error
		report, err = s.enforcementWithin(ctx, tx, membershipID)
		return err
	}); err != nil {
		return Enforcement{}, err
	}
	return report, nil
}

// enforcementWithin reads the evidence inside a transaction bound to one Tenant.
func (s *Service) enforcementWithin(ctx context.Context, tx db.Tx, membershipID id.UUID) (Enforcement, error) {
	report := Enforcement{MembershipID: membershipID, Budget: PropagationBudget, Consumers: []ConsumerEvidence{}}
	found, err := latestEvent(ctx, tx, &report)
	if err != nil {
		return Enforcement{}, err
	}
	if !found {
		if _, err := load(ctx, tx, selectOne, membershipID); err != nil {
			return Enforcement{}, err
		}
		return Enforcement{}, fmt.Errorf("%w: %s", ErrNoTransition, membershipID)
	}

	rows, err := tx.Query(ctx, evidenceStatement, report.EventID.String())
	if err != nil {
		return Enforcement{}, fmt.Errorf("membership: read enforcement evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			consumer     string
			publishedAt  *time.Time
			evidence     *string
			recordedAt   *time.Time
			deadLettered bool
		)
		if err := rows.Scan(&consumer, &publishedAt, &evidence, &recordedAt, &deadLettered); err != nil {
			return Enforcement{}, fmt.Errorf("membership: scan enforcement evidence: %w", err)
		}
		if publishedAt != nil && (report.PublishedAt == nil || publishedAt.Before(*report.PublishedAt)) {
			at := *publishedAt
			report.PublishedAt = &at
		}
		report.Consumers = append(report.Consumers, consumerEvidence(consumer, evidence, recordedAt, deadLettered))
	}
	if err := rows.Err(); err != nil {
		return Enforcement{}, err
	}
	report.EvaluatedAt = s.now().UTC()
	report.State = report.derive()
	return report, nil
}

func latestEvent(ctx context.Context, tx db.Tx, report *Enforcement) (bool, error) {
	rows, err := tx.Query(ctx, latestEventStatement, report.MembershipID.String())
	if err != nil {
		return false, fmt.Errorf("membership: read latest transition: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var rawEvent, eventType string
	if err := rows.Scan(&rawEvent, &eventType, &report.AcceptedAt); err != nil {
		return false, fmt.Errorf("membership: scan latest transition: %w", err)
	}
	eventID, err := id.Parse(rawEvent)
	if err != nil {
		return false, fmt.Errorf("membership: stored event identifier %q: %w", rawEvent, err)
	}
	report.EventID = eventID
	report.Transition = actionOf(eventType)
	return true, nil
}

// consumerEvidence orders what one consumer has recorded. Applied is enforcement whatever else is
// recorded; an unresolved dead letter outranks a transport acceptance, because delivery that then
// failed is not delivery the operator can wait on.
func consumerEvidence(consumer string, evidence *string, recordedAt *time.Time, deadLettered bool) ConsumerEvidence {
	switch {
	case evidence != nil && *evidence == EvidenceConsumerApplied:
		return ConsumerEvidence{ConsumerID: consumer, Evidence: EvidenceConsumerApplied, RecordedAt: recordedAt}
	case deadLettered:
		return ConsumerEvidence{ConsumerID: consumer, Evidence: EvidenceDeadLettered}
	case evidence != nil && *evidence == EvidenceTransportAccepted:
		return ConsumerEvidence{ConsumerID: consumer, Evidence: EvidenceTransportAccepted, RecordedAt: recordedAt}
	default:
		return ConsumerEvidence{ConsumerID: consumer, Evidence: EvidencePending}
	}
}

// derive is the state the evidence supports (ADR-ORG-004 §5.2). An event owed to no consumer is
// enforced: no projection holds the Membership, so authority is the only copy.
func (e Enforcement) derive() EnforcementState {
	enforced, deadLettered := true, false
	for _, consumer := range e.Consumers {
		if consumer.Evidence != EvidenceConsumerApplied {
			enforced = false
		}
		if consumer.Evidence == EvidenceDeadLettered {
			deadLettered = true
		}
	}
	switch {
	case enforced:
		return EnforcementEnforced
	case deadLettered || e.EvaluatedAt.Sub(e.AcceptedAt) > e.Budget:
		return EnforcementOverBudget
	case e.PublishedAt != nil:
		return EnforcementPropagating
	default:
		return EnforcementAccepted
	}
}

// actionOf names the transition an event type records.
func actionOf(eventType string) Action {
	for action, name := range eventTypes {
		if name == eventType {
			return action
		}
	}
	return Action(eventType)
}
