package main

import (
	"context"

	"github.com/anshacerbia2/organization-control/internal/access"
	"github.com/anshacerbia2/organization-control/internal/authority"
	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/delivery"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/posture"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
)

// main wires a concrete recorder into a pool, as the real composition root does. Without it no
// recorder type flows into the interface the wrappers call, and the call graph has no edge for a
// derivation to follow wrongly.
func main() {
	pool := db.NewConsumerPool(nil, &access.Recorder{})
	_ = projection.OwnRecord(context.Background(), pool)

	// The caller records and the bootstrap, as the real composition root builds them.
	records, grants := &authority.Reader{}, &authority.Grants{}
	_ = records.ProviderStanding(context.Background())
	_ = records.EmergencyGrants(context.Background())
	_ = records.ConsumerFor(context.Background())
	_ = grants.Bootstrap(context.Background())

	// The two scheduled sweeps, on the provider connections.
	_ = (&tenant.ScheduledSweep{}).SweepUnresolved(context.Background())
	_ = (&invitation.ScheduledExpiry{}).ExpireLapsed(context.Background())

	// The isolation posture, read at startup on the tenant connections.
	_ = posture.AssertIsolation(context.Background(), nil)

	// The dispatchers' registration check, on the dispatch pool: out of the tool's scope.
	_ = delivery.Run(context.Background(), nil)
}
