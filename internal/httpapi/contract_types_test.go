package httpapi

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi-validator/schema_validation"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	"github.com/pb33f/libopenapi/renderer"

	occontext "github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/projection"
)

// The route tests of this service stop every request before the database (routes_test.go,
// refusingTransactor), so a handler's success body is not produced by any of them. This test checks
// what a handler would write: the Go type it writes (a view, or a response struct) against the
// operation's schema, and the Go type it reads against the operation's request body.
//
// For each operation the table names, a document is generated from the schema, decoded into the type
// with unknown fields refused (a field the schema documents that the type cannot carry fails), and
// written back (a field the type writes that the schema does not document, or a value the schema
// refuses, fails). That is the contract of the type, not of a handler's wiring: which type a handler
// writes is what the table records, and what ROADMAP item 67 replaces with an exchange against a
// database.

type typed struct {
	operation string
	status    int
	response  any // the type a handler writes for the status, nil when the status has no body
	request   any // the type a handler reads, nil when it reads no body
}

var typedOperations = []typed{
	{"GET /v1/memberships", 200, membershipPageView{}, nil},
	{"POST /v1/memberships", 201, membershipResultView{}, grantMembershipRequest{}},
	{"GET /v1/memberships/{membership_id}", 200, membershipView{}, nil},
	{"POST /v1/memberships/{membership_id}/suspend", 200, membershipResultView{}, membershipCommand{}},
	{"POST /v1/memberships/{membership_id}/restore", 200, membershipResultView{}, membershipCommand{}},
	{"POST /v1/memberships/{membership_id}/revoke", 200, membershipResultView{}, membershipCommand{}},
	{"GET /v1/memberships/{membership_id}/enforcement", 200, enforcementView{}, nil},
	{"POST /v1/membership-batches", 201, batchView{}, membershipBatchRequest{}},
	{"GET /v1/membership-batches/{batch_id}", 200, batchView{}, nil},
	{"POST /v1/membership-batches/{batch_id}/execute", 200, batchView{}, executeBatchRequest{}},
	{"GET /v1/workspaces", 200, workspacePageView{}, nil},
	{"POST /v1/workspaces", 201, workspaceView{}, createWorkspaceRequest{}},
	{"GET /v1/workspaces/{workspace_id}", 200, workspaceView{}, nil},
	{"POST /v1/workspaces/{workspace_id}/archive", 200, workspaceView{}, workspaceCommand{}},
	{"POST /v1/workspaces/{workspace_id}/restore", 200, workspaceView{}, workspaceCommand{}},
	{"POST /v1/workspaces/{workspace_id}/retire", 200, workspaceView{}, workspaceCommand{}},
	{"GET /v1/invitations", 200, invitationPageView{}, nil},
	{"POST /v1/invitations", 201, issuedView{}, issueInvitationRequest{}},
	{"GET /v1/invitations/{invitation_id}", 200, invitationView{}, nil},
	{"POST /v1/invitations/{invitation_id}/revoke", 200, invitationView{}, nil},
	{"POST /v1/invitations/lookup", 200, lookupResponse{}, acceptInvitationRequest{}},
	{"POST /v1/invitations/accept", 200, acceptInvitationResponse{}, acceptInvitationRequest{}},
	{"POST /v1/invitations/resolve", 200, resolvedInvitationView{}, resolveInvitationRequest{}},
	{"POST /v1/invitations/expire-lapsed", 200, batchResponse{}, batchRequest{}},
	{"GET /v1/organizations", 200, organizationPageView{}, nil},
	{"POST /v1/organizations", 201, organizationView{}, registerOrganizationRequest{}},
	{"GET /v1/organizations/{organization_id}", 200, organizationView{}, nil},
	{"POST /v1/organizations/{organization_id}/suspend", 200, organizationView{}, providerCommand{}},
	{"POST /v1/organizations/{organization_id}/restore", 200, organizationView{}, providerCommand{}},
	{"POST /v1/organizations/{organization_id}/retire", 200, organizationView{}, providerCommand{}},
	{"GET /v1/tenants", 200, tenantPageView{}, nil},
	{"POST /v1/tenants", 201, requestedView{}, requestTenantRequest{}},
	{"GET /v1/tenants/{tenant_id}", 200, tenantDetailView{}, nil},
	{"POST /v1/tenants/{tenant_id}/activate", 200, tenantResultView{}, providerCommand{}},
	{"POST /v1/tenants/{tenant_id}/suspend", 200, tenantResultView{}, providerCommand{}},
	{"POST /v1/tenants/{tenant_id}/restore", 200, tenantResultView{}, providerCommand{}},
	{"POST /v1/tenants/{tenant_id}/administrators", 201, administratorView{}, grantTenantAdministratorRequest{}},
	{"POST /v1/tenants/{tenant_id}/administrators/{grant_id}/revoke", 200, tenantAdminGrantView{}, nil},
	{"GET /v1/tenants/{tenant_id}/memberships/{membership_id}", 200, membershipView{}, nil},
	{"GET /v1/tenants/{tenant_id}/memberships/{membership_id}/enforcement", 200, enforcementView{}, nil},
	{"POST /v1/tenants/{tenant_id}/memberships/{membership_id}/suspend", 200, membershipResultView{}, membershipCommand{}},
	{"POST /v1/tenants/{tenant_id}/memberships/{membership_id}/revoke", 200, membershipResultView{}, membershipCommand{}},
	{"POST /v1/tenants/{tenant_id}/provisioning", 200, tenantResultView{}, providerCommand{}},
	{"POST /v1/provisioning/realized", 200, resolutionView{}, provisioningOutcomeRequest{}},
	{"POST /v1/provisioning/failed", 200, resolutionView{}, provisioningOutcomeRequest{}},
	{"POST /v1/provisioning/sweep-unresolved", 200, batchResponse{}, batchRequest{}},
	{"GET /v1/offboardings", 200, offboardingPageView{}, nil},
	{"POST /v1/offboardings", 201, offboardingView{}, beginOffboardingRequest{}},
	{"GET /v1/offboardings/{offboarding_id}", 200, offboardingView{}, nil},
	{"POST /v1/offboardings/{offboarding_id}/freeze", 200, batchResponse{}, batchRequest{}},
	{"POST /v1/offboardings/{offboarding_id}/complete-freeze", 200, offboardingView{}, nil},
	{"POST /v1/offboardings/{offboarding_id}/release", 200, offboardingView{}, nil},
	{"POST /v1/offboardings/{offboarding_id}/retire", 200, offboardingView{}, providerCommand{}},
	{"POST /v1/offboardings/{offboarding_id}/cancel", 200, offboardingView{}, providerCommand{}},
	{"POST /v1/offboardings/{offboarding_id}/legal-hold", 200, offboardingView{}, legalHoldRequest{}},
	{"GET /v1/offboardings/{offboarding_id}/obligations", 200, outstandingResponse{}, nil},
	{"POST /v1/offboardings/{offboarding_id}/obligations", 201, obligationView{}, raiseObligationRequest{}},
	{"POST /v1/offboardings/{offboarding_id}/deprovisioning", 204, nil, deprovisioningRequest{}},
	{"POST /v1/offboardings/{offboarding_id}/deprovisioning/resend", 200, offboardingView{}, nil},
	{"POST /v1/obligations/{obligation_id}/resolve", 200, obligationView{}, resolveObligationRequest{}},
	{"POST /v1/provider-grants", 201, providerGrantView{}, grantProviderRequest{}},
	{"POST /v1/provider-grants/{grant_id}/revoke", 200, providerGrantView{}, nil},
	{"GET /v1/privileged-access", 200, accessPageView{}, nil},
	{"GET /v1/privileged-access/reviews", 200, reviewPageView{}, nil},
	{"POST /v1/privileged-access/reviews", 201, reviewView{}, recordReviewRequest{}},
	{"GET /v1/provider-access", 200, accessPageView{}, nil},
	{"POST /v1/provider-activations", 201, providerActivationView{}, requestActivationRequest{}},
	{"POST /v1/provider-activations/{activation_id}/approve", 200, providerActivationView{}, nil},
	{"POST /v1/provider-activations/{activation_id}/deny", 200, providerActivationView{}, nil},
	{"POST /v1/provider-activations/{activation_id}/end", 200, providerActivationView{}, nil},
	{"GET /v1/projections/consumers", 200, consumerPageView{}, nil},
	{"POST /v1/projections/consumers", 201, consumerView{}, registerConsumerRequest{}},
	{"GET /v1/projections/consumers/{consumer_id}", 200, consumerView{}, nil},
	{"POST /v1/projections/consumers/{consumer_id}/retire", 204, nil, nil},
	{"POST /v1/projections/consumers/{consumer_id}/progress", 200, consumerView{}, progressRequest{}},
	{"POST /v1/projections/consumers/{consumer_id}/bootstrap", 200, consumerView{}, bootstrapRequest{}},
	{"GET /v1/projections/consumers/{consumer_id}/dead-letters", 200, deadLetterPageView{}, nil},
	{"POST /v1/projections/snapshot", 200, projection.Page{}, snapshotRequest{}},
	{"POST /v1/projections/provider-authority/snapshot", 200, projection.ProviderPage{}, snapshotRequest{}},
	{"GET /v1/projections/frontier", 200, frontierResponse{}, nil},
	{"POST /v1/projections/reconcile", 200, projection.Result{}, reconcileRequest{}},
	{"POST /v1/projections/advance-versions", 200, advanceView{}, reconcileRequest{}},
	{"GET /v1/tenant-administration-pause", 200, pauseView{}, nil},
	{"POST /v1/tenant-administration-pause", 200, pauseView{}, setPauseRequest{}},
	{"POST /v1/dead-letters/{event_id}/consumers/{consumer}/replay", 202, replayResponse{}, nil},
	{"POST /v1/dead-letters/{event_id}/consumers/{consumer}/resolve", 200, resolveResponse{}, resolveRequest{}},
	{"POST /v1/dead-letters/{event_id}/consumers/{consumer}/waive", 200, waiveResponse{}, waiveRequest{}},
	{"GET /v1/principals/{principal_id}/contexts", 200, contextPageView{}, nil},
	{"POST /v1/context/verify", 200, occontext.Decision{}, verifyContextRequest{}},
	{"POST /v1/context/switch-eligible", 200, occontext.Decision{}, verifyContextRequest{}},
	{"POST /v1/context/rate", 200, occontext.Rate{}, rateRequest{}},
	{"GET /v1/context/rate/over-threshold", 200, ratesResponse{}, nil},
	{"GET /v1/reporters", 200, reporterPageView{}, nil},
	{"POST /v1/reporters", 201, reporterView{}, registerReporterRequest{}},
	{"GET /v1/reporters/{reporter_id}", 200, reporterView{}, nil},
	{"POST /v1/reporters/{reporter_id}/retire", 200, reporterView{}, retireReporterRequest{}},
}

