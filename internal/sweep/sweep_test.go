package sweep

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anshacerbia2/organization-control/internal/telemetry"
)

// batches returns a Batch that answers each call with the next count, or err once they run out.
func batches(counts []int64, err error, calls *int) Batch {
	return func(_ context.Context, size int) (int64, error) {
		if size != BatchSize {
			return 0, errors.New("unexpected batch size")
		}
		*calls++
		if *calls > len(counts) {
			return 0, err
		}
		return counts[*calls-1], nil
	}
}

type harness struct {
	runner *Runner
	reader *sdkmetric.ManualReader
	logs   *bytes.Buffer
	clock  *time.Time
}

func newHarness(t *testing.T, jobs ...Job) harness {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	logs := &bytes.Buffer{}
	runner, err := New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), "organization-control", "SAD-004",
		slog.New(slog.NewJSONHandler(logs, nil)), jobs...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return clock }
	return harness{runner: runner, reader: reader, logs: logs, clock: &clock}
}

func (h harness) advance(d time.Duration) {
	*h.clock = h.clock.Add(d)
}

// collect returns every data point by Prometheus series name, as float64.
func (h harness) collect(t *testing.T) map[string][]point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	names := map[string]string{}
	for _, i := range telemetry.Instruments {
		names[i.Name] = i.PrometheusName()
	}
	out := map[string][]point{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			name := names[m.Name]
			if name == "" {
				t.Fatalf("%s is exported and is not a telemetry.Instrument, so no rule test holds it", m.Name)
			}
			switch data := m.Data.(type) {
			case metricdata.Gauge[float64]:
				for _, p := range data.DataPoints {
					out[name] = append(out[name], point{p.Attributes, p.Value})
				}
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					out[name] = append(out[name], point{p.Attributes, float64(p.Value)})
				}
			}
		}
	}
	return out
}

type point struct {
	attrs attribute.Set
	value float64
}

func find(t *testing.T, points []point, want map[string]string) (float64, bool) {
	t.Helper()
	for _, p := range points {
		if !p.attrs.HasValue("deployable") || !p.attrs.HasValue("system") {
			t.Fatalf("a point carries no deployable or system: %v", p.attrs)
		}
		match := true
		for k, v := range want {
			if got, ok := p.attrs.Value(attribute.Key(k)); !ok || got.AsString() != v {
				match = false
			}
		}
		if match {
			return p.value, true
		}
	}
	return 0, false
}

func mustFind(t *testing.T, points []point, want map[string]string) float64 {
	t.Helper()
	v, ok := find(t, points, want)
	if !ok {
		t.Fatalf("no point with %v in %v", want, points)
	}
	return v
}

// A run takes batches until one is short, and records the success, the rows and the instant.
func TestARunTakesBatchesUntilOneIsShort(t *testing.T) {
	calls := 0
	h := newHarness(t, Job{Name: ProvisioningUnresolved, Interval: 15 * time.Minute,
		Batch: batches([]int64{BatchSize, BatchSize, 7}, nil, &calls)})
	h.advance(time.Minute)

	result, err := h.runner.Run(context.Background(), ProvisioningUnresolved)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 3 || result.Batches != 3 || result.Affected != 2*BatchSize+7 || result.Outcome != Success || result.Bounded {
		t.Fatalf("result = %+v after %d calls, want three batches, %d rows, success", result, calls, 2*BatchSize+7)
	}

	got := h.collect(t)
	sweep := map[string]string{"sweep": ProvisioningUnresolved}
	if v := mustFind(t, got[telemetry.SweepLastSuccess.PrometheusName()], sweep); v != float64(h.clock.Unix()) {
		t.Errorf("last success = %v, want the run's instant %d", v, h.clock.Unix())
	}
	if v := mustFind(t, got[telemetry.SweepLastRun.PrometheusName()], sweep); v != float64(h.clock.Unix()) {
		t.Errorf("last run = %v, want %d", v, h.clock.Unix())
	}
	if v := mustFind(t, got[telemetry.SweepInterval.PrometheusName()], sweep); v != 900 {
		t.Errorf("interval = %v, want 900", v)
	}
	if v := mustFind(t, got[telemetry.SweepAffected.PrometheusName()], sweep); v != 2*BatchSize+7 {
		t.Errorf("affected = %v, want %d", v, 2*BatchSize+7)
	}
	if v := mustFind(t, got[telemetry.SweepRuns.PrometheusName()],
		map[string]string{"sweep": ProvisioningUnresolved, "outcome": "success"}); v != 1 {
		t.Errorf("successful runs = %v, want 1", v)
	}
}

