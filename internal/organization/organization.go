// Package organization holds the enterprise party registry.
//
// An Organization is a party in the ecosystem — provider, customer, partner, or publisher. It is
// not a Tenant, not a Subscriber Account, and not a BPO Client Account, and ADR-ORG-001 §5.2 exists
// because collapsing any two of those is the failure this registry is shaped to prevent.
//
// # It is not contained by a Tenant
//
// An Organization sponsors several Tenants, so it carries no `tenant_id` and no Row-Level Security.
// `TDD-organization-control-001` states that as a deliberate exception rather than an oversight: a
// policy scoping it to one Tenant would be wrong, and its protection is provider scope plus
// application authorization. `organization_rt` holds nothing on it at all, because a tenant-scoped
// caller with SELECT here could read every customer in the estate.
//
// # Retiring an Organization does not retire its Tenants
//
// A cascade would take an irreversible action on isolation boundaries as a side effect of a registry
// change, which is the shape of an accidental mass outage. Retirement is refused while any Tenant
// is not retired, and the refusal names them.
package organization

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
	"github.com/anshacerbia2/organization-control/internal/system"
)

// Classification is what kind of party this is.
type Classification string

const (
	ClassificationProvider  Classification = "provider"
	ClassificationCustomer  Classification = "customer"
	ClassificationPartner   Classification = "partner"
	ClassificationPublisher Classification = "publisher"
)

// Valid reports whether the classification is one this registry persists. Mirrors
// `organization_classification_check`.
func (c Classification) Valid() bool {
	switch c {
	case ClassificationProvider, ClassificationCustomer, ClassificationPartner, ClassificationPublisher:
		return true
	}
	return false
}

// State is the registry lifecycle position.
//
// Deliberately simpler than the Tenant machine and deliberately not coupled to it. A registry entry
// and an isolation boundary have different reasons to change and different consequences when they
// do.
type State string

const (
	StateActive    State = "active"
	StateSuspended State = "suspended"
	StateRetired   State = "retired"
)

// Valid reports whether the state is one this registry persists.
func (s State) Valid() bool {
	switch s {
	case StateActive, StateSuspended, StateRetired:
		return true
	}
	return false
}

// Action is a requested transition.
type Action string

const (
	ActionSuspend Action = "suspend"
	ActionRestore Action = "restore"
	ActionRetire  Action = "retire"
)

// transitions is the machine as data.
//
// `retire` is reachable from both active and suspended: an Organization being wound down may or may
// not have been suspended first, and requiring a suspension before retirement would add a step that
// changes nothing about the outcome.
var transitions = map[Action]struct {
	from []State
	to   State
}{
	ActionSuspend: {from: []State{StateActive}, to: StateSuspended},
	ActionRestore: {from: []State{StateSuspended}, to: StateActive},
	ActionRetire:  {from: []State{StateActive, StateSuspended}, to: StateRetired},
}

var (
	// ErrInvalid is a malformed request: a required field absent, a value outside its permitted
	// set, or two fields that contradict each other.
	//
	// It exists so the HTTP surface can answer 400. Before it, every validation failure here was
	// a bare errors.New, indistinguishable at the transport boundary from a failed statement --
	// so a caller who omitted a field received 500, which says the service is broken rather than
	// that the request is. Constructor guards and stored-value decoders deliberately do NOT carry
	// it: those are a process built wrong and a row that should not exist, and both are 500.
	ErrInvalid = errors.New("organization: the request is invalid")

	// ErrUnknownAction reports an action outside the machine. A programming error: a route exists
	// without a transition behind it.
	ErrUnknownAction = errors.New("organization: action is not in the state machine")

	// ErrTransitionRefused reports a transition the machine does not permit. Maps to a 409.
	ErrTransitionRefused = errors.New("organization: transition is not permitted from the current state")

	// ErrRetired is separate so a caller can tell "not yet" from "never again". A retired
	// Organization refuses every action permanently.
	ErrRetired = errors.New("organization: a retired Organization is terminal")

	// ErrNotFound reports an absent Organization.
	ErrNotFound = errors.New("organization: not found")

	// ErrVersionMismatch reports that the caller acted on a view that has since changed.
	ErrVersionMismatch = errors.New("organization: the expected version does not match the stored one")

	// ErrTenantsNotRetired refuses retirement while the Organization still sponsors a Tenant that
	// is not retired. The refusal names them.
	ErrTenantsNotRetired = errors.New("organization: sponsored Tenants are not retired")
)

