package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"

	"github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

// registry names every sentinel by the identifier the source declares it under, and holds its value.
//
// It exists to bridge a gap Go cannot close at runtime: `TestSentinelRegistryCoversSource` finds
// sentinels by reading the source, which yields names, and `TestEverySentinelIsMapped` needs values.
// Without the source walk, this file would be a list somebody updates when they remember to, and the
// mapping test would pass by testing only what it already knew about — a control that reports
// success because it never looked at the thing that changed.
//
// The two tests interlock. Declare a new sentinel and the source walk fails until it is added here;
// add it here and the mapping test fails until it is classified in `mapping`. Neither can be
// satisfied by editing only one file.
var registry = map[string]error{
	"db.ErrNoScope":        db.ErrNoScope,
	"db.ErrWrongScope":     db.ErrWrongScope,
	"db.ErrReasonRequired": db.ErrReasonRequired,

	"membership.ErrUnknownAction":     membership.ErrUnknownAction,
	"membership.ErrTransitionRefused": membership.ErrTransitionRefused,
	"membership.ErrInvalid":           membership.ErrInvalid,
	"membership.ErrNotFound":          membership.ErrNotFound,
	"membership.ErrRevoked":           membership.ErrRevoked,

	"tenant.ErrUnknownAction":           tenant.ErrUnknownAction,
	"tenant.ErrTransitionRefused":       tenant.ErrTransitionRefused,
	"tenant.ErrRetired":                 tenant.ErrRetired,
	"tenant.ErrInvalid":                 tenant.ErrInvalid,
	"tenant.ErrNotFound":                tenant.ErrNotFound,
	"tenant.ErrVersionMismatch":         tenant.ErrVersionMismatch,
	"tenant.ErrProvisioningNotRealized": tenant.ErrProvisioningNotRealized,
	"tenant.ErrSponsorNotActive":        tenant.ErrSponsorNotActive,

	"organization.ErrUnknownAction":     organization.ErrUnknownAction,
	"organization.ErrTransitionRefused": organization.ErrTransitionRefused,
	"organization.ErrRetired":           organization.ErrRetired,
	"organization.ErrInvalid":           organization.ErrInvalid,
	"organization.ErrNotFound":          organization.ErrNotFound,
	"organization.ErrVersionMismatch":   organization.ErrVersionMismatch,
	"organization.ErrTenantsNotRetired": organization.ErrTenantsNotRetired,

	"workspace.ErrUnknownAction":      workspace.ErrUnknownAction,
	"workspace.ErrTransitionRefused":  workspace.ErrTransitionRefused,
	"workspace.ErrRetired":            workspace.ErrRetired,
	"workspace.ErrInvalid":            workspace.ErrInvalid,
	"workspace.ErrNotFound":           workspace.ErrNotFound,
	"workspace.ErrVersionMismatch":    workspace.ErrVersionMismatch,
	"workspace.ErrMembershipsPresent": workspace.ErrMembershipsPresent,

	"invitation.ErrUnknownAction":     invitation.ErrUnknownAction,
	"invitation.ErrTransitionRefused": invitation.ErrTransitionRefused,
	"invitation.ErrSettled":           invitation.ErrSettled,
	"invitation.ErrInvalid":           invitation.ErrInvalid,
	"invitation.ErrNotFound":          invitation.ErrNotFound,
	"invitation.ErrExpired":           invitation.ErrExpired,
	"invitation.ErrTenantNotActive":   invitation.ErrTenantNotActive,
	"invitation.ErrAlreadyMember":     invitation.ErrAlreadyMember,
	"invitation.ErrTTL":               invitation.ErrTTL,
	"invitation.ErrToken":             invitation.ErrToken,

	"offboarding.ErrInvalid":                  offboarding.ErrInvalid,
	"offboarding.ErrNotFound":                 offboarding.ErrNotFound,
	"offboarding.ErrStageRefused":             offboarding.ErrStageRefused,
	"offboarding.ErrObligationsOutstanding":   offboarding.ErrObligationsOutstanding,
	"offboarding.ErrLegalHold":                offboarding.ErrLegalHold,
	"offboarding.ErrWrongDomain":              offboarding.ErrWrongDomain,
	"offboarding.ErrAlreadyResolved":          offboarding.ErrAlreadyResolved,
	"offboarding.ErrAmbiguousOutcome":         offboarding.ErrAmbiguousOutcome,
	"offboarding.ErrDeprovisioningIncomplete": offboarding.ErrDeprovisioningIncomplete,

	"projection.ErrInvalid":            projection.ErrInvalid,
	"projection.ErrPageSize":           projection.ErrPageSize,
	"projection.ErrCursor":             projection.ErrCursor,
	"projection.ErrNotRegistered":      projection.ErrNotRegistered,
	"projection.ErrNoSnapshotMark":     projection.ErrNoSnapshotMark,
	"projection.ErrMarkWentBackwards":  projection.ErrMarkWentBackwards,
	"projection.ErrReportMarkRequired": projection.ErrReportMarkRequired,

	"context.ErrInvalid":         context.ErrInvalid,
	"context.ErrNotRegistered":   context.ErrNotRegistered,
	"context.ErrRequestRequired": context.ErrRequestRequired,
}

