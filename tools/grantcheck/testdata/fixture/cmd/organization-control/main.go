package main

import (
	"context"

	"github.com/anshacerbia2/organization-control/internal/access"
	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/projection"
)

// main wires a concrete recorder into a pool, as the real composition root does. Without it no
// recorder type flows into the interface the wrappers call, and the call graph has no edge for a
// derivation to follow wrongly.
func main() {
	pool := db.NewConsumerPool(nil, &access.Recorder{})
	_ = projection.OwnRecord(context.Background(), pool)
}
