// Package telemetry turns the enforcement signals into OpenTelemetry metrics.
//
// ROADMAP item 14: there was no metric for dispatcher lag, projection age or security-debt depth,
// and an operator learned that a dead letter was refusing every projection-backed check by reading
// refusal reasons. These gauges are read from the database on each collection, and the alert rules
// in observability/alerts evaluate them against SAD-004 §9.3.2's thresholds.
package telemetry

import (
	"context"
	"errors"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/anshacerbia2/organization-control/internal/projection"
)

// Instrument is one metric: its OpenTelemetry name and unit.
type Instrument struct {
	Name string
	Unit string
}

// PrometheusName is the series name a Collector's Prometheus exporter produces for it: dots become
// underscores and a seconds unit becomes the _seconds suffix, a ratio the _ratio suffix. The alert
// rules reference these names, and a test holds the two together.
func (i Instrument) PrometheusName() string {
	name := strings.ReplaceAll(i.Name, ".", "_")
	switch i.Unit {
	case "s":
		return name + "_seconds"
	case "1":
		return name + "_ratio"
	}
	return name
}

// The instruments. Unit "{...}" is an annotation, which the Prometheus conversion drops.
var (
	OutboxUnpublished        = Instrument{"organization.outbox.unpublished", "{event}"}
	OutboxOldestUnpublished  = Instrument{"organization.outbox.oldest_unpublished_age", "s"}
	SecurityDebt             = Instrument{"organization.security_debt.dead_letters", "{event}"}
	SecurityDebtOldest       = Instrument{"organization.security_debt.oldest_age", "s"}
	StaleDeadLetters         = Instrument{"organization.dead_letters.unresolved_unwaived", "{event}"}
	StaleDeadLettersOldest   = Instrument{"organization.dead_letters.oldest_unresolved_unwaived_age", "s"}
	ConsumerReportAge        = Instrument{"organization.projection.consumer_report_age", "s"}
	ConsumerMaxAcceptedAge   = Instrument{"organization.projection.consumer_max_accepted_age", "s"}
	ConsumerVerifyRatio      = Instrument{"organization.projection.consumer_verify", "1"}
	Instruments              = []Instrument{OutboxUnpublished, OutboxOldestUnpublished, SecurityDebt, SecurityDebtOldest, StaleDeadLetters, StaleDeadLettersOldest, ConsumerReportAge, ConsumerMaxAcceptedAge, ConsumerVerifyRatio}
	instrumentationName      = "github.com/anshacerbia2/organization-control/internal/telemetry"
	errReaderRequired        = errors.New("telemetry: a signals reader is required")
	errMeterProviderRequired = errors.New("telemetry: a meter provider is required")
)

// reader is what the gauges read. *projection.SignalsReader satisfies it.
type reader interface {
	Read(ctx context.Context) (projection.Signals, error)
}

// Register creates the gauges on provider and one callback that reads the signals once per
// collection and observes every gauge from that one reading, so the values describe one instant.
// A failed read observes nothing: an absent series is what the absent-telemetry alert catches,
// where a zero would read as healthy.
func Register(provider metric.MeterProvider, signals reader, deployable, system string) error {
	if provider == nil {
		return errMeterProviderRequired
	}
	if signals == nil {
		return errReaderRequired
	}
	meter := provider.Meter(instrumentationName)
	gauge := func(i Instrument) (metric.Float64ObservableGauge, error) {
		return meter.Float64ObservableGauge(i.Name, metric.WithUnit(i.Unit))
	}

	unpublished, err := gauge(OutboxUnpublished)
	if err != nil {
		return err
	}
	unpublishedAge, err := gauge(OutboxOldestUnpublished)
	if err != nil {
		return err
	}
	debt, err := gauge(SecurityDebt)
	if err != nil {
		return err
	}
	debtAge, err := gauge(SecurityDebtOldest)
	if err != nil {
		return err
	}
	stale, err := gauge(StaleDeadLetters)
	if err != nil {
		return err
	}
	staleAge, err := gauge(StaleDeadLettersOldest)
	if err != nil {
		return err
	}
	reportAge, err := gauge(ConsumerReportAge)
	if err != nil {
		return err
	}
	budget, err := gauge(ConsumerMaxAcceptedAge)
	if err != nil {
		return err
	}
	verify, err := gauge(ConsumerVerifyRatio)
	if err != nil {
		return err
	}

	// Every series carries deployable and system, as TDD-foundation-platform-002 requires.
	identity := []attribute.KeyValue{attribute.String("deployable", deployable), attribute.String("system", system)}
	with := func(extra ...attribute.KeyValue) metric.ObserveOption {
		return metric.WithAttributes(append(append([]attribute.KeyValue{}, identity...), extra...)...)
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s, err := signals.Read(ctx)
		if err != nil {
			return err
		}
		for _, lane := range s.Lanes {
			labels := with(attribute.String("consumer", lane.Consumer), attribute.String("lane", lane.Lane))
			o.ObserveFloat64(unpublished, float64(lane.Count), labels)
			o.ObserveFloat64(unpublishedAge, lane.OldestAge, labels)
		}
		for _, d := range s.Debt {
			consumer := attribute.String("consumer", d.Consumer)
			o.ObserveFloat64(debt, float64(d.Count), with(consumer))
			o.ObserveFloat64(debtAge, d.OldestAge, with(consumer))
		}
		o.ObserveFloat64(stale, float64(s.Stale), with())
		o.ObserveFloat64(staleAge, s.StaleOldestAge, with())
		for _, c := range s.Consumers {
			consumer := attribute.String("consumer", c.Consumer)
			o.ObserveFloat64(reportAge, c.ReportAge, with(consumer))
			o.ObserveFloat64(budget, c.MaxAcceptedAge, with(consumer))
			if c.VerifyRatio != nil {
				o.ObserveFloat64(verify, *c.VerifyRatio, with(consumer))
			}
		}
		return nil
	}, unpublished, unpublishedAge, debt, debtAge, stale, staleAge, reportAge, budget, verify)
	return err
}