// mapResponses are the operations whose handler writes a map literal, which has no type to check.
// Their success responses wait for an exchange (ROADMAP item 67). The reason is the same for each.
var mapResponses = map[string]bool{
	"GET /v1/tenants/{tenant_id}/administrators":   true,
	"GET /v1/provider-grants":                      true,
	"GET /v1/provider-grants:emergency-validation": true,
	"GET /v1/privileged-access:unreviewed":         true,
	"GET /v1/provider-activations":                 true,
	"GET /v1/provider-activations/grants":          true,
}

func init() {
	for operation := range mapResponses {
		contractWaived[operation] = "the handler writes a map literal"
	}
}

func TestTheTypesHandlersWriteAndReadMatchTheSpecification(t *testing.T) {
	if err := loadContract(); err != nil {
		t.Fatal(err)
	}
	validator := schema_validation.NewSchemaValidator()
	for _, c := range typedOperations {
		t.Run(c.operation+" "+strconv.Itoa(c.status), func(t *testing.T) {
			method, path, _ := strings.Cut(c.operation, " ")
			item, ok := contract.model.Paths.PathItems.Get(path)
			if !ok {
				t.Fatalf("%s is not a path of the specification", path)
			}
			operation := item.GetOperations().GetOrZero(strings.ToLower(method))
			if operation == nil {
				t.Fatalf("%s has no such operation", c.operation)
			}
			if c.response != nil {
				response, ok := operation.Responses.Codes.Get(strconv.Itoa(c.status))
				if !ok || response.Content == nil {
					t.Fatalf("the specification has no %d body for %s", c.status, c.operation)
				}
				media, ok := response.Content.Get("application/json")
				if !ok {
					t.Fatalf("%s %d is not application/json", c.operation, c.status)
				}
				roundTrip(t, validator, media.Schema.Schema(), c.response, "the response", true)
			}
			if c.request != nil {
				if operation.RequestBody == nil {
					t.Fatalf("%s documents no request body, and the handler reads a %T", c.operation, c.request)
				}
				media, ok := operation.RequestBody.Content.Get("application/json")
				if !ok {
					t.Fatalf("%s request is not application/json", c.operation)
				}
				roundTrip(t, validator, media.Schema.Schema(), c.request, "the request", false)
			}
			contract.mu.Lock()
			defer contract.mu.Unlock()
			key := c.operation
			if contract.seen[key] == nil {
				contract.seen[key] = map[int]bool{}
			}
			contract.seen[key][c.status] = true
		})
	}
}

