// Package sweep runs the periodic sweeps of TDD-organization-control-003 §Scheduled Sweeps and
// records what each run did.
//
// It starts no goroutine: arch.json leaves that to the composition root, which calls Run once per
// interval. What lives here is one run -- the batches, their bound, the outcome -- and the telemetry
// the ScheduledSweepStopped alert reads, so the run and its evidence cannot drift apart.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/anshacerbia2/organization-control/internal/telemetry"
)

const (
	// BatchSize is how many rows one batch takes, the size the runbooks give the routes.
	BatchSize = 100

	// MaxBatches bounds one run. A batch that keeps coming back full is either a backlog, which the
	// next run continues because the batch predicate is the resume point, or a defect that would
	// otherwise hold the run in a loop.
	MaxBatches = 50
)

// Batch takes up to size rows and reports how many it changed.
type Batch func(ctx context.Context, size int) (int64, error)

// Job is one scheduled sweep.
type Job struct {
	// Name is the sweep label: provisioning_unresolved or invitation_expiry.
	Name     string
	Interval time.Duration
	Batch    Batch
}

// The two sweeps' names, which the alert rules and the runbooks use.
const (
	ProvisioningUnresolved = "provisioning_unresolved"
	InvitationExpiry       = "invitation_expiry"
)

// Outcome is how a run ended.
type Outcome string

const (
	Success Outcome = "success"
	Failure Outcome = "failure"
)

// Result is one run.
type Result struct {
	Outcome  Outcome
	Affected int64
	Batches  int

	// Bounded is true when the run stopped at MaxBatches with the last batch full.
	Bounded bool
}

type state struct {
	Job
	mu          sync.Mutex
	lastSuccess time.Time
	lastRun     time.Time
}

// Runner runs the jobs and holds what their last runs did.
type Runner struct {
	jobs     map[string]*state
	order    []string
	runs     metric.Int64Counter
	affected metric.Int64Counter
	identity []attribute.KeyValue
	logger   *slog.Logger
	now      func() time.Time
}

// New builds a Runner over jobs and registers its telemetry on provider, or on nothing when
// provider is nil. Every series carries deployable and system, as TDD-foundation-platform-002
// requires.
//
// The last success starts at construction, which is the process's start: a sweep that never
// succeeds then reaches the alert in the same two intervals as one that stopped, where a zero would
// fire at once and an absent series would never fire.
func New(provider metric.MeterProvider, deployable, system string, logger *slog.Logger, jobs ...Job) (*Runner, error) {
	if logger == nil {
		return nil, errors.New("sweep: a logger is required")
	}
	if len(jobs) == 0 {
		return nil, errors.New("sweep: at least one job is required")
	}
	if provider == nil {
		provider = noop.NewMeterProvider()
	}
	r := &Runner{
		jobs:     map[string]*state{},
		identity: []attribute.KeyValue{attribute.String("deployable", deployable), attribute.String("system", system)},
		logger:   logger,
		now:      time.Now,
	}
	started := r.now()
	for _, job := range jobs {
		switch {
		case job.Name == "":
			return nil, errors.New("sweep: a job requires a name")
		case job.Interval <= 0:
			return nil, fmt.Errorf("sweep: %s requires a positive interval", job.Name)
		case job.Batch == nil:
			return nil, fmt.Errorf("sweep: %s requires a batch", job.Name)
		}
		if _, dup := r.jobs[job.Name]; dup {
			return nil, fmt.Errorf("sweep: %s is named twice", job.Name)
		}
		r.jobs[job.Name] = &state{Job: job, lastSuccess: started}
		r.order = append(r.order, job.Name)
	}

	meter := provider.Meter("github.com/anshacerbia2/organization-control/internal/sweep")
	var err error
	if r.runs, err = meter.Int64Counter(telemetry.SweepRuns.Name, metric.WithUnit(telemetry.SweepRuns.Unit)); err != nil {
		return nil, err
	}
	if r.affected, err = meter.Int64Counter(telemetry.SweepAffected.Name,
		metric.WithUnit(telemetry.SweepAffected.Unit)); err != nil {
		return nil, err
	}
	gauge := func(i telemetry.Instrument) (metric.Float64ObservableGauge, error) {
		return meter.Float64ObservableGauge(i.Name, metric.WithUnit(i.Unit))
	}
	lastSuccess, err := gauge(telemetry.SweepLastSuccess)
	if err != nil {
		return nil, err
	}
	lastRun, err := gauge(telemetry.SweepLastRun)
	if err != nil {
		return nil, err
	}
	interval, err := gauge(telemetry.SweepInterval)
	if err != nil {
		return nil, err
	}
	if _, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, name := range r.order {
			s := r.jobs[name]
			s.mu.Lock()
			success, run := s.lastSuccess, s.lastRun
			s.mu.Unlock()
			labels := metric.WithAttributes(r.attributes(name)...)
			o.ObserveFloat64(lastSuccess, seconds(success), labels)
			o.ObserveFloat64(interval, s.Interval.Seconds(), labels)
			// No run yet is no series rather than a zero, which would read as a run in 1970.
			if !run.IsZero() {
				o.ObserveFloat64(lastRun, seconds(run), labels)
			}
		}
		return nil
	}, lastSuccess, lastRun, interval); err != nil {
		return nil, err
	}
	return r, nil
}

