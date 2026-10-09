package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anshacerbia2/organization-control/internal/telemetry"
)

// pgError carries a SQLSTATE the way the driver's error does, through its SQLState method alone.
type pgError struct{ state, message string }

func (e pgError) Error() string    { return e.message }
func (e pgError) SQLState() string { return e.state }

// The classification reads the SQLSTATE and the setting the message names, through a wrap, and
// nothing else is an isolation refusal.
func TestAnIsolationRefusalIsNamedByItsControl(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  error
		want string
	}{
		{pgError{"42501", `new row violates row-level security policy for table "membership"`}, controlWithCheck},
		{fmt.Errorf("membership: insert: %w", pgError{"42501", `new row violates row-level security policy for table "membership"`}), controlWithCheck},
		{pgError{"42501", `permission denied for table membership`}, ""},
		{pgError{"42704", `unrecognized configuration parameter "app.binding"`}, controlUnsetBinding},
		{pgError{"22P02", `invalid input syntax for type uuid: ""`}, controlUnsetBinding},
		{pgError{"22P02", `invalid input syntax for type boolean: ""`}, controlUnsetBinding},
		{pgError{"22P02", `invalid input syntax for type uuid: "x"`}, ""},
		{pgError{"23505", `duplicate key value violates unique constraint`}, ""},
		{errors.New(`new row violates row-level security policy`), ""},
	} {
		if got := isolationControl(c.err); got != c.want {
			t.Errorf("%v classified %q, want %q", c.err, got, c.want)
		}
	}
}

// A refusal is counted by its control and logged as one; another internal error is logged with its
// cause and not counted. Not parallel: it replaces the default logger, which other tests write to.
func TestAnIsolationRefusalIsCountedAndLogged(t *testing.T) {
	manual := sdkmetric.NewManualReader()
	signals, err := newSurfaceSignals(sdkmetric.NewMeterProvider(sdkmetric.WithReader(manual)), nil)
	if err != nil {
		t.Fatalf("newSurfaceSignals: %v", err)
	}
	var logs strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	request := httptest.NewRequest("POST", "/v1/memberships", nil)
	request = request.WithContext(withSignals(request.Context(), signals))
	recordInternal(request, pgError{"42501", `new row violates row-level security policy for table "membership"`})
	recordInternal(request, errors.New("a statement failed"))

	var rm metricdata.ResourceMetrics
	if err := manual.Collect(request.Context(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var counted int64
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != telemetry.IsolationRefusals.Name {
				continue
			}
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if v, ok := point.Attributes.Value(attribute.Key("control")); ok && v.AsString() == controlWithCheck {
					counted += point.Value
				}
			}
		}
	}
	if counted != 1 {
		t.Errorf("counted %d WITH CHECK refusals, want 1", counted)
	}
	if !strings.Contains(logs.String(), "an isolation control refused a statement") ||
		!strings.Contains(logs.String(), "control=with_check") || !strings.Contains(logs.String(), "a statement failed") {
		t.Errorf("the log reads:\n%s", logs.String())
	}
}