// There is deliberately no cycle error and no cycle check on `parent_id`.
//
// A cycle needs a re-parent operation to create, and there is none: `TDD-organization-control-003`
// §"API / Interface" exposes create, read, suspend, restore, and retire, so the parent is fixed at
// creation and the parent named there must already exist. A new row cannot be its own ancestor.
//
// Written here rather than left as an absence, because "no cycle check" reads like an oversight and
// the reason it is safe is a property of the command surface rather than of the column. A re-parent
// command would reintroduce the possibility and would owe a check.

// Resolve reports the state an action moves to, or why it cannot.
func Resolve(action Action, current State) (State, error) {
	rule, ok := transitions[action]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownAction, action)
	}
	if current == StateRetired {
		return "", ErrRetired
	}
	for _, permitted := range rule.from {
		if permitted == current {
			return rule.to, nil
		}
	}
	return "", fmt.Errorf("%w: %s from %s", ErrTransitionRefused, action, current)
}

// Actions returns every action in the machine, so a test walks the table rather than a list beside
// it.
func Actions() []Action {
	all := make([]Action, 0, len(transitions))
	for action := range transitions {
		all = append(all, action)
	}
	return all
}

// Organization is one row of organization.organization.
type Organization struct {
	OrganizationID id.UUID
	DisplayName    string
	Classification Classification
	Status         State

	// ParentID supports internal hierarchy only. It carries no Tenant implication: a child
	// Organization does not inherit its parent's Tenants, and nothing here traverses it to decide
	// access.
	ParentID *id.UUID

	Version   int64
	CreatedAt time.Time
}

// eventTypes are the registry events.
//
// `registry` is the class, and it is deliberately neither `lifecycle` nor `security`. These describe
// a party record rather than an isolation boundary, and nothing downstream stops access on them —
// so putting them on the reserved dispatch lane would delay events that do.
var eventTypes = map[Action]string{
	ActionSuspend: "com.scnehaux.organization.organization.registry.suspended",
	ActionRestore: "com.scnehaux.organization.organization.registry.restored",
	ActionRetire:  "com.scnehaux.organization.organization.registry.retired",
}

const createdEventType = "com.scnehaux.organization.organization.registry.created"

// EventType returns the validated type for an action.
func EventType(action Action) (event.Type, error) {
	raw, ok := eventTypes[action]
	if !ok {
		return "", fmt.Errorf("%w: %q has no event type", ErrUnknownAction, action)
	}
	return event.ParseType(raw)
}

// Payload is the body every registry event carries.
type Payload struct {
	OrganizationID id.UUID        `json:"organization_id"`
	DisplayName    string         `json:"display_name"`
	Classification Classification `json:"classification"`
	Status         State          `json:"status"`
	ParentID       *id.UUID       `json:"parent_id"`
	Version        int64          `json:"version"`
}

// NewPayload builds the body for a committed change.
func NewPayload(o Organization) Payload {
	return Payload{
		OrganizationID: o.OrganizationID,
		DisplayName:    o.DisplayName,
		Classification: o.Classification,
		Status:         o.Status,
		ParentID:       o.ParentID,
		Version:        o.Version,
	}
}

// Service performs authoritative Organization mutations.
//
// Provider-scoped, because an Organization is not contained by a Tenant and `organization_rt` holds
// no privilege on the table at all.
type Service struct {
	pool  *db.ProviderPool
	now   func() time.Time
	newID func() (id.UUID, error)
}

// New constructs the service.
func New(pool *db.ProviderPool) (*Service, error) {
	if pool == nil {
		return nil, errors.New("organization: a provider-scoped pool is required")
	}
	return &Service{pool: pool, now: time.Now, newID: id.NewV7}, nil
}

// RegisterRequest creates an Organization.
type RegisterRequest struct {
	DisplayName    string
	Classification Classification

	// ParentID is optional. A provider Organization may hold customer Organizations beneath it,
	// and the relationship is one parent at most — self-referential rather than a hierarchy table.
	ParentID *id.UUID

	Reason string
}

const insertStatement = `INSERT INTO organization.organization
    (organization_id, display_name, classification, status, parent_id)
VALUES ($1, $2, $3, 'active', $4)
RETURNING version, created_at`

