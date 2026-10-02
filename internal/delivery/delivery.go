// Package delivery runs this service's consumers' dispatchers in its own process (ADR-GLB-018 §5.4,
// TDD-organization-control-005 §Technical Context).
//
// One dispatcher per configured consumer. Each runs on the dispatch role's own pool, posts to that
// consumer's acceptance API with foundation-platform's outbox/httpdelivery, and authenticates as this
// service's workload with a clientauth access token. No consumer holds a credential to this
// database, and no shared secret authenticates a delivery (STD-IAM-001 §3).
//
// The outbound clients are foundation-platform's. This package imports no net/http of its own, so
// the denial in arch.json still holds for every package of this repository.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/observability"
	"github.com/anshacerbia2/foundation-platform/outbox"
	"github.com/anshacerbia2/foundation-platform/outbox/httpdelivery"
)

// Target is one consumer's acceptance API.
type Target struct {
	Consumer string
	Endpoint string
}

// Config is what the dispatchers share.
type Config struct {
	Targets []Target

	// Pool connects as a login role inheriting organization_dispatch_rt.
	Pool *fdb.Pool

	// Tokens is this service's workload credential, a clientauth.Tokens in production.
	Tokens httpdelivery.TokenSource

	Timeout       time.Duration
	RetryInterval time.Duration
	Telemetry     *observability.Telemetry
	Logger        *slog.Logger

	// registered and run are seams for the tests; nil uses the real ones.
	registered func(ctx context.Context, consumer string) (bool, error)
	run        func(ctx context.Context, target Target) error
}

// registeredStatement is the startup check foundation-reference's dispatcher makes on its own name,
// on the two columns the dispatch role may read. One row always.
const registeredStatement = `SELECT EXISTS (
    SELECT 1 FROM projection.consumer WHERE consumer_id = $1 AND retired_at IS NULL)`

// Run runs every target's dispatcher until ctx is done, and returns once all of them have stopped.
//
// A target whose consumer is not yet registered waits for it, checking again every RetryInterval:
// a consumer is often registered after this service is deployed, and a misconfigured target must
// not stop the control plane from serving. A dispatcher that stops with an error -- its database
// contract unmet, its pool lost -- is logged and started again after the same interval.
func Run(ctx context.Context, cfg Config) error {
	if err := validate(cfg); err != nil {
		return err
	}
	if cfg.registered == nil {
		pool := cfg.Pool
		cfg.registered = func(ctx context.Context, consumer string) (bool, error) {
			return checkRegistered(ctx, pool, consumer)
		}
	}
	if cfg.run == nil {
		cfg.run = func(ctx context.Context, target Target) error {
			publisher, err := httpdelivery.NewPublisher(httpdelivery.Config{
				Endpoint: target.Endpoint, Tokens: cfg.Tokens, Timeout: cfg.Timeout, Telemetry: cfg.Telemetry,
			})
			if err != nil {
				return err
			}
			dispatcher, err := outbox.NewDispatcher(cfg.Pool, publisher, outbox.Config{Consumer: target.Consumer})
			if err != nil {
				return err
			}
			return dispatcher.Run(ctx)
		}
	}

	var wg sync.WaitGroup
	for _, target := range cfg.Targets {
		wg.Add(1)
		go func(target Target) {
			defer wg.Done()
			serve(ctx, cfg, target)
		}(target)
	}
	wg.Wait()
	return ctx.Err()
}

// checkRegistered reads whether the consumer is an active registered consumer, on the dispatch
// pool. tools/grantcheck lists it out of scope: it runs as organization_dispatch_rt, whose
// privileges dispatch_role_integration_test.go measures.
func checkRegistered(ctx context.Context, pool *fdb.Pool, consumer string) (bool, error) {
	var registered bool
	err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, registeredStatement, consumer).Scan(&registered)
	})
	return registered, err
}

func validate(cfg Config) error {
	switch {
	case cfg.Pool == nil && (cfg.registered == nil || cfg.run == nil):
		return errors.New("delivery: the dispatch pool is required")
	case cfg.Tokens == nil && cfg.run == nil:
		return errors.New("delivery: the workload token source is required")
	case cfg.Timeout <= 0:
		return errors.New("delivery: the publication timeout must be positive")
	case cfg.RetryInterval <= 0:
		return errors.New("delivery: the retry interval must be positive")
	case cfg.Telemetry == nil && cfg.run == nil:
		return errors.New("delivery: telemetry is required")
	case cfg.Logger == nil:
		return errors.New("delivery: a logger is required")
	}
	seen := map[string]bool{}
	for _, target := range cfg.Targets {
		name := strings.TrimSpace(target.Consumer)
		if name == "" || strings.TrimSpace(target.Endpoint) == "" {
			return errors.New("delivery: a target names a consumer and an endpoint")
		}
		if seen[name] {
			return fmt.Errorf("delivery: %s is named twice", name)
		}
		seen[name] = true
	}
	return nil
}

// serve keeps one target's dispatcher running until ctx is done.
func serve(ctx context.Context, cfg Config, target Target) {
	logger := cfg.Logger.With(slog.String("consumer", target.Consumer))
	for ctx.Err() == nil {
		registered, err := cfg.registered(ctx, target.Consumer)
		switch {
		case err != nil:
			logger.Error("the consumer's registration could not be checked; the dispatcher waits",
				slog.String("error", err.Error()))
		case !registered:
			// A delivery under a name no registration holds produces receipts no resolution
			// reads (TDD-organization-control-005 §Configuration), so the dispatcher does not start.
			logger.Warn("the consumer is not an active registered consumer; its dispatcher waits for the registration")
		default:
			logger.Info("delivering to the consumer", slog.String("endpoint", target.Endpoint))
			err := cfg.run(ctx, target)
			if ctx.Err() != nil {
				return
			}
			logger.Error("the consumer's dispatcher stopped; it starts again after the retry interval",
				slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.RetryInterval):
		}
	}
}