// roundTrip generates a document from the schema, reads it into the type, writes it back, and holds
// what was written to the schema and to the document's own fields.
func roundTrip(t *testing.T, validator schema_validation.SchemaValidator, schema *base.Schema, value any, what string, emits bool) {
	t.Helper()
	generator := renderer.NewMockGenerator(renderer.JSON)
	generator.DisableRequiredCheck()
	generator.SetSeed(1)
	sample, err := generator.GenerateMock(schema, "")
	if err != nil || len(sample) == 0 {
		t.Fatalf("generate %s from the schema: %v", what, err)
	}
	target := reflect.New(reflect.TypeOf(value)).Interface()
	decoder := json.NewDecoder(bytes.NewReader(sample))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("%s: the schema documents what %T cannot carry: %v\nsample: %s", what, value, err, sample)
	}
	if !emits {
		// A request type is only read: a field with its own decoder (a duration in seconds) does not
		// write what it reads, and nothing writes it.
		return
	}
	written, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("write %T: %v", value, err)
	}
	// The generator does not honour every constraint (a maximum, a constant). When its own document
	// fails the schema, what the type writes is not held to it, and the fields still are.
	if ok, _ := validator.ValidateSchemaBytes(schema, sample); !ok {
		t.Logf("%s: the generated document does not satisfy the schema; its values are not checked", what)
	} else if ok, problems := validator.ValidateSchemaBytes(schema, written); !ok {
		t.Errorf("%s: %T writes what the schema refuses:\n%s\nwritten: %s", what, value, describe(problems), written)
	}
	var documented, got any
	_ = json.Unmarshal(sample, &documented)
	_ = json.Unmarshal(written, &got)
	if extra := undocumented("$", documented, got); len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("%s: %T writes fields the schema does not document: %s", what, value, strings.Join(extra, ", "))
	}
}

// undocumented lists the fields of got that documented lacks, by path. An array is compared through
// its first element, and a field the type leaves out is not a field it writes.
func undocumented(path string, documented, got any) []string {
	var extra []string
	switch got := got.(type) {
	case map[string]any:
		documented, _ := documented.(map[string]any)
		if documented == nil {
			return nil
		}
		for key, value := range got {
			if _, ok := documented[key]; !ok {
				extra = append(extra, path+"."+key)
				continue
			}
			extra = append(extra, undocumented(path+"."+key, documented[key], value)...)
		}
	case []any:
		documented, _ := documented.([]any)
		if len(documented) == 0 {
			return nil
		}
		for i, element := range got {
			extra = append(extra, undocumented(path+"["+strconv.Itoa(i)+"]", documented[0], element)...)
		}
	}
	return extra
}
