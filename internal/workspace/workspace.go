// Package workspace holds the collaboration and operating contexts inside one Tenant.
//
// A Workspace never moves between Tenants. Its Tenant is fixed at creation and is part of the key
// Membership references, so a move would silently reassign every Membership scoped to it — and
// nothing in the Membership row would record that it had happened.
//
// That immutability is enforced by omission rather than by a check: there is no operation that
// takes a Tenant identifier for an existing Workspace, and `workspace_tenant_scope_unique` is what
// lets the composite foreign key on Membership hold the invariant in the database.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/system"
)

// State is the Workspace lifecycle position.
type State string

const (
	// StateActive is in use.
	StateActive State = "active"

	// StateArchived is withdrawn from use and still readable. Reversible, and that is the point of
	// having it: an operator archiving the wrong Workspace has removed it from use and destroyed
	// nothing.
	StateArchived State = "archived"

	// StateRetired is terminal.
	StateRetired State = "retired"
)

// Valid reports whether the state is one this service persists. Mirrors `workspace_status_check`.
func (s State) Valid() bool {
	switch s {
	case StateActive, StateArchived, StateRetired:
		return true
	}
	return false
}

// Action is a requested transition.
type Action string

const (
	ActionArchive Action = "archive"
	ActionRestore Action = "restore"
	ActionRetire  Action = "retire"
)

// transitions is the machine as data.
//
// `retire` is reachable only from archived. `TDD-organization-control-003` draws the lifecycle as
// active → archived → retired, and the ordering is the same argument as offboarding's: archiving is
// reversible and immediate, retirement is neither, so collapsing them would make every mistaken
// retirement irreversible.
//
// `restore` is not in that diagram and is added here. An archive that could not be undone would
// make the reversibility the diagram relies on theoretical, and the recorded state carries no
// history that a restore would corrupt.
var transitions = map[Action]struct {
	from []State
	to   State
}{
	ActionArchive: {from: []State{StateActive}, to: StateArchived},
	ActionRestore: {from: []State{StateArchived}, to: StateActive},
	ActionRetire:  {from: []State{StateArchived}, to: StateRetired},
}

var (
	// ErrUnknownAction reports an action outside the machine.
	ErrUnknownAction = errors.New("workspace: action is not in the state machine")

	// ErrTransitionRefused reports a transition the machine does not permit. Maps to a 409.
	ErrTransitionRefused = errors.New("workspace: transition is not permitted from the current state")

	// ErrRetired is separate so a caller can tell "not yet" from "never again".
	ErrRetired = errors.New("workspace: a retired Workspace is terminal")

	// ErrNotFound reports an absent Workspace. Under Row-Level Security this is also what a caller
	// bound to another Tenant sees, which is correct: telling it the Workspace exists elsewhere
	// would disclose a row it may not read.
	ErrNotFound = errors.New("workspace: not found")

	// ErrVersionMismatch reports that the caller acted on a view that has since changed.
	ErrVersionMismatch = errors.New("workspace: the expected version does not match the stored one")

	// ErrMembershipsPresent refuses retirement while a Membership still references the Workspace.
	//
	// The composite foreign key would refuse the delete, but nothing deletes here — the row is
	// retired in place, so the constraint never fires and a retired Workspace would keep every
	// Membership scoped to a context that no longer exists. The refusal names them.
	ErrMembershipsPresent = errors.New("workspace: Memberships still reference this Workspace")
)

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

// Actions returns every action in the machine.
func Actions() []Action {
	all := make([]Action, 0, len(transitions))
	for action := range transitions {
		all = append(all, action)
	}
	return all
}

// Workspace is one row of workspace.workspace.
type Workspace struct {
	WorkspaceID id.UUID
	TenantID    id.UUID
	DisplayName string

	// Type is what kind of context this is. Free text rather than an enumeration: the set differs
	// per product and `TDD-organization-control-003` declares no check constraint on it, so an
	// enumeration here would be this service inventing a vocabulary for domains that own it.
	Type string

	Status    State
	Version   int64
	CreatedAt time.Time
}

var eventTypes = map[Action]string{
	ActionArchive: "com.scnehaux.organization.workspace.lifecycle.archived",
	ActionRestore: "com.scnehaux.organization.workspace.lifecycle.restored",
	ActionRetire:  "com.scnehaux.organization.workspace.lifecycle.retired",
}

const createdEventType = "com.scnehaux.organization.workspace.lifecycle.created"

// EventType returns the validated type for an action.
func EventType(action Action) (event.Type, error) {
	raw, ok := eventTypes[action]
	if !ok {
		return "", fmt.Errorf("%w: %q has no event type", ErrUnknownAction, action)
	}
	return event.ParseType(raw)
}