// A run that keeps finding full batches stops at the bound and says so; the next run continues.
func TestARunStopsAtTheBatchBound(t *testing.T) {
	calls := 0
	full := make([]int64, MaxBatches+10)
	for i := range full {
		full[i] = BatchSize
	}
	h := newHarness(t, Job{Name: InvitationExpiry, Interval: time.Hour, Batch: batches(full, nil, &calls)})

	result, err := h.runner.Run(context.Background(), InvitationExpiry)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != MaxBatches || !result.Bounded || result.Outcome != Success {
		t.Fatalf("result = %+v after %d calls, want %d calls, bounded, success", result, calls, MaxBatches)
	}
	if !strings.Contains(h.logs.String(), "batch bound") {
		t.Errorf("a bounded run logged nothing about its bound:\n%s", h.logs.String())
	}
}

// A failed run leaves the last success where it was, counts a failure, and is logged with the
// sweep's name, which is what the alert and the runbook read.
func TestAFailedRunKeepsTheLastSuccess(t *testing.T) {
	calls := 0
	failing := false
	batch := func(context.Context, int) (int64, error) {
		calls++
		if failing {
			return 0, errors.New("the database refused")
		}
		return 0, nil
	}
	h := newHarness(t, Job{Name: ProvisioningUnresolved, Interval: 15 * time.Minute, Batch: batch})

	if _, err := h.runner.Run(context.Background(), ProvisioningUnresolved); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	succeeded := h.clock.Unix()

	failing = true
	h.advance(15 * time.Minute)
	result, err := h.runner.Run(context.Background(), ProvisioningUnresolved)
	if err == nil || result.Outcome != Failure {
		t.Fatalf("a failing batch returned %+v, %v; want a failure", result, err)
	}

	got := h.collect(t)
	sweep := map[string]string{"sweep": ProvisioningUnresolved}
	if v := mustFind(t, got[telemetry.SweepLastSuccess.PrometheusName()], sweep); v != float64(succeeded) {
		t.Errorf("last success = %v after a failure, want the earlier success %d", v, succeeded)
	}
	if v := mustFind(t, got[telemetry.SweepLastRun.PrometheusName()], sweep); v != float64(h.clock.Unix()) {
		t.Errorf("last run = %v, want the failed run's instant %d", v, h.clock.Unix())
	}
	if v := mustFind(t, got[telemetry.SweepRuns.PrometheusName()],
		map[string]string{"sweep": ProvisioningUnresolved, "outcome": "failure"}); v != 1 {
		t.Errorf("failed runs = %v, want 1", v)
	}
	if !strings.Contains(h.logs.String(), `"sweep":"provisioning_unresolved"`) ||
		!strings.Contains(h.logs.String(), "the database refused") {
		t.Errorf("the failure was not logged with the sweep and the cause:\n%s", h.logs.String())
	}
}

// Before any run the last success is the start, so a sweep that never succeeds reaches the alert in
// two intervals like one that stopped; and there is no last-run series to read as a run in 1970.
func TestBeforeAnyRunTheLastSuccessIsTheStart(t *testing.T) {
	calls := 0
	h := newHarness(t, Job{Name: InvitationExpiry, Interval: time.Hour, Batch: batches(nil, nil, &calls)})

	got := h.collect(t)
	v := mustFind(t, got[telemetry.SweepLastSuccess.PrometheusName()], map[string]string{"sweep": InvitationExpiry})
	if v <= 0 {
		t.Errorf("last success before any run = %v, want the construction instant", v)
	}
	if _, ok := find(t, got[telemetry.SweepLastRun.PrometheusName()], map[string]string{"sweep": InvitationExpiry}); ok {
		t.Error("a last-run series was exported before any run")
	}
}

// A run is bounded by its interval, so a hung statement ends as a failure rather than a run that
// never finishes and never fails.
func TestARunIsBoundedByItsInterval(t *testing.T) {
	h := newHarness(t, Job{Name: InvitationExpiry, Interval: time.Hour,
		Batch: func(ctx context.Context, _ int) (int64, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > time.Hour {
				return 0, errors.New("no deadline within the interval")
			}
			return 0, nil
		}})
	if _, err := h.runner.Run(context.Background(), InvitationExpiry); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestNewRefusesAnIncompleteJob(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	ok := func(context.Context, int) (int64, error) { return 0, nil }
	for name, jobs := range map[string][]Job{
		"none":        nil,
		"no name":     {{Interval: time.Minute, Batch: ok}},
		"no interval": {{Name: "x", Batch: ok}},
		"no batch":    {{Name: "x", Interval: time.Minute}},
		"duplicate":   {{Name: "x", Interval: time.Minute, Batch: ok}, {Name: "x", Interval: time.Minute, Batch: ok}},
	} {
		if _, err := New(nil, "d", "s", logger, jobs...); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if _, err := New(nil, "d", "s", nil, Job{Name: "x", Interval: time.Minute, Batch: ok}); err == nil {
		t.Error("New accepted a nil logger")
	}
	runner, err := New(nil, "d", "s", logger, Job{Name: "x", Interval: time.Minute, Batch: ok})
	if err != nil {
		t.Fatalf("New with no meter provider: %v", err)
	}
	if _, err := runner.Run(context.Background(), "y"); err == nil {
		t.Error("Run accepted a job that does not exist")
	}
}
