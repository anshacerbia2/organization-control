package controldb

// Dead letters with no closure path, refused at deploy.
//
// An unresolved authority-bearing dead letter refuses every projection-backed check until it is
// closed, and TDD-organization-control-005 gives three ways out: REPLAYED, SUPERSEDED, and, for a
// retired consumer's incident, WAIVED. Rows written by older platform versions can lack what each
// of those needs:
//
//   - before foundation-platform v0.2.3 a dead letter kept no aggregate_id or priority, so it cannot
//     replay itself;
//   - before membership.membership_event, tenant.tenant_event and organization.provider_grant_event
//     it has no recorded version, so it cannot be superseded;
//   - a waiver is refused for an active consumer's incident.
//
// A row with all three gaps has no sanctioned closure and blocks its consumer forever. So does any
// row that names no consumer, written before v0.2.8: it counts for every consumer, and replay,
// resolve and waive address a dead letter by (event_id, consumer) (ADR-GLB-018 §5.3), so nothing can
// act on it at all. Nothing
// is in production and the databases this service has run against are rebuilt, so the count is
// expected to be zero; the post stage asserts it rather than assuming it. The decision is recorded
// in ROADMAP.md item 12.

import (
	"context"
	"fmt"
	"strings"

	"github.com/anshacerbia2/foundation-platform/db"
)

// unclosableStatement lists them. A row is waivable only when its consumer is not active; an active
// consumer's incident must be delivered, not waived.
const unclosableStatement = `SELECT d.event_id::text
FROM platform.dead_letter d
WHERE d.resolved_at IS NULL
  AND d.event_type = ANY($1)
  AND (d.consumer IS NULL
       OR ((d.aggregate_id IS NULL OR d.priority IS NULL)
           AND NOT EXISTS (SELECT 1 FROM membership.membership_event m WHERE m.event_id = d.event_id)
           AND NOT EXISTS (SELECT 1 FROM tenant.tenant_event t WHERE t.event_id = d.event_id)
           AND NOT EXISTS (SELECT 1 FROM organization.provider_grant_event g WHERE g.event_id = d.event_id)
           AND d.consumer IN (SELECT c.consumer_id FROM projection.consumer c WHERE c.retired_at IS NULL)))
ORDER BY d.event_id`

// UnclosableDeadLetters returns the unresolved dead letters of the given authority-bearing types
// that no resolution in TDD-organization-control-005 can close.
func UnclosableDeadLetters(ctx context.Context, pool *db.Pool, authorityTypes []string) ([]string, error) {
	if len(authorityTypes) == 0 {
		return nil, fmt.Errorf("controldb: no authority-bearing event types given; the check would pass vacuously")
	}
	var ids []string
	err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, unclosableStatement, authorityTypes)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("controldb: reading unclosable dead letters: %w", err)
	}
	return ids, nil
}

// UnclosableError reports them in a form an operator can act on.
func UnclosableError(ids []string) error {
	return fmt.Errorf("controldb: %d unresolved authority-bearing dead letter(s) have no closure path "+
		"-- naming no consumer, or not replayable (no aggregate_id or priority), with no recorded "+
		"version, and an active consumer's -- "+
		"and would refuse every projection-backed check forever: %s. See ROADMAP.md item 12",
		len(ids), strings.Join(ids, ", "))
}