// Payload is the body every Workspace event carries.
//
// It carries the Tenant, because a consumer keying a Workspace without its Tenant would hold a
// Workspace identifier it cannot scope — and every Membership referencing it is scoped by the pair.
type Payload struct {
	WorkspaceID id.UUID `json:"workspace_id"`
	TenantID    id.UUID `json:"tenant_id"`
	DisplayName string  `json:"display_name"`
	Type        string  `json:"workspace_type"`
	Status      State   `json:"workspace_status"`
	Version     int64   `json:"workspace_version"`
}

// NewPayload builds the body for a committed change.
func NewPayload(w Workspace) Payload {
	return Payload{
		WorkspaceID: w.WorkspaceID,
		TenantID:    w.TenantID,
		DisplayName: w.DisplayName,
		Type:        w.Type,
		Status:      w.Status,
		Version:     w.Version,
	}
}

// Service performs authoritative Workspace mutations.
//
// Tenant-scoped, and this is the one lifecycle aggregate that is. A Workspace lives inside exactly
// one Tenant, so the policy constrains every read and write to the bound Tenant and a Tenant
// administrator can manage its own Workspaces without provider authority.
type Service struct {
	pool  *db.TenantPool
	now   func() time.Time
	newID func() (id.UUID, error)
}

// New constructs the service.
func New(pool *db.TenantPool) (*Service, error) {
	if pool == nil {
		return nil, errors.New("workspace: a tenant-scoped pool is required")
	}
	return &Service{pool: pool, now: time.Now, newID: id.NewV7}, nil
}

// CreateRequest creates a Workspace inside the bound Tenant.
type CreateRequest struct {
	DisplayName string
	Type        string

	// TenantID is optional and checked rather than used. SAD-004 §8.3 makes a Tenant identifier
	// arriving with a request a *requested* scope; the authoritative one comes from the bound
	// scope, and a mismatch is refused before any statement runs.
	TenantID id.UUID
}

const insertStatement = `INSERT INTO workspace.workspace
    (workspace_id, tenant_id, display_name, workspace_type, status)
VALUES ($1, $2, $3, $4, 'active')
RETURNING version, created_at`

// Create makes an active Workspace in the bound Tenant.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Workspace, error) {
	switch {
	case strings.TrimSpace(req.DisplayName) == "":
		return Workspace{}, errors.New("workspace: a display name is required")
	case strings.TrimSpace(req.Type) == "":
		return Workspace{}, errors.New("workspace: a workspace type is required")
	}

	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Workspace{}, db.ErrNoScope
	}
	if !req.TenantID.IsNil() && req.TenantID != scope.TenantID() {
		return Workspace{}, fmt.Errorf(
			"workspace: the request names Tenant %s and the bound scope is %s",
			req.TenantID, scope.TenantID())
	}

	workspaceID, err := s.newID()
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: mint identifier: %w", err)
	}

	record := Workspace{
		WorkspaceID: workspaceID,
		TenantID:    scope.TenantID(),
		DisplayName: req.DisplayName,
		Type:        req.Type,
		Status:      StateActive,
	}
	at := s.now().UTC()

	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		if err := tx.QueryRow(ctx, insertStatement,
			record.WorkspaceID.String(), record.TenantID.String(),
			record.DisplayName, record.Type).Scan(&record.Version, &record.CreatedAt); err != nil {
			return fmt.Errorf("workspace: insert: %w", err)
		}
		return s.publish(ctx, tx, createdEventType, record, at)
	}); err != nil {
		return Workspace{}, err
	}

	return record, nil
}

// Command is one requested transition.
type Command struct {
	WorkspaceID     id.UUID
	ExpectedVersion int64
}

func (c Command) validate() error {
	switch {
	case c.WorkspaceID.IsNil():
		return errors.New("workspace: a workspace identifier is required")
	case c.ExpectedVersion <= 0:
		return errors.New("workspace: the expected version the caller was shown is required")
	}
	return nil
}

// Archive withdraws the Workspace from use, reversibly.
func (s *Service) Archive(ctx context.Context, cmd Command) (Workspace, error) {
	return s.transition(ctx, ActionArchive, cmd)
}

// Restore returns an archived Workspace to active.
func (s *Service) Restore(ctx context.Context, cmd Command) (Workspace, error) {
	return s.transition(ctx, ActionRestore, cmd)
}

// Retire ends the Workspace, and is refused while a Membership still references it.
func (s *Service) Retire(ctx context.Context, cmd Command) (Workspace, error) {
	return s.transition(ctx, ActionRetire, cmd)
}

const selectForUpdate = `SELECT workspace_id::text,
       tenant_id::text,
       display_name,
       workspace_type,
       status,
       version,
       created_at
FROM workspace.workspace
WHERE workspace_id = $1
FOR UPDATE`

const updateStatement = `UPDATE workspace.workspace
SET status = $2,
    version = version + 1,
    updated_at = now()
WHERE workspace_id = $1
RETURNING version`

