package httpapi

// The `Idempotency-Key` surface.
//
// # What this middleware does and does not own
//
// It attaches a claim to the request context and records the response afterwards. It does not make
// the claim: `internal/db` does that, inside the scoped transaction the service opens, because the
// claim has to commit with the effect it guards. A claim made here would commit separately, and a
// key held by a mutation that then rolled back refuses every retry of a request that never happened
// â€” reported as "already in progress", which sends whoever is debugging to look for a concurrent
// request that does not exist. `TestAFailedMutationReleasesItsKey` in `internal/db` fails if the
// claim is moved out of that transaction.
//
// # The window this design leaves
//
// `idempotency.Complete` needs the status and body, and neither exists until the handler has
// rendered them â€” so the completion happens here, after the domain transaction has committed. A
// process dying in between leaves a key claimed and uncompleted, and later retries of it are refused
// rather than replayed. The mutation happened exactly once, which is the half that matters; what is
// lost is being told what it returned. Closing the window entirely would mean the handler owning the
// transaction, which is a `Within` variant on some thirty service methods across eight packages.

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/observability"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// IdempotencyHeader is the header a caller supplies to make a mutation retryable.
const IdempotencyHeader = "Idempotency-Key"

// maxIdempotentBody bounds what is buffered to compute a digest.
//
// The body has to be read to be hashed and read again by the handler, so it is held in memory for
// the length of the request. A bound rather than none: without it, a caller supplying an
// `Idempotency-Key` and a large body would decide how much memory this process uses.
//
// It sits above any legitimate request on this surface â€” every body here is a small JSON command â€”
// and a body past it is refused rather than silently unclaimed, because a caller that supplied a key
// and had it ignored would believe its retries were safe.
const maxIdempotentBody = 1 << 20 // 1 MiB

// ClaimCompleter records the response a completed mutation returned.
//
// An interface rather than `*db.ClaimStore` so this package cannot reach past the one method it
// needs, and so the completion can be asserted without a database.
type ClaimCompleter interface {
	Complete(ctx stdcontext.Context, claim db.Claim, status int, body json.RawMessage) error
}

// Idempotent honours the `Idempotency-Key` header.
//
// A request without the header passes through untouched, which is what keeps every read path and
// every existing caller unaffected. TDD-organization-control-003 Â§"API / Interface" states that every
// mutation *requires* the header; enforcing that is a change to the client contract rather than to
// this mechanism, and it belongs in one deliberate step rather than as a side effect of adding the
// mechanism. See ROADMAP.md.
//
// It must run after authentication and scope resolution: the claim is scoped per authenticated
// caller, so a claim built before the caller is known would either be global â€” one caller's key
// usable by another â€” or absent.
func Idempotent(store ClaimCompleter, telemetry *observability.Telemetry) (Middleware, error) {
	if store == nil {
		return nil, errors.New("httpapi: a claim completer is required")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(IdempotencyHeader)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			// A key on a request that changes nothing is refused rather than honoured. Claiming it
			// would consume the key against a read, and the caller's later mutation with the same
			// key would be answered with the read's response.
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				platform.Problem(w, r, platform.ValidationFailed, "An "+IdempotencyHeader+" is not accepted on a read")
				return
			}

			caller, ok := CallerFrom(r.Context())
			if !ok {
				// Unreachable behind the chain this runs in, and refused rather than passed through
				// if the ordering is ever changed: a claim with no caller in it is a claim any
				// caller could use.
				platform.Problem(w, r, platform.ValidationFailed, "The request has no authenticated caller")
				return
			}

			body, ok := buffer(w, r)
			if !ok {
				return
			}

			claim := db.Claim{
				Scope: claimScope(caller),
				Key:   key,
				// Method and path are in the digest as well as the body. Without them, one key
				// reused across two different routes with identical bodies would replay the first
				// route's response for the second.
				Digest: db.Digest([]byte(r.Method), []byte(r.URL.Path), body),
			}

			ctx := db.WithClaim(r.Context(), claim)
			captured := &capturingWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(captured, r.WithContext(ctx))

			// Only a claim this request actually reserved is completed, and only for a response the
			// caller should get again. A refusal is not recorded: the key was released with the
			// rolled-back transaction, and storing a 4xx for replay would answer a corrected retry
			// with the error the first attempt earned.
			if !db.ClaimMade(ctx) || captured.status < 200 || captured.status > 299 {
				return
			}
			if !json.Valid(captured.body) {
				return
			}

			if err := store.Complete(ctx, claim, captured.status, captured.body); err != nil {
				// Logged, never surfaced. The mutation has committed and the caller has its
				// response; turning a bookkeeping failure into an error would tell the client the
				// work did not happen. The cost is that later retries of this key are refused as in
				// progress rather than replayed, which is the safe direction.
				if telemetry != nil {
					telemetry.Logger(r.Context()).WarnContext(r.Context(),
						"the idempotency response was not recorded",
						slog.String("error", err.Error()),
						slog.Int("status", captured.status))
				}
			}
		})
	}, nil
}

// claimScope identifies the authenticated caller the key belongs to.
//
// The subject is included, not only the authority. Two provider operators sharing a key namespace
// would let one replay the other's response, and a key is a value a client chooses â€” so it is
// guessable by construction.
func claimScope(caller Caller) string {
	if caller.Provider {
		return "provider:" + caller.Subject.String()
	}
	return "tenant:" + caller.Tenant.String() + ":" + caller.Subject.String()
}

// buffer reads the body so it can be hashed, and puts it back for the handler.
func buffer(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}

	// One byte past the bound, so a body exactly at the limit is accepted and one over is detected
	// rather than silently truncated into a digest that would match a different request.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIdempotentBody+1))
	if err != nil {
		platform.Problem(w, r, platform.ValidationFailed, "The request body could not be read")
		return nil, false
	}
	if len(body) > maxIdempotentBody {
		platform.Problem(w, r, platform.ValidationFailed, fmt.Sprintf(
			"A request carrying an %s may not exceed %d bytes", IdempotencyHeader, maxIdempotentBody))
		return nil, false
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

// capturingWriter records the response so it can be stored for replay, and writes it through.
//
// It buffers rather than tees to the store as it goes, because a response is only worth replaying
// once it is complete: a body captured up to a mid-write failure would be stored as the answer to a
// request that failed.
type capturingWriter struct {
	http.ResponseWriter
	status  int
	body    []byte
	written bool
}

func (c *capturingWriter) WriteHeader(status int) {
	if !c.written {
		c.status = status
		c.written = true
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *capturingWriter) Write(p []byte) (int, error) {
	if !c.written {
		// net/http implies 200 on a first Write with no WriteHeader. Recorded here so a handler
		// that only writes a body is stored with the status the client actually saw.
		c.status = http.StatusOK
		c.written = true
	}
	if len(c.body)+len(p) <= maxIdempotentBody {
		c.body = append(c.body, p...)
	}
	return c.ResponseWriter.Write(p)
}
