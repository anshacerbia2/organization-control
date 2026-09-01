package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// maxBody bounds a request body.
//
// Every body in this surface is a handful of fields. The bound exists so an unbounded read cannot
// be used to exhaust memory on a path that is reached before any domain rule runs.
const maxBody = 64 << 10

// decode reads a JSON body into T.
//
// Unknown fields are refused. A caller that misspells `expected_version` and receives 200 has been
// told the version was checked when it was not, and the next thing it does is act on that belief.
// `DisallowUnknownFields` turns that into a 400 naming the field.
func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var value T

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		var zero T
		platform.Problem(w, r, platform.ValidationFailed, decodeDetail(err))
		return zero, false
	}

	// A second value in the stream means the caller sent two documents and would otherwise have
	// the first silently applied.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		var zero T
		platform.Problem(w, r, platform.ValidationFailed, "The body must contain exactly one JSON document")
		return zero, false
	}

	return value, true
}

// decodeDecodeDetail phrasing is kept caller-facing: a decode failure names the field or the offset,
// which is what lets a client fix its request without reading this source.
func decodeDetail(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("The body is not valid JSON at byte %d", syntax.Offset)
	}
	var unmarshal *json.UnmarshalTypeError
	if errors.As(err, &unmarshal) {
		return fmt.Sprintf("Field %q expects %s", unmarshal.Field, unmarshal.Type)
	}
	if errors.Is(err, io.EOF) {
		return "The body is empty"
	}
	return err.Error()
}

// respond writes a success body.
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status is already written, so an encoding failure cannot become a problem document. It is
	// dropped rather than partially reported: the platform chain's logging middleware records the
	// response, and a half-written body with a 200 line is what an operator will see.
	_ = json.NewEncoder(w).Encode(body)
}

// pathUUID reads a UUID path segment.
//
// A malformed identifier is a 400 rather than a 404. The two differ for a client: 404 says the
// record is absent and the request was well formed, and retrying elsewhere is reasonable; 400 says
// the request itself is wrong and no retry helps.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (id.UUID, bool) {
	raw := r.PathValue(name)
	parsed, err := id.Parse(raw)
	if err != nil {
		platform.Problem(w, r, platform.ValidationFailed,
			fmt.Sprintf("Path segment %q is not a valid identifier", name))
		return id.UUID{}, false
	}
	return parsed, true
}

// floatQuery reads an optional numeric query parameter.
//
// An absent parameter takes the default; a present but unparseable one is an error rather than the
// default. Falling back silently would answer a question the caller did not ask — a threshold of
// 0.5 reported as if they had asked for it.
func floatQuery(r *http.Request, name string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("Query parameter %q is not a number", name)
	}
	return value, nil
}

// requireProvider refuses a tenant-scoped caller on a cross-Tenant route.
//
// The pool types already make the mistake a compile error inside the domain, and this is the
// transport-side half: a tenant caller reaching a provider route would otherwise receive
// `db.ErrWrongScope` as a 500, which reports a caller's error as the service's defect.
func requireProvider(w http.ResponseWriter, r *http.Request) (db.Scope, bool) {
	scope, ok := db.ScopeFrom(r.Context())
	if !ok {
		platform.Problem(w, r, platform.Internal, "The request could not be completed")
		return db.Scope{}, false
	}
	if !scope.IsProvider() {
		platform.Problem(w, r, platform.Forbidden,
			"This route requires provider authority, and the caller is scoped to a single Tenant")
		return db.Scope{}, false
	}
	if strings.TrimSpace(r.Header.Get(ReasonHeader)) == "" {
		// Checked here as well as in db, so the caller is told which header is missing rather than
		// receiving the domain's phrasing for a transport-level omission.
		platform.Problem(w, r, platform.ValidationFailed,
			"A cross-Tenant request must carry the "+ReasonHeader+" header")
		return db.Scope{}, false
	}
	return scope, true
}

// requireConsumerSelfOrProvider admits the two authorities that may act on a consumer's own
// records, and returns which consumer the request is accountable to.
//
// A registered consumer is admitted without the administrative reason header, and a provider is
// not. That asymmetry is the point rather than an exemption: a provider performing a fresh check
// is doing something out of the ordinary and owes an explanation, while a consumer doing it is
// the designed traffic -- and its accountability is the per-consumer meter, which cannot be
// forgotten the way a header can. Demanding a reason on every consumer call would produce a
// million rows saying "routine".
// requested is the consumer the request names -- from the path or the body. A consumer caller may
// only name itself: it is the whole reason the identity comes from the token, and a consumer able to
// bootstrap, snapshot, or report progress for another consumer could rewrite that consumer's
// recorded position and make a stale model look current.
func requireConsumerSelfOrProvider(w http.ResponseWriter, r *http.Request, requested string) (db.Scope, string, bool) {
	scope, ok := db.ScopeFrom(r.Context())
	if !ok {
		platform.Problem(w, r, platform.Internal, "The request could not be completed")
		return db.Scope{}, "", false
	}
	caller, ok := CallerFrom(r.Context())
	if !ok {
		platform.Problem(w, r, platform.AuthenticationRequired, "The request carries no authenticated caller")
		return db.Scope{}, "", false
	}

	if caller.Consumer != "" {
		if trimmed := strings.TrimSpace(requested); trimmed != "" && trimmed != caller.Consumer {
			platform.Problem(w, r, platform.Forbidden,
				"The request names a different consumer than the token; a consumer may only act as itself")
			return db.Scope{}, "", false
		}
		return scope, caller.Consumer, true
	}

	if !scope.IsProvider() {
		platform.Problem(w, r, platform.Forbidden,
			"This route requires provider authority or a registered consumer, and the caller is scoped to a single Tenant")
		return db.Scope{}, "", false
	}
	if strings.TrimSpace(r.Header.Get(ReasonHeader)) == "" {
		platform.Problem(w, r, platform.ValidationFailed,
			"A cross-Tenant request must carry the "+ReasonHeader+" header")
		return db.Scope{}, "", false
	}
	return scope, strings.TrimSpace(requested), true
}

// requireTenant refuses a provider caller on a tenant-scoped route.
//
// Refused rather than served, even though provider authority is the broader one. A provider caller
// on a tenant route has no Tenant to bind, so the request has no scope to run under — and choosing
// one for them would be this layer inventing the isolation boundary.
func requireTenant(w http.ResponseWriter, r *http.Request) (db.Scope, bool) {
	scope, ok := db.ScopeFrom(r.Context())
	if !ok {
		platform.Problem(w, r, platform.Internal, "The request could not be completed")
		return db.Scope{}, false
	}
	if scope.IsProvider() {
		platform.Problem(w, r, platform.Forbidden,
			"This route acts within one Tenant, and a provider caller carries none")
		return db.Scope{}, false
	}
	return scope, true
}

// seconds is a duration expressed in whole seconds on the wire.
//
// Go renders a `time.Duration` as a nanosecond integer in JSON, which no client should be expected
// to know. A named type with its own marshalling keeps the wire contract readable and keeps the
// domain signature a `time.Duration`.
type seconds time.Duration

func (s *seconds) UnmarshalJSON(raw []byte) error {
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return errors.New("expected a whole number of seconds")
	}
	*s = seconds(time.Duration(value) * time.Second)
	return nil
}

func (s seconds) Duration() time.Duration { return time.Duration(s) }