// Jobs returns the jobs in the order they were given.
func (r *Runner) Jobs() []Job {
	jobs := make([]Job, 0, len(r.order))
	for _, name := range r.order {
		jobs = append(jobs, r.jobs[name].Job)
	}
	return jobs
}

// Run runs the named job once: batches of BatchSize until one comes back short, at most MaxBatches,
// within one interval. It records the outcome and logs a failure, and returns the result for a
// caller that wants it.
func (r *Runner) Run(ctx context.Context, name string) (Result, error) {
	s, ok := r.jobs[name]
	if !ok {
		return Result{}, fmt.Errorf("sweep: no job named %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Interval)
	defer cancel()

	var result Result
	var runErr error
	for result.Batches < MaxBatches {
		n, err := s.Batch(ctx, BatchSize)
		if err != nil {
			runErr = err
			break
		}
		result.Batches++
		result.Affected += n
		if n < BatchSize {
			break
		}
		if result.Batches == MaxBatches {
			result.Bounded = true
		}
	}

	at := r.now()
	if result.Affected > 0 {
		r.affected.Add(context.WithoutCancel(ctx), result.Affected, metric.WithAttributes(r.attributes(name)...))
	}
	s.mu.Lock()
	s.lastRun = at
	if runErr == nil {
		s.lastSuccess = at
	}
	s.mu.Unlock()

	result.Outcome = Success
	if runErr != nil {
		result.Outcome = Failure
	}
	r.runs.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(
		append(r.attributes(name), attribute.String("outcome", string(result.Outcome)))...))

	if runErr != nil {
		r.logger.Error("scheduled sweep failed", slog.String("sweep", name),
			slog.Int64("affected", result.Affected), slog.Int("batches", result.Batches),
			slog.String("error", runErr.Error()))
		return result, runErr
	}
	if result.Bounded {
		r.logger.Warn("scheduled sweep stopped at its batch bound; the next run continues",
			slog.String("sweep", name), slog.Int64("affected", result.Affected), slog.Int("batches", result.Batches))
	} else if result.Affected > 0 {
		r.logger.Info("scheduled sweep", slog.String("sweep", name), slog.Int64("affected", result.Affected))
	}
	return result, nil
}

// attributes are one job's: deployable, system and sweep.
func (r *Runner) attributes(name string) []attribute.KeyValue {
	return append(append([]attribute.KeyValue{}, r.identity...), attribute.String("sweep", name))
}

func seconds(t time.Time) float64 {
	return float64(t.UnixNano()) / float64(time.Second)
}