// sentinelsInSource reads every exported `Err*` declared under internal/, by package.
func sentinelsInSource(t *testing.T) map[string]struct{} {
	t.Helper()

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve internal/: %v", err)
	}

	found := map[string]struct{}{}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			decl, ok := decl.(*ast.GenDecl)
			if !ok || decl.Tok != token.VAR {
				continue
			}
			for _, spec := range decl.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range spec.Names {
					if strings.HasPrefix(name.Name, "Err") && name.IsExported() {
						found[file.Name.Name+"."+name.Name] = struct{}{}
					}
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk internal/: %v", walkErr)
	}

	// A walk that found nothing would let both tests pass while proving nothing, which is the
	// failure this whole file exists to prevent. The floor is well below the real count so it
	// catches a broken walk without failing every time a sentinel is added or removed.
	if len(found) < 40 {
		t.Fatalf("the source walk found only %d sentinels, so it is not working", len(found))
	}
	return found
}

func TestSentinelRegistryCoversSource(t *testing.T) {
	t.Parallel()

	found := sentinelsInSource(t)

	for name := range found {
		if _, ok := registry[name]; !ok {
			t.Errorf("%s is declared in the source but absent from the registry, so nothing checks "+
				"that it reaches a caller as anything other than a 500", name)
		}
	}

	// The reverse direction too. A registry entry for a sentinel the source no longer declares
	// would not fail to compile if the identifier still exists under another meaning, and a stale
	// entry makes the count look healthier than it is.
	for name := range registry {
		if _, ok := found[name]; !ok {
			t.Errorf("%s is in the registry but the source walk did not find it", name)
		}
	}
}

func TestEverySentinelIsMapped(t *testing.T) {
	t.Parallel()

	for name, err := range registry {
		problem, mapped := problemFor(err)
		if !mapped {
			t.Errorf("%s reaches a caller as an unclassified 500", name)
			continue
		}

		// Wrapped, as every service returns them. `errors.Is` should still match, and a table that
		// compared with `==` would pass the bare case and fail every real one.
		wrapped := fmt.Errorf("service: doing the thing: %w", err)
		wrappedProblem, wrappedMapped := problemFor(wrapped)
		if !wrappedMapped || wrappedProblem != problem {
			t.Errorf("%s classifies as %v bare but %v (mapped=%v) when wrapped",
				name, problem, wrappedProblem, wrappedMapped)
		}
	}
}

func TestUnmappedErrorRevealsNothing(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/memberships", nil)
	leak := errors.New(`pq: duplicate key value violates unique constraint "membership_pkey"`)

	writeError(recorder, request, leak)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("an unmapped error answered %d, want 500", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "membership_pkey") {
		t.Errorf("the response carried the constraint name:\n%s", recorder.Body.String())
	}
}

// TestValidationFailureNamesTheField is what the ErrInvalid sweep bought.
//
// Before it, every validation failure in the services was a bare errors.New, so a caller who omitted
// `provenance` received 500 — which says the service is broken rather than that the request is, and
// sends whoever is debugging it to the wrong side of the boundary.
func TestValidationFailureNamesTheField(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/memberships", nil)

	writeError(recorder, request, fmt.Errorf("%w: provenance is required", membership.ErrInvalid))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a missing required field answered %d, want 400", recorder.Code)
	}

	var document platform.ProblemDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("the response is not a problem document: %v", err)
	}
	if !strings.Contains(document.Detail, "provenance is required") {
		t.Errorf("the detail did not name the field: %q", document.Detail)
	}
}

func TestRefusalDetailReachesTheCaller(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/memberships/x/suspend", nil)

	writeError(recorder, request, fmt.Errorf("membership: suspend from revoked: %w", membership.ErrTransitionRefused))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("a refused transition answered %d, want 409", recorder.Code)
	}

	var document platform.ProblemDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("the response is not a problem document: %v\n%s", err, recorder.Body.String())
	}
	// The detail is what a caller acts on. A 409 whose detail was suppressed tells an operator
	// only that something conflicted, which is the same as telling them nothing.
	if !strings.Contains(document.Detail, "suspend from revoked") {
		t.Errorf("the refusal detail did not reach the caller: %q", document.Detail)
	}
}