// Register creates an active Organization.
//
// Active on creation, unlike a Tenant. There is no provisioning gate because there is nothing to
// provision: a registry entry is a record of a party that already exists in the world, and the
// thing a Tenant waits for — an isolation boundary — is not part of it.
func (s *Service) Register(ctx context.Context, req RegisterRequest) (Organization, error) {
	switch {
	case strings.TrimSpace(req.DisplayName) == "":
		return Organization{}, fmt.Errorf("%w: a display name is required", ErrInvalid)
	case !req.Classification.Valid():
		return Organization{}, fmt.Errorf("%w: classification %q is not a declared classification", ErrInvalid,
			req.Classification)
	case strings.TrimSpace(req.Reason) == "":
		return Organization{}, fmt.Errorf("%w: a reason is required", ErrInvalid)
	}

	organizationID, err := s.newID()
	if err != nil {
		return Organization{}, fmt.Errorf("organization: mint identifier: %w", err)
	}

	record := Organization{
		OrganizationID: organizationID,
		DisplayName:    req.DisplayName,
		Classification: req.Classification,
		Status:         StateActive,
		ParentID:       req.ParentID,
	}
	at := s.now().UTC()

	if err := db.WithProviderScope(ctx, s.pool, req.Reason,
		func(ctx context.Context, tx db.Tx) error {
			if req.ParentID != nil {
				// The parent must exist and must not be retired. Attaching a child to a retired
				// party would create a live record whose ancestor has been wound down, which no
				// report of the hierarchy could present coherently.
				var parentStatus string
				if err := tx.QueryRow(ctx,
					`SELECT status FROM organization.organization WHERE organization_id = $1`,
					req.ParentID.String()).Scan(&parentStatus); err != nil {
					return fmt.Errorf("%w: parent %s", ErrNotFound, req.ParentID)
				}
				if parentStatus == string(StateRetired) {
					return fmt.Errorf("%w: parent %s is retired", ErrTransitionRefused, req.ParentID)
				}
			}

			if err := tx.QueryRow(ctx, insertStatement,
				record.OrganizationID.String(), record.DisplayName,
				string(record.Classification), nullableUUID(record.ParentID),
			).Scan(&record.Version, &record.CreatedAt); err != nil {
				return fmt.Errorf("organization: insert: %w", err)
			}

			return s.publish(ctx, tx, createdEventType, record, at)
		}); err != nil {
		return Organization{}, err
	}

	return record, nil
}

// Command is one requested transition.
type Command struct {
	OrganizationID id.UUID
	Reason         string

	// ExpectedVersion is the version the caller was shown, required for the same reason as on a
	// Tenant: two operators acting from two stale views would otherwise have the second write win
	// silently.
	ExpectedVersion int64
}

func (c Command) validate() error {
	switch {
	case c.OrganizationID.IsNil():
		return fmt.Errorf("%w: an organization identifier is required", ErrInvalid)
	case strings.TrimSpace(c.Reason) == "":
		return fmt.Errorf("%w: a reason is required", ErrInvalid)
	case c.ExpectedVersion <= 0:
		return fmt.Errorf("%w: the expected version the caller was shown is required", ErrInvalid)
	}
	return nil
}

// Suspend withholds the party record.
func (s *Service) Suspend(ctx context.Context, cmd Command) (Organization, error) {
	return s.transition(ctx, ActionSuspend, cmd)
}

// Restore returns a suspended Organization to active.
func (s *Service) Restore(ctx context.Context, cmd Command) (Organization, error) {
	return s.transition(ctx, ActionRestore, cmd)
}

// Retire ends the party record, and is refused while it still sponsors a Tenant that is not retired.
func (s *Service) Retire(ctx context.Context, cmd Command) (Organization, error) {
	return s.transition(ctx, ActionRetire, cmd)
}

const selectForUpdate = `SELECT organization_id::text,
       display_name,
       classification,
       status,
       coalesce(parent_id::text, ''),
       version,
       created_at
FROM organization.organization
WHERE organization_id = $1
FOR UPDATE`

const updateStatement = `UPDATE organization.organization
SET status = $2,
    version = version + 1,
    updated_at = now()
WHERE organization_id = $1
RETURNING version`

// liveTenantsStatement names the Tenants that block retirement.
//
// Names them rather than counting them. An operator told "3 Tenants are not retired" has to go and
// find them, and will retire the wrong one; the list is what makes the refusal actionable.
const liveTenantsStatement = `SELECT tenant_id::text, display_name, status
FROM tenant.tenant
WHERE organization_id = $1
  AND status <> 'retired'
ORDER BY display_name, tenant_id`

