package projection

// The provider authority snapshot (ADR-ORG-002 §5.3, TDD-organization-control-001 §Provider
// Authority Projection): every unrevoked grant of a published scope with the activation in force,
// under one mark, for a consumer subscribed to the provider types.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// ProviderEventTypes are the provider grant event types internal/authority publishes. This package
// may not import internal/authority (arch.json), so the list is copied, and
// TestTheFrontierDebtCoversEveryAuthorityEvent in internal/httpapi keeps the copy honest.
var ProviderEventTypes = []string{
	"com.scnehaux.organization.provider.lifecycle.granted",
	"com.scnehaux.organization.provider.lifecycle.activated",
	"com.scnehaux.organization.provider.security.ended",
	"com.scnehaux.organization.provider.security.revoked",
}

// providerSnapshotScopes are authority.PublishedScopes: a grant of another scope was never
// published, so no consumer is owed it in a snapshot either. Copied for the reason above.
var providerSnapshotScopes = []string{"provider:identity-control"}

// ErrNotSubscribed refuses a snapshot of a projection the consumer does not subscribe to. A
// subscription is the consumer's declared need, and a snapshot beyond it would hand over authority
// data no delivery would ever have carried to it (NIST SP 800-53 AC-6).
var ErrNotSubscribed = errors.New("projection: the consumer does not subscribe to this projection")

// ProviderSnapshotScopes reports the scopes the provider authority snapshot returns, for the test
// that keeps the copy honest.
func ProviderSnapshotScopes() []string { return append([]string(nil), providerSnapshotScopes...) }

func subscribesToAny(subscribed, wanted []string) bool {
	for _, have := range subscribed {
		for _, want := range wanted {
			if have == want {
				return true
			}
		}
	}
	return false
}

// ProviderGrantRow is one grant as the provider grant events carry it: the same fields, so a
// snapshot row and an event about the same grant compare field by field.
type ProviderGrantRow struct {
	GrantID      id.UUID                `json:"grant_id"`
	PrincipalID  id.UUID                `json:"principal_id"`
	Scope        string                 `json:"scope"`
	Kind         string                 `json:"kind"`
	GrantStatus  string                 `json:"grant_status"`
	GrantVersion int64                  `json:"grant_version"`
	Activation   *ProviderActivationRow `json:"activation"`
}

// ProviderActivationRow is the activation in force.
type ProviderActivationRow struct {
	ActivationID id.UUID   `json:"activation_id"`
	EndsAt       time.Time `json:"ends_at"`
}

// ProviderPage is one page of the provider authority snapshot.
type ProviderPage struct {
	HighWaterMark int64              `json:"high_water_mark"`
	TakenAt       time.Time          `json:"taken_at"`
	Grants        []ProviderGrantRow `json:"grants"`
	Cursor        string             `json:"cursor,omitempty"`
}

// selectProviderGrants is the projection: unrevoked grants of the published scopes, each with its
// activation in force, keyed on grant_id. now() is the snapshot transaction's start, the instant
// the mark is read at.
const selectProviderGrants = `SELECT g.grant_id::text, g.principal_id::text, g.scope, g.kind, g.grant_version,
       coalesce(a.activation_id::text, ''), a.ends_at
  FROM organization.provider_grant g
  LEFT JOIN LATERAL (
        SELECT activation_id, ends_at
          FROM organization.provider_activation
         WHERE grant_id = g.grant_id
           AND decision = 'approved' AND ended_at IS NULL AND now() < ends_at
         ORDER BY ends_at DESC
         LIMIT 1) a ON TRUE
 WHERE g.revoked_at IS NULL
   AND g.scope = ANY ($1::text[])
   AND ($2 = '' OR g.grant_id > $2::uuid)
 ORDER BY g.grant_id
 LIMIT $3`

// ProviderSnapshot produces one page, on the provider connections.
func (p *Publisher) ProviderSnapshot(ctx context.Context, req SnapshotRequest) (ProviderPage, error) {
	size, start, err := startPage(req, p.now())
	if err != nil {
		return ProviderPage{}, err
	}
	page := ProviderPage{HighWaterMark: start.HighWaterMark, TakenAt: start.TakenAt}
	if err := db.WithProviderSnapshot(ctx, p.pool, "provider authority snapshot for "+req.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			return providerSnapshotIn(ctx, tx, req, size, &page)
		}); err != nil {
		return ProviderPage{}, err
	}
	return endProviderPage(page, size), nil
}

// ProviderSnapshot produces one page for the consumer. See Publisher.ProviderSnapshot.
func (a *ConsumerAccess) ProviderSnapshot(ctx context.Context, req SnapshotRequest) (ProviderPage, error) {
	size, start, err := startPage(req, a.now())
	if err != nil {
		return ProviderPage{}, err
	}
	page := ProviderPage{HighWaterMark: start.HighWaterMark, TakenAt: start.TakenAt}
	if err := db.WithConsumerSnapshot(ctx, a.pool, "own provider authority snapshot for "+req.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			return providerSnapshotIn(ctx, tx, req, size, &page)
		}); err != nil {
		return ProviderPage{}, err
	}
	return endProviderPage(page, size), nil
}

func providerSnapshotIn(ctx context.Context, tx db.Tx, req SnapshotRequest, size int, page *ProviderPage) error {
	var consumer Consumer
	if err := load(ctx, tx, req.ConsumerID, &consumer); err != nil {
		return err
	}
	if !subscribesToAny(consumer.EventTypes, ProviderEventTypes) {
		return fmt.Errorf("%w: %s subscribes to no provider grant event type", ErrNotSubscribed, req.ConsumerID)
	}
	if req.Cursor == "" {
		if err := tx.QueryRow(ctx, markStatement).Scan(&page.HighWaterMark); err != nil {
			return fmt.Errorf("projection: read high-water mark: %w", err)
		}
	}

	rows, err := tx.Query(ctx, selectProviderGrants, providerSnapshotScopes, req.Cursor, size)
	if err != nil {
		return fmt.Errorf("projection: read provider grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			row                               ProviderGrantRow
			rawGrant, rawPrincipal, rawActive string
			endsAt                            *time.Time
		)
		if err := rows.Scan(&rawGrant, &rawPrincipal, &row.Scope, &row.Kind, &row.GrantVersion,
			&rawActive, &endsAt); err != nil {
			return fmt.Errorf("projection: scan provider grant: %w", err)
		}
		if row.GrantID, err = id.Parse(rawGrant); err != nil {
			return fmt.Errorf("projection: stored grant id %q: %w", rawGrant, err)
		}
		if row.PrincipalID, err = id.Parse(rawPrincipal); err != nil {
			return fmt.Errorf("projection: stored principal id %q: %w", rawPrincipal, err)
		}
		row.GrantStatus = "active"
		if rawActive != "" && endsAt != nil {
			activation, err := id.Parse(rawActive)
			if err != nil {
				return fmt.Errorf("projection: stored activation id %q: %w", rawActive, err)
			}
			row.Activation = &ProviderActivationRow{ActivationID: activation, EndsAt: endsAt.UTC()}
		}
		page.Grants = append(page.Grants, row)
	}
	return rows.Err()
}

func endProviderPage(page ProviderPage, size int) ProviderPage {
	if len(page.Grants) == size {
		page.Cursor = page.Grants[len(page.Grants)-1].GrantID.String()
	}
	return page
}
