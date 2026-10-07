package telemetry

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anshacerbia2/organization-control/internal/projection"
)

type fixedReader struct {
	signals projection.Signals
	err     error
}

func (f fixedReader) Read(context.Context) (projection.Signals, error) { return f.signals, f.err }

func collect(t *testing.T, r reader) map[string][]metricdata.DataPoint[float64] {
	t.Helper()
	got, err := collectAllowingErrors(t, r)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return got
}

// collectAllowingErrors returns the callback's error beside whatever was observed, which the SDK
// reports rather than swallows.
func collectAllowingErrors(t *testing.T, r reader) (map[string][]metricdata.DataPoint[float64], error) {
	t.Helper()
	manual := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(manual))
	if err := Register(provider, r, "organization-control", "SAD-004"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var rm metricdata.ResourceMetrics
	collectErr := manual.Collect(context.Background(), &rm)
	out := map[string][]metricdata.DataPoint[float64]{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if gauge, ok := m.Data.(metricdata.Gauge[float64]); ok {
				out[m.Name] = gauge.DataPoints
			}
		}
	}
	return out, collectErr
}

func value(t *testing.T, points []metricdata.DataPoint[float64], key, want string) float64 {
	t.Helper()
	for _, p := range points {
		if v, ok := p.Attributes.Value(attribute.Key(key)); (key == "" || ok && v.AsString() == want) &&
			p.Attributes.HasValue("deployable") && p.Attributes.HasValue("system") {
			return p.Value
		}
	}
	t.Fatalf("no point with %s=%q carrying deployable and system in %v", key, want, points)
	return 0
}

// One reading, observed on every gauge, with the attributes the alert rules select on.
func TestTheGaugesReportTheSignals(t *testing.T) {
	ratio := 0.2
	got := collect(t, fixedReader{signals: projection.Signals{
		Lanes: []projection.LaneSignal{
			{Consumer: "foundation-reference", Lane: "priority", Count: 2, OldestAge: 45},
			{Consumer: "foundation-reference", Lane: "standard", Count: 7, OldestAge: 400},
		},
		Debt:  []projection.DebtSignal{{Consumer: "foundation-reference", Count: 1, OldestAge: 90}},
		Stale: 3, StaleOldestAge: 100000,
		Consumers: []projection.ConsumerSignal{
			{Consumer: "foundation-reference", ReportAge: 120, MaxAcceptedAge: 60, VerifyRatio: &ratio},
		},
	}})

	for _, c := range []struct {
		instrument Instrument
		key, value string
		want       float64
	}{
		{OutboxUnpublished, "lane", "priority", 2},
		{OutboxOldestUnpublished, "lane", "priority", 45},
		{OutboxOldestUnpublished, "lane", "standard", 400},
		{SecurityDebt, "consumer", "foundation-reference", 1},
		{SecurityDebtOldest, "consumer", "foundation-reference", 90},
		{StaleDeadLetters, "", "", 3},
		{StaleDeadLettersOldest, "", "", 100000},
		{ConsumerReportAge, "consumer", "foundation-reference", 120},
		{ConsumerMaxAcceptedAge, "consumer", "foundation-reference", 60},
		{ConsumerVerifyRatio, "consumer", "foundation-reference", 0.2},
	} {
		if v := value(t, got[c.instrument.Name], c.key, c.value); v != c.want {
			t.Errorf("%s{%s=%q} = %v, want %v", c.instrument.Name, c.key, c.value, v, c.want)
		}
	}
}

// A failed read observes nothing rather than zeros: zero debt reads as healthy, and an absent series
// is what EnforcementTelemetryAbsent is for.
func TestAFailedReadObservesNothing(t *testing.T) {
	got, err := collectAllowingErrors(t, fixedReader{err: errors.New("database unreachable")})
	if err == nil {
		t.Error("a failed read was not reported to the SDK")
	}
	for name, points := range got {
		if len(points) > 0 {
			t.Errorf("%s observed %v after a failed read", name, points)
		}
	}
}

var ruleSeries = regexp.MustCompile(`\borganization_[a-z_]+\b`)

// The rules name Prometheus series, and the code names OpenTelemetry instruments. A rename on
// either side would leave an alert that can never fire, so every series a rule reads must be one
// this package exports.
func TestEveryRuleReadsASeriesThisPackageExports(t *testing.T) {
	rules, err := os.ReadFile("../../observability/alerts/organization-control.rules.yml")
	if err != nil {
		t.Fatalf("reading the rules: %v", err)
	}
	exported := map[string]bool{}
	for _, i := range Instruments {
		exported[i.PrometheusName()] = true
	}
	seen := 0
	for _, name := range ruleSeries.FindAllString(string(rules), -1) {
		seen++
		if !exported[name] {
			t.Errorf("the rules read %s, which no instrument exports", name)
		}
	}
	if seen == 0 {
		t.Fatal("found no series in the rules; the pattern no longer matches them")
	}
}

func TestRegisterRefusesAnIncompleteConstruction(t *testing.T) {
	if err := Register(nil, fixedReader{}, "d", "s"); err == nil {
		t.Error("Register accepted no meter provider")
	}
	if err := Register(sdkmetric.NewMeterProvider(), nil, "d", "s"); err == nil {
		t.Error("Register accepted no signals reader")
	}
}
