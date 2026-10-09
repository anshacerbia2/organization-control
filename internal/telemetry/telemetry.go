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

	// Counter is a monotonic sum, which the Prometheus conversion suffixes _total; every other
	// instrument here is a gauge.
	Counter bool
}

// PrometheusName is the series name a Collector's Prometheus exporter produces for it: dots become
// underscores and a seconds unit becomes the _seconds suffix, a ratio the _ratio suffix, and a
// counter ends in _total. The alert rules reference these names, and a test holds the two together.
func (i Instrument) PrometheusName() string {
	name := strings.ReplaceAll(i.Name, ".", "_")
	switch i.Unit {
	case "s":
		name += "_seconds"
	case "1":
		name += "_ratio"
	}
	if i.Counter {
		name += "_total"
	}
	return name
}

// The instruments. Unit "{...}" is an annotation, which the Prometheus conversion drops.
var (
	OutboxUnpublished       = Instrument{Name: "organization.outbox.unpublished", Unit: "{event}"}
	OutboxOldestUnpublished = Instrument{Name: "organization.outbox.oldest_unpublished_age", Unit: "s"}
	SecurityDebt            = Instrument{Name: "organization.security_debt.dead_letters", Unit: "{event}"}
	SecurityDebtOldest      = Instrument{Name: "organization.security_debt.oldest_age", Unit: "s"}
	StaleDeadLetters        = Instrument{Name: "organization.dead_letters.unresolved_unwaived", Unit: "{event}"}
	StaleDeadLettersOldest  = Instrument{Name: "organization.dead_letters.oldest_unresolved_unwaived_age", Unit: "s"}
	ConsumerReportAge       = Instrument{Name: "organization.projection.consumer_report_age", Unit: "s"}
	ConsumerMaxAcceptedAge  = Instrument{Name: "organization.projection.consumer_max_accepted_age", Unit: "s"}
	ConsumerVerifyRatio     = Instrument{Name: "organization.projection.consumer_verify", Unit: "1"}

	// ConsumerExtraFindings is the count of `extra` findings in a consumer's last reconciliation, and
	// OldestUnapplied the age of its oldest security event not yet applied: the two signals of
	// TDD-organization-control-002 §Operational Notes that had no metric (1.15.0).
	ConsumerExtraFindings = Instrument{Name: "organization.projection.consumer_extra_findings", Unit: "{finding}"}
	OldestUnapplied       = Instrument{Name: "organization.enforcement.oldest_unapplied_age", Unit: "s"}

	// The lifecycle gauges of TDD-organization-control-004 and -003 §Operational Notes.
	ObligationsOverdue      = Instrument{Name: "organization.offboarding.obligations_overdue", Unit: "{obligation}"}
	OldestOverdueObligation = Instrument{Name: "organization.offboarding.oldest_overdue_obligation_age", Unit: "s"}
	OffboardingsInProgress  = Instrument{Name: "organization.offboarding.in_progress", Unit: "{offboarding}"}
	OldestOffboarding       = Instrument{Name: "organization.offboarding.oldest_in_progress_age", Unit: "s"}
	ProvisioningRequests    = Instrument{Name: "organization.provisioning.requests", Unit: "{request}"}
	OldestProvisioning      = Instrument{Name: "organization.provisioning.oldest_request_age", Unit: "s"}

	// Counters the HTTP surface adds to: a statement an isolation control refused, by control
	// (TDD-organization-control-001 §Operational Notes), and an anonymous invitation lookup
	// (TDD-organization-control-004 §Operational Notes).
	IsolationRefusals = Instrument{Name: "organization.isolation.refusals", Unit: "{refusal}", Counter: true}
	InvitationLookups = Instrument{Name: "organization.invitation.lookups", Unit: "{lookup}", Counter: true}
	Instruments       = []Instrument{OutboxUnpublished, OutboxOldestUnpublished, SecurityDebt, SecurityDebtOldest,
		StaleDeadLetters, StaleDeadLettersOldest, ConsumerReportAge, ConsumerMaxAcceptedAge, ConsumerVerifyRatio,
		ConsumerExtraFindings, OldestUnapplied, ObligationsOverdue, OldestOverdueObligation,
		OffboardingsInProgress, OldestOffboarding, ProvisioningRequests, OldestProvisioning,
		IsolationRefusals, InvitationLookups}
	instrumentationName      = "github.com/anshacerbia2/organization-control/internal/telemetry"
	errReaderRequired        = errors.New("telemetry: a signals reader is required")
	errMeterProviderRequired = errors.New("telemetry: a meter provider is required")
)

// reader is what the gauges read. *projection.SignalsReader satisfies it.
type reader interface {
	Read(ctx context.Context) (projection.Signals, error)
	ReadLifecycle(ctx context.Context) (projection.LifecycleSignals, error)
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
	extra, err := gauge(ConsumerExtraFindings)
	if err != nil {
		return err
	}
	unapplied, err := gauge(OldestUnapplied)
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
			o.ObserveFloat64(extra, float64(c.ExtraFindings), with(consumer))
		}
		for _, u := range s.Unapplied {
			o.ObserveFloat64(unapplied, u.OldestAge, with(attribute.String("consumer", u.Consumer)))
		}
		return nil
	}, unpublished, unpublishedAge, debt, debtAge, stale, staleAge, reportAge, budget, verify, extra, unapplied)
	if err != nil {
		return err
	}
	return registerLifecycle(meter, signals, with)
}

// registerLifecycle observes the offboarding and provisioning gauges from a callback of their own,
// so a failure to read them leaves the enforcement gauges above reporting, and LifecycleTelemetryAbsent
// names which half is blind.
func registerLifecycle(meter metric.Meter, signals reader, with func(...attribute.KeyValue) metric.ObserveOption) error {
	gauges := map[Instrument]metric.Float64ObservableGauge{}
	var observables []metric.Observable
	for _, i := range []Instrument{ObligationsOverdue, OldestOverdueObligation, OffboardingsInProgress,
		OldestOffboarding, ProvisioningRequests, OldestProvisioning} {
		g, err := meter.Float64ObservableGauge(i.Name, metric.WithUnit(i.Unit))
		if err != nil {
			return err
		}
		gauges[i] = g
		observables = append(observables, g)
	}
	_, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s, err := signals.ReadLifecycle(ctx)
		if err != nil {
			return err
		}
		o.ObserveFloat64(gauges[ObligationsOverdue], float64(s.ObligationsOverdue), with())
		o.ObserveFloat64(gauges[OldestOverdueObligation], s.OldestOverdueObligation, with())
		o.ObserveFloat64(gauges[OffboardingsInProgress], float64(s.OffboardingsInProgress), with())
		o.ObserveFloat64(gauges[OldestOffboarding], s.OldestOffboarding, with())
		for _, r := range s.Requests {
			labels := with(attribute.String("operation", r.Operation), attribute.String("state", r.State))
			o.ObserveFloat64(gauges[ProvisioningRequests], float64(r.Count), labels)
			o.ObserveFloat64(gauges[OldestProvisioning], r.OldestAge, labels)
		}
		return nil
	}, observables...)
	return err
}