// referencingStatement names the Memberships that block retirement.
//
// Non-revoked only. A revoked Membership scoped to the Workspace is a historical record that no
// longer grants anything, so blocking on it would make a Workspace unretireable forever once
// anybody's access there had been withdrawn.
const referencingStatement = `SELECT membership_id::text, status
FROM membership.membership
WHERE workspace_id = $1
  AND status <> 'revoked'
ORDER BY membership_id`

func (s *Service) transition(ctx context.Context, action Action, cmd Command) (Workspace, error) {
	if err := cmd.validate(); err != nil {
		return Workspace{}, err
	}
	if _, ok := db.ScopeFrom(ctx); !ok {
		return Workspace{}, db.ErrNoScope
	}

	var record Workspace
	at := s.now().UTC()

	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		current, err := load(ctx, tx, cmd.WorkspaceID)
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
			referencing, err := referencingMemberships(ctx, tx, cmd.WorkspaceID)
			if err != nil {
				return err
			}
			if len(referencing) > 0 {
				return fmt.Errorf("%w: %s", ErrMembershipsPresent, strings.Join(referencing, ", "))
			}
		}

		var updated int64
		if err := tx.QueryRow(ctx, updateStatement,
			cmd.WorkspaceID.String(), string(next)).Scan(&updated); err != nil {
			return fmt.Errorf("workspace: update status: %w", err)
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
		return Workspace{}, err
	}

	return record, nil
}

func referencingMemberships(ctx context.Context, tx db.Tx, workspaceID id.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, referencingStatement, workspaceID.String())
	if err != nil {
		return nil, fmt.Errorf("workspace: read referencing Memberships: %w", err)
	}
	defer rows.Close()

	var referencing []string
	for rows.Next() {
		var membershipID, status string
		if err := rows.Scan(&membershipID, &status); err != nil {
			return nil, fmt.Errorf("workspace: scan referencing Membership: %w", err)
		}
		referencing = append(referencing, fmt.Sprintf("%s (%s)", membershipID, status))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read referencing Memberships: %w", err)
	}
	return referencing, nil
}

// ReferencingMemberships names what blocks retirement, for an operator rather than for the gate.
// The same query the gate runs, so the screen and the refusal cannot disagree.
func (s *Service) ReferencingMemberships(ctx context.Context, workspaceID id.UUID) ([]string, error) {
	var referencing []string
	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		var err error
		referencing, err = referencingMemberships(ctx, tx, workspaceID)
		return err
	}); err != nil {
		return nil, err
	}
	return referencing, nil
}

// Get reads one Workspace in the bound Tenant.
func (s *Service) Get(ctx context.Context, workspaceID id.UUID) (Workspace, error) {
	var record Workspace
	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		var err error
		record, err = load(ctx, tx, workspaceID)
		return err
	}); err != nil {
		return Workspace{}, err
	}
	return record, nil
}

func load(ctx context.Context, tx db.Tx, workspaceID id.UUID) (Workspace, error) {
	var (
		record           Workspace
		rawID, rawTenant string
		status           string
	)
	if err := tx.QueryRow(ctx, selectForUpdate, workspaceID.String()).Scan(
		&rawID, &rawTenant, &record.DisplayName, &record.Type, &status,
		&record.Version, &record.CreatedAt); err != nil {
		// Under Row-Level Security a Workspace in another Tenant is simply absent, which is the
		// correct answer: reporting that it exists elsewhere would disclose a row this caller may
		// not read.
		return Workspace{}, fmt.Errorf("%w: %s", ErrNotFound, workspaceID)
	}

	parsed, err := id.Parse(rawID)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: stored identifier %q: %w", rawID, err)
	}
	tenantID, err := id.Parse(rawTenant)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: stored tenant %q: %w", rawTenant, err)
	}
	record.WorkspaceID, record.TenantID = parsed, tenantID

	record.Status = State(status)
	if !record.Status.Valid() {
		return Workspace{}, fmt.Errorf("workspace: stored status %q is not a state", status)
	}
	return record, nil
}

func (s *Service) publish(ctx context.Context, tx db.Tx, rawType string,
	record Workspace, at time.Time) error {
	eventType, err := event.ParseType(rawType)
	if err != nil {
		return fmt.Errorf("workspace: event type %q: %w", rawType, err)
	}
	envelope, err := event.New(system.Source, eventType, at, NewPayload(record))
	if err != nil {
		return fmt.Errorf("workspace: build envelope: %w", err)
	}
	// The standard lane. Archiving a Workspace does not withdraw a Membership — the Memberships
	// scoped to it keep their own status, and the events that stop access are theirs.
	if err := outbox.Append(ctx, tx, record.WorkspaceID, envelope); err != nil {
		return fmt.Errorf("workspace: append event: %w", err)
	}
	return nil
}
