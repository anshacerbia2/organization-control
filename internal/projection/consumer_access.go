package projection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// ConsumerAccess is a registered consumer acting on its own records, as organization_consumer_rt.
//
// The same operations as Registry.Get, Registry.RecordProgress, Publisher.Bootstrap and
// Publisher.Snapshot, with the same transaction bodies, on the consumer pool. The provider versions
// stay for a provider acting on a consumer's behalf with a reason. Two entry points rather than one
// that picks a pool, because the pool decides the database role, and tools/grantcheck derives each
// role's privileges from the wrapper a body is passed to. A body reachable from both is granted to
// both, which is correct; a pool chosen at run time would leave the derivation unable to say which.
//
// Which consumer a request may name is decided before this: the transport layer admits a consumer
// caller only for itself.
type ConsumerAccess struct {
	pool *db.ConsumerPool
	now  func() time.Time
}

// NewConsumerAccess constructs the consumer's own access.
func NewConsumerAccess(pool *db.ConsumerPool) (*ConsumerAccess, error) {
	if pool == nil {
		return nil, errors.New("projection: a consumer pool is required")
	}
	return &ConsumerAccess{pool: pool, now: time.Now}, nil
}

// Get reads the consumer's own registration. See Registry.Get.
func (a *ConsumerAccess) Get(ctx context.Context, consumerID string) (Consumer, error) {
	var consumer Consumer
	if err := db.WithConsumerScope(ctx, a.pool, "read own projection consumer "+consumerID,
		func(ctx context.Context, tx db.Tx) error {
			return load(ctx, tx, consumerID, &consumer)
		}); err != nil {
		return Consumer{}, err
	}
	return consumer, nil
}

// RecordProgress accepts the consumer's report of its own position. See Registry.RecordProgress.
func (a *ConsumerAccess) RecordProgress(ctx context.Context, report Progress) (Consumer, error) {
	if strings.TrimSpace(report.ConsumerID) == "" {
		return Consumer{}, fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	}

	var consumer Consumer
	at := a.now().UTC()
	if err := db.WithConsumerScope(ctx, a.pool, "record own projection progress for "+report.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			return recordProgressIn(ctx, tx, report, at, &consumer)
		}); err != nil {
		return Consumer{}, err
	}

	mark, reported := report.AppliedMark, at
	consumer.LastReportedMark, consumer.LastReportedAt = &mark, &reported
	return consumer, nil
}

// Bootstrap records the mark the consumer bootstrapped from. See Publisher.Bootstrap.
func (a *ConsumerAccess) Bootstrap(ctx context.Context, consumerID string, mark int64) (Consumer, error) {
	if mark < 0 {
		return Consumer{}, fmt.Errorf("%w: a snapshot mark cannot be negative", ErrInvalid)
	}

	var consumer Consumer
	if err := db.WithConsumerScope(ctx, a.pool, "record own projection bootstrap for "+consumerID,
		func(ctx context.Context, tx db.Tx) error {
			return bootstrapIn(ctx, tx, consumerID, mark, &consumer)
		}); err != nil {
		return Consumer{}, err
	}

	recorded := mark
	consumer.SnapshotMark = &recorded
	return consumer, nil
}

// Snapshot produces one page for the consumer. See Publisher.Snapshot.
func (a *ConsumerAccess) Snapshot(ctx context.Context, req SnapshotRequest) (Page, error) {
	size, page, err := startPage(req, a.now())
	if err != nil {
		return Page{}, err
	}
	if err := db.WithConsumerSnapshot(ctx, a.pool, "own projection snapshot for "+req.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			return snapshotIn(ctx, tx, req, size, &page)
		}); err != nil {
		return Page{}, err
	}
	return endPage(page, size), nil
}