func (s *Service) transition(ctx context.Context, action Action, cmd Command) (Organization, error) {
	if err := cmd.validate(); err != nil {
		return Organization{}, err
	}

	var record Organization
	at := s.now().UTC()

	if err := db.WithProviderScope(ctx, s.pool, cmd.Reason,
		func(ctx context.Context, tx db.Tx) error {
			current, err := load(ctx, tx, cmd.OrganizationID)
			if err != nil {
				return err
			}
			next, err := Resolve(action, current.Status)
			if err != nil {
				return err
			}
			if current.Version != cmd.ExpectedVersion {
				return fmt.Errorf("%w: expected %d, stored %d",
					ErrVersionMismatch, cmd.ExpectedVersion, current.Version)
			}

			if action == ActionRetire {
				live, err := liveTenants(ctx, tx, cmd.OrganizationID)
				if err != nil {
					return err
				}
				if len(live) > 0 {
					return fmt.Errorf("%w: %s", ErrTenantsNotRetired, strings.Join(live, ", "))
				}
			}

			var updated int64
			if err := tx.QueryRow(ctx, updateStatement,
				cmd.OrganizationID.String(), string(next)).Scan(&updated); err != nil {
				return fmt.Errorf("organization: update status: %w", err)
			}
			current.Status = next
			current.Version = updated
			record = current

			eventType, err := EventType(action)
			if err != nil {
				return err
			}
			return s.publish(ctx, tx, string(eventType), record, at)
		}); err != nil {
		return Organization{}, err
	}

	return record, nil
}

func liveTenants(ctx context.Context, tx db.Tx, organizationID id.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, liveTenantsStatement, organizationID.String())
	if err != nil {
		return nil, fmt.Errorf("organization: read sponsored Tenants: %w", err)
	}
	defer rows.Close()

	var live []string
	for rows.Next() {
		var tenantID, displayName, status string
		if err := rows.Scan(&tenantID, &displayName, &status); err != nil {
			return nil, fmt.Errorf("organization: scan sponsored Tenant: %w", err)
		}
		live = append(live, fmt.Sprintf("%s %s (%s)", displayName, tenantID, status))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("organization: read sponsored Tenants: %w", err)
	}
	sort.Strings(live)
	return live, nil
}

// SponsoredTenantsNotRetired names what blocks retirement, for an operator rather than for the gate.
//
// The same query the gate runs. A separate one would eventually disagree with the one that actually
// blocks, and then the screen and the refusal would say different things.
func (s *Service) SponsoredTenantsNotRetired(ctx context.Context, organizationID id.UUID) ([]string, error) {
	var live []string
	if err := db.WithProviderScope(ctx, s.pool,
		"read sponsored Tenants of "+organizationID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var err error
			live, err = liveTenants(ctx, tx, organizationID)
			return err
		}); err != nil {
		return nil, err
	}
	return live, nil
}

// Get reads one Organization.
func (s *Service) Get(ctx context.Context, organizationID id.UUID) (Organization, error) {
	var record Organization
	if err := db.WithProviderScope(ctx, s.pool,
		"read organization "+organizationID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var err error
			record, err = load(ctx, tx, organizationID)
			return err
		}); err != nil {
		return Organization{}, err
	}
	return record, nil
}

func load(ctx context.Context, tx db.Tx, organizationID id.UUID) (Organization, error) {
	var (
		record         Organization
		rawID          string
		classification string
		status         string
		rawParent      string
	)
	if err := tx.QueryRow(ctx, selectForUpdate, organizationID.String()).Scan(
		&rawID, &record.DisplayName, &classification, &status, &rawParent,
		&record.Version, &record.CreatedAt); err != nil {
		return Organization{}, fmt.Errorf("%w: %s", ErrNotFound, organizationID)
	}

	parsed, err := id.Parse(rawID)
	if err != nil {
		return Organization{}, fmt.Errorf("organization: stored identifier %q: %w", rawID, err)
	}
	record.OrganizationID = parsed
	if rawParent != "" {
		parent, err := id.Parse(rawParent)
		if err != nil {
			return Organization{}, fmt.Errorf("organization: stored parent %q: %w", rawParent, err)
		}
		record.ParentID = &parent
	}

	record.Classification = Classification(classification)
	if !record.Classification.Valid() {
		return Organization{}, fmt.Errorf("organization: stored classification %q is not declared", classification)
	}
	record.Status = State(status)
	if !record.Status.Valid() {
		return Organization{}, fmt.Errorf("organization: stored status %q is not a state", status)
	}
	return record, nil
}

func (s *Service) publish(ctx context.Context, tx db.Tx, rawType string,
	record Organization, at time.Time) error {
	eventType, err := event.ParseType(rawType)
	if err != nil {
		return fmt.Errorf("organization: event type %q: %w", rawType, err)
	}
	envelope, err := event.New(system.Source, eventType, at, NewPayload(record))
	if err != nil {
		return fmt.Errorf("organization: build envelope: %w", err)
	}
	// The standard lane. A registry change stops no access, and putting it on the reserved lane
	// would let a bulk registry import delay a live revocation.
	if err := outbox.Append(ctx, tx, record.OrganizationID, envelope); err != nil {
		return fmt.Errorf("organization: append event: %w", err)
	}
	return nil
}

func nullableUUID(value *id.UUID) any {
	if value == nil {
		return nil
	}
	return value.String()
}
