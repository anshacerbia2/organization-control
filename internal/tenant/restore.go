package tenant

// After a restore to an older point (ADR-ORG-003 §5.5, amended 2026-10-20; TDD-organization-control-002
// §After a Restore to an Older Point). The restore-bump stage adds a margin to every Tenant's version
// and security version, then calls PublishWithdrawn, in one transaction with no actor, before the
// service serves.

import (
	"context"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/system"
)

// withdrawnStatement reads every Tenant whose state withholds access: suspended, offboarding and
// retired. Every other state is left unpublished. An active Tenant republished could give back
// what a withdrawal the restore lost had taken away, and a Tenant never active was never projected.
const withdrawnStatement = `SELECT tenant_id::text, organization_id::text, status, version, tenant_security_version
FROM tenant.tenant
WHERE status IN ('suspended', 'offboarding', 'retired')
ORDER BY tenant_id`

// restoreActions names, for each withdrawing state, the transition whose event type every consumer
// already applies for it: the same types the transitions into those states publish.
var restoreActions = map[State]Action{
	StateSuspended:   ActionSuspend,
	StateOffboarding: ActionBeginOffboarding,
	StateRetired:     ActionRetire,
}

// PublishWithdrawn appends, inside tx, one event for each Tenant whose state withholds access, at the
// versions the Tenant now holds, and records each event's security version as a transition does. It
// returns how many it published. It publishes no active Tenant: a restore never widens access.
//
// tx must be able to read and write every Tenant: the restore's own connection, which runs before
// any Tenant is bound.
func PublishWithdrawn(ctx context.Context, tx db.Tx, at time.Time) (int, error) {
	rows, err := tx.Query(ctx, withdrawnStatement)
	if err != nil {
		return 0, fmt.Errorf("tenant: reading the withdrawn Tenants: %w", err)
	}
	var records []Tenant
	for rows.Next() {
		var (
			record                 Tenant
			rawTenant, rawOrg, raw string
		)
		if err := rows.Scan(&rawTenant, &rawOrg, &raw, &record.Version, &record.SecurityVersion); err != nil {
			rows.Close()
			return 0, fmt.Errorf("tenant: reading a withdrawn Tenant: %w", err)
		}
		if record.TenantID, err = id.Parse(rawTenant); err != nil {
			rows.Close()
			return 0, fmt.Errorf("tenant: stored identifier %q: %w", rawTenant, err)
		}
		if record.OrganizationID, err = id.Parse(rawOrg); err != nil {
			rows.Close()
			return 0, fmt.Errorf("tenant: stored organization %q: %w", rawOrg, err)
		}
		record.Status = State(raw)
		records = append(records, record)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("tenant: reading the withdrawn Tenants: %w", err)
	}

	for _, record := range records {
		action := restoreActions[record.Status]
		eventType, _, err := EventType(action)
		if err != nil {
			return 0, err
		}
		envelope, err := event.New(system.Source, eventType, at, NewPayload(record))
		if err != nil {
			return 0, fmt.Errorf("tenant: build envelope: %w", err)
		}
		var opts []outbox.Option
		if Priority(action) {
			opts = append(opts, outbox.Priority())
		}
		if err := outbox.Append(ctx, tx, record.TenantID, envelope, opts...); err != nil {
			return 0, fmt.Errorf("tenant: append the restored withdrawal of %s: %w", record.TenantID, err)
		}
		if _, err := tx.Exec(ctx, recordEventStatement, envelope.ID.String(), record.TenantID.String(),
			record.SecurityVersion, string(eventType)); err != nil {
			return 0, fmt.Errorf("tenant: record the restored withdrawal's security version: %w", err)
		}
	}
	return len(records), nil
}
