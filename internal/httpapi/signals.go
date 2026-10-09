package httpapi

// What the HTTP surface reports besides its responses: the isolation controls' refusals
// (TDD-organization-control-001 §Operational Notes), the anonymous invitation lookups
// (TDD-organization-control-004 §Operational Notes), and the security findings of a reconciliation
// (TDD-organization-control-002 §Operational Notes). The gauges read from the database live in
// internal/telemetry; these are counted where they happen, because no table records them.

import (
	stdcontext "context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/anshacerbia2/foundation-platform/observability"

	"github.com/anshacerbia2/organization-control/internal/telemetry"
)

// The isolation controls a refused statement can name.
const (
	// controlWithCheck is a write the WITH CHECK half of a policy refused: a row for a Tenant the
	// transaction was not bound to. A defect or an attack, and a security finding at any count.
	controlWithCheck = "with_check"

	// controlUnsetBinding is a statement that reached a policy whose scope setting was never bound.
	// The policies read the setting with missing_ok false precisely so this raises instead of
	// returning no rows.
	controlUnsetBinding = "unset_binding"
)

// surfaceSignals carries the logger and the two counters the surface adds to.
type surfaceSignals struct {
	telemetry *observability.Telemetry
	refusals  metric.Int64Counter
	lookups   metric.Int64Counter
}

func newSurfaceSignals(provider metric.MeterProvider, t *observability.Telemetry) (*surfaceSignals, error) {
	if provider == nil {
		provider = noop.NewMeterProvider()
	}
	meter := provider.Meter("github.com/anshacerbia2/organization-control/internal/httpapi")
	refusals, err := meter.Int64Counter(telemetry.IsolationRefusals.Name,
		metric.WithUnit(telemetry.IsolationRefusals.Unit))
	if err != nil {
		return nil, err
	}
	lookups, err := meter.Int64Counter(telemetry.InvitationLookups.Name,
		metric.WithUnit(telemetry.InvitationLookups.Unit))
	if err != nil {
		return nil, err
	}
	return &surfaceSignals{telemetry: t, refusals: refusals, lookups: lookups}, nil
}

type signalsKey struct{}

func withSignals(ctx stdcontext.Context, s *surfaceSignals) stdcontext.Context {
	return stdcontext.WithValue(ctx, signalsKey{}, s)
}

func signalsFrom(ctx stdcontext.Context) *surfaceSignals {
	s, _ := ctx.Value(signalsKey{}).(*surfaceSignals)
	return s
}

// logger is the request's logger, or the default one outside a configured surface.
func (s *surfaceSignals) logger(ctx stdcontext.Context) *slog.Logger {
	if s == nil || s.telemetry == nil {
		return slog.Default()
	}
	return s.telemetry.Logger(ctx)
}

// sqlStater is the one method of the driver's error this package reads. arch.json denies it the
// driver itself, and the SQLSTATE is all the classification needs.
type sqlStater interface{ SQLState() string }

// isolationControl names the isolation control an error is a refusal by, or "".
//
// 42501 with "row-level security" in the message is PostgreSQL's "new row violates row-level security
// policy", which only a WITH CHECK expression raises. An unset binding raises 42704, "unrecognized
// configuration parameter", on a connection that never set the parameter, and 22P02, invalid input
// for the uuid or boolean cast, on one where an earlier SET LOCAL left it empty; both name the
// setting's cast or the app. parameter in the message.
func isolationControl(err error) string {
	var state sqlStater
	if !errors.As(err, &state) {
		return ""
	}
	message := err.Error()
	switch state.SQLState() {
	case "42501":
		if strings.Contains(message, "row-level security") {
			return controlWithCheck
		}
	case "42704":
		if strings.Contains(message, `"app.`) {
			return controlUnsetBinding
		}
	case "22P02":
		if strings.Contains(message, `type uuid: ""`) || strings.Contains(message, `type boolean: ""`) {
			return controlUnsetBinding
		}
	}
	return ""
}

// recordInternal reports an error the surface answers 500. Every one is logged with its cause, which
// the response withholds; an isolation refusal is counted by its control and logged as one, so the
// alert and the runbook (docs/runbooks/with-check-rejection.md, unset-binding.md) find it by name.
func recordInternal(r *http.Request, err error) {
	s := signalsFrom(r.Context())
	log := s.logger(r.Context())
	attrs := []any{slog.String("method", r.Method), slog.String("route", r.Pattern), slog.String("error", err.Error())}
	control := isolationControl(err)
	if control == "" {
		log.ErrorContext(r.Context(), "the request failed", attrs...)
		return
	}
	log.ErrorContext(r.Context(), "an isolation control refused a statement",
		append(attrs, slog.String("control", control))...)
	if s != nil {
		s.refusals.Add(r.Context(), 1, metric.WithAttributes(attribute.String("control", control)))
	}
}
