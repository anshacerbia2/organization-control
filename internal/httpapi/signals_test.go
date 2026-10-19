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

	"github.com/anshacerbia2/organization-control/internal/invitation"
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

// A refused acceptance is counted and logged with the caller and nothing of the token or address, and
// answers the one 404 with its constant detail; an unavailable match answers 503 with a detail that
// carries none of its cause (ADR-ORG-007 §5.3, §5.4). Not parallel: it replaces the default logger.
func TestARefusedAcceptanceIsCountedAndAnswersOneDetail(t *testing.T) {
	manual := sdkmetric.NewManualReader()
	signals, err := newSurfaceSignals(sdkmetric.NewMeterProvider(sdkmetric.WithReader(manual)), nil)
	if err != nil {
		t.Fatalf("newSurfaceSignals: %v", err)
	}
	var logs strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	caller := Caller{Subject: mustID(t), Self: true}
	request := httptest.NewRequest("POST", "/v1/invitations/accept", nil)
	request = request.WithContext(withSignals(request.Context(), signals))

	refused := acceptanceError(request, caller, fmt.Errorf("wrapped: %w", invitation.ErrNotAcceptable))
	recorder := httptest.NewRecorder()
	writeError(recorder, request, refused)
	if recorder.Code != 404 || !strings.Contains(recorder.Body.String(), `"detail":"invitation: the invitation cannot be accepted"`) {
		t.Errorf("a refusal answered %d %s", recorder.Code, recorder.Body.String())
	}

	unavailable := acceptanceError(request, caller, fmt.Errorf("%w: %w", invitation.ErrIdentityUnavailable,
		errors.New("Post http://identity-control:8090/v1/principals/x/identifiers:match: connection refused")))
	recorder = httptest.NewRecorder()
	writeError(recorder, request, unavailable)
	if recorder.Code != 503 || strings.Contains(recorder.Body.String(), "connection refused") ||
		strings.Contains(recorder.Body.String(), "identity-control:8090") {
		t.Errorf("an unavailable match answered %d %s", recorder.Code, recorder.Body.String())
	}

	var rm metricdata.ResourceMetrics
	if err := manual.Collect(request.Context(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var counted int64
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == telemetry.InvitationAcceptancesRefused.Name {
				for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
					counted += point.Value
				}
			}
		}
	}
	if counted != 1 {
		t.Errorf("counted %d refused acceptances, want 1: an unavailable match is not a refusal", counted)
	}
	if !strings.Contains(logs.String(), "an invitation acceptance was refused") ||
		!strings.Contains(logs.String(), "principal_id="+caller.Subject.String()) {
		t.Errorf("the log reads:\n%s", logs.String())
	}
	if acceptanceError(request, caller, nil) != nil {
		t.Error("no error became one")
	}
}
