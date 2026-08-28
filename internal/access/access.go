// Package access records the evidence a cross-Tenant transaction leaves behind.
//
// It exists because `db.PrivilegedRecorder` had no implementation outside tests. The interface made
// the recorder a mandatory constructor argument, which is a real control — the provider pool cannot
// be built without one — but a mandatory argument only guarantees that something was passed, and
// every implementation in the repository was a fake that discarded it. PAD-PLT-002 §3.3 invariant 22
// requires cross-tenant administration to carry evidence, and until this package existed that
// requirement was satisfied by the type system and by nothing that survived a restart.
//
// # Why this writes in its own transaction
//
// `db.withProviderScope` calls the recorder before it opens the domain transaction, so the write
// here commits independently. That is the point: the access an investigation asks about is the one
// that failed, panicked, or was killed mid-flight, and evidence enrolled in the domain transaction
// would roll back with exactly those cases.
//
// The cost is a row for an access whose work did not happen. That is the correct direction to fail —
// an over-recorded access is answerable, an unrecorded one is not.
package access

import (
	"context"
	"errors"
	"fmt"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// insertStatement writes one evidence row.
//
// `occurred_at` is left to the column default so the timestamp is the database's, not the process's.
// A recorder that stamped its own clock would let a replica with a skewed clock produce evidence
// that cannot be ordered against the rows the transaction went on to write.
const insertStatement = `INSERT INTO audit.privileged_access
    (access_id, actor_id, correlation_id, reason)
VALUES ($1, $2, $3, $4)`

// Recorder persists provider access to audit.privileged_access.
type Recorder struct {
	tx    db.Transactor
	newID func() (id.UUID, error)
}

// New constructs the recorder.
//
// It takes the same transaction source the provider pool is built on, deliberately: the evidence
// belongs in the same database as the work, so a deployment cannot end up with a control database it
// can write and an audit destination it cannot reach. It takes no scope binder because the table
// carries no Row-Level Security — see internal/controldb/rls.sql for why.
func New(tx db.Transactor) (*Recorder, error) {
	if tx == nil {
		return nil, errors.New("access: a transaction source is required")
	}
	return &Recorder{tx: tx, newID: id.NewV7}, nil
}

// RecordProviderAccess writes one row and reports whether it committed.
//
// Every failure is returned rather than logged and swallowed. `db.withProviderScope` stops the
// transaction when this returns an error, which is the behaviour the invariant requires: proceeding
// without evidence would make the access unattributable, and a recorder that reported success on a
// failed write would make the whole control decorative.
func (r *Recorder) RecordProviderAccess(ctx context.Context, provider db.ProviderAccess) error {
	// Checked here as well as in db.ProviderScope. This is the last point before the row is
	// written, and a zero identifier reaching the table would produce evidence naming nobody —
	// which reads as a record rather than as an absence, and is worse than no row at all.
	switch {
	case provider.Actor.IsNil():
		return errors.New("access: evidence requires an acting subject")
	case provider.Correlation.IsNil():
		return errors.New("access: evidence requires a correlation identifier")
	case provider.Reason == "":
		return db.ErrReasonRequired
	}

	accessID, err := r.newID()
	if err != nil {
		return fmt.Errorf("access: mint identifier: %w", err)
	}

	return r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if _, err := tx.Exec(ctx, insertStatement,
			accessID.String(), provider.Actor.String(), provider.Correlation.String(),
			provider.Reason); err != nil {
			return fmt.Errorf("access: insert evidence: %w", err)
		}
		return nil
	})
}
