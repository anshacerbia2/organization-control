package authority

// The privileged-access record read and reviewed, against the real engine as the provider role and
// the tenant role (ADR-ORG-002 §5.6, TDD-organization-control-001 §Privileged Access Review).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

type reviewFixture struct {
	review *AccessReview
	owner  *fdb.Pool
	base   context.Context
}

func accessReview(t *testing.T) reviewFixture {
	t.Helper()
	provider, owner, ctx := pools(t)
	rest := os.Getenv("TEST_DATABASE_URL")
	if index := strings.Index(rest, "://"); index >= 0 {
		rest = rest[index+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	runtime, err := fdb.Open(ctx, fdb.Config{Name: "authority-test-review-tenant", MaxConns: 2,
		DSN: fmt.Sprintf("postgres://organization_app:%s@%s", os.Getenv("TEST_RUNTIME_PASSWORD"), rest)})
	if err != nil {
		t.Fatalf("open the tenant pool: %v", err)
	}
	t.Cleanup(runtime.Close)
	providerPool, err := db.NewProviderPool(provider, noEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	tenantPool, err := db.NewTenantPool(runtime)
	if err != nil {
		t.Fatal(err)
	}
	review, err := NewAccessReview(providerPool, tenantPool)
	if err != nil {
		t.Fatal(err)
	}
	return reviewFixture{review: review, owner: owner, base: ctx}
}

// seed writes one access as the owner, at an instant relative to the database's clock.
func (f reviewFixture) seed(t *testing.T, actor id.UUID, authority string, tenant *id.UUID, ago string) {
	t.Helper()
	var activation any
	if authority == db.AuthorityActivation {
		activation = newID(t).String()
	}
	var tenantArg any
	if tenant != nil {
		tenantArg = tenant.String()
	}
	exec(t, f.base, f.owner, `INSERT INTO audit.privileged_access
	    (access_id, actor_id, correlation_id, reason, authority, activation_id, tenant_id, operation, occurred_at)
	    VALUES ($1, $2, $3, 'seeded', $4, $5::uuid, $6::uuid, 'GET /v1/tenants', now() - $7::interval)`,
		newID(t).String(), actor.String(), newID(t).String(), authority, activation, tenantArg, ago)
}

func unreviewedOf(t *testing.T, report []Unreviewed, actor id.UUID) *Unreviewed {
	t.Helper()
	for i := range report {
		if report[i].Actor == actor {
			return &report[i]
		}
	}
	return nil
}

// TestAReviewCoversTheAccessOfItsPeriodAndOnlyThat is the review's whole contract: it counts the
// provider accesses of its period, consumer rows left out, and they leave the unreviewed report while
// an access outside the period stays, overdue after seven days.
func TestAReviewCoversTheAccessOfItsPeriodAndOnlyThat(t *testing.T) {
	f := accessReview(t)
	actor, reviewer := newID(t), newID(t)
	f.seed(t, actor, db.AuthorityEligible, nil, "10 days")
	f.seed(t, actor, db.AuthorityEmergency, nil, "3 days")
	f.seed(t, actor, db.AuthorityActivation, nil, "2 days")
	f.seed(t, actor, db.AuthorityConsumer, nil, "1 hour")

	asReviewer := actingAs(t, f.base, reviewer)
	report, err := f.review.Unreviewed(asReviewer, "weekly review", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := unreviewedOf(t, report, actor)
	if got == nil || got.Unreviewed != 3 || got.Emergency != 1 || !got.Overdue {
		t.Fatalf("before the review the actor reads %+v; want 3 unreviewed, 1 emergency, overdue", got)
	}
	if want := got.OldestAt.Add(ReviewDue); !got.DueAt.Equal(want) {
		t.Errorf("due at %v, want %v: seven days after the oldest", got.DueAt, want)
	}

	now := time.Now().Add(-time.Second)
	review, err := f.review.Record(asReviewer, ReviewRequest{
		Actor: actor, From: now.Add(-5 * 24 * time.Hour), To: now, Outcome: OutcomeAppropriate,
		Statement: "the week's rotation, ticket OPS-12",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if review.Accesses != 2 || review.EmergencyAccesses != 1 || review.ReviewedBy != reviewer ||
		review.Statement != "the week's rotation, ticket OPS-12" {
		t.Errorf("the review recorded %+v; want 2 accesses, 1 emergency, by the reviewer", review)
	}

	report, err = f.review.Unreviewed(asReviewer, "weekly review", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := unreviewedOf(t, report, actor); got == nil || got.Unreviewed != 1 || got.Emergency != 0 || !got.Overdue {
		t.Errorf("after the review the actor reads %+v; want the one access outside the period, overdue", got)
	}

	page, err := f.review.Reviews(asReviewer, ReviewQuery{Actor: actor}, "weekly review")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Reviews) != 1 || page.Reviews[0].ID != review.ID || page.Next != nil {
		t.Errorf("the actor's reviews read %+v; want the one recorded", page)
	}
}

// TestAProviderDoesNotReviewItsOwnAccess is AC-5, by the service: the database's half is in
// internal/controldb.
func TestAProviderDoesNotReviewItsOwnAccess(t *testing.T) {
	f := accessReview(t)
	actor := newID(t)
	now := time.Now().Add(-time.Second)
	_, err := f.review.Record(actingAs(t, f.base, actor), ReviewRequest{
		Actor: actor, From: now.Add(-time.Hour), To: now, Outcome: OutcomeAppropriate, Statement: "mine",
	})
	if !errors.Is(err, ErrSelfReview) {
		t.Errorf("a self-review answered %v, want ErrSelfReview", err)
	}
}

func TestAReviewIsRefusedForAnImpossiblePeriod(t *testing.T) {
	f := accessReview(t)
	asReviewer := actingAs(t, f.base, newID(t))
	now := time.Now()
	for name, req := range map[string]ReviewRequest{
		"a period ending in the future":   {From: now.Add(-time.Hour), To: now.Add(time.Hour)},
		"a period ending where it starts": {From: now.Add(-time.Hour), To: now.Add(-time.Hour)},
		"an unknown outcome":              {From: now.Add(-2 * time.Hour), To: now.Add(-time.Hour), Outcome: "fine"},
	} {
		t.Run(name, func(t *testing.T) {
			req.Actor = newID(t)
			req.Statement = "a review"
			if req.Outcome == "" {
				req.Outcome = OutcomeAppropriate
			}
			if _, err := f.review.Record(asReviewer, req); !errors.Is(err, ErrReviewInvalid) {
				t.Errorf("answered %v, want ErrReviewInvalid", err)
			}
		})
	}
}

// TestTheProviderListFiltersAndPages covers each filter and the keyset.
func TestTheProviderListFiltersAndPages(t *testing.T) {
	f := accessReview(t)
	actor, tenant := newID(t), newID(t)
	f.seed(t, actor, db.AuthorityEmergency, &tenant, "3 days")
	f.seed(t, actor, db.AuthorityActivation, nil, "2 days")
	f.seed(t, actor, db.AuthorityActivation, &tenant, "1 day")
	asReviewer := actingAs(t, f.base, newID(t))

	count := func(query AccessQuery) int {
		t.Helper()
		query.Actor = actor
		page, err := f.review.List(asReviewer, query, "weekly review")
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Accesses)
	}
	from, to := time.Now().Add(-60*time.Hour), time.Now().Add(-36*time.Hour)
	for name, c := range map[string]struct {
		query AccessQuery
		want  int
	}{
		"the actor":             {AccessQuery{}, 3},
		"emergency":             {AccessQuery{Authority: db.AuthorityEmergency}, 1},
		"activation":            {AccessQuery{Authority: db.AuthorityActivation}, 2},
		"the Tenant":            {AccessQuery{Tenant: tenant}, 2},
		"the window":            {AccessQuery{From: &from, To: &to}, 1},
		"from alone":            {AccessQuery{From: &from}, 2},
		"the Tenant, emergency": {AccessQuery{Tenant: tenant, Authority: db.AuthorityEmergency}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := count(c.query); got != c.want {
				t.Errorf("%d rows, want %d", got, c.want)
			}
		})
	}

	first, err := f.review.List(asReviewer, AccessQuery{Actor: actor, Limit: 2}, "weekly review")
	if err != nil || len(first.Accesses) != 2 || first.Next == nil {
		t.Fatalf("the first page reads %+v, %v; want two rows and a next", first, err)
	}
	second, err := f.review.List(asReviewer, AccessQuery{Actor: actor, Limit: 2, After: *first.Next}, "weekly review")
	if err != nil || len(second.Accesses) != 1 || second.Next != nil {
		t.Fatalf("the second page reads %+v, %v; want the last row and no next", second, err)
	}

	backwards := time.Now()
	if _, err := f.review.List(asReviewer, AccessQuery{From: &backwards, To: &from}, "r"); !errors.Is(err, ErrAccessQueryInvalid) {
		t.Errorf("a to before from answered %v, want ErrAccessQueryInvalid", err)
	}
	if _, err := f.review.List(asReviewer, AccessQuery{Authority: "root"}, "r"); !errors.Is(err, ErrAccessQueryInvalid) {
		t.Errorf("an unknown authority answered %v, want ErrAccessQueryInvalid", err)
	}
}

// TestATenantListsTheProviderAccessThatNamedIt reads through the view as the tenant role: its own
// Tenant's provider rows, neither another Tenant's nor a consumer's.
func TestATenantListsTheProviderAccessThatNamedIt(t *testing.T) {
	f := accessReview(t)
	actor, mine, theirs := newID(t), newID(t), newID(t)
	f.seed(t, actor, db.AuthorityEmergency, &mine, "2 days")
	f.seed(t, actor, db.AuthorityActivation, &mine, "1 day")
	f.seed(t, actor, db.AuthorityConsumer, &mine, "1 hour")
	f.seed(t, actor, db.AuthorityEmergency, &theirs, "1 hour")

	scope, err := db.TenantScope(mine, newID(t), newID(t))
	if err != nil {
		t.Fatal(err)
	}
	asTenant := db.WithScope(f.base, scope)
	page, err := f.review.TenantList(asTenant, AccessQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Accesses) != 2 {
		t.Fatalf("the Tenant reads %d rows, want its 2 provider rows", len(page.Accesses))
	}
	for _, access := range page.Accesses {
		if access.Tenant == nil || *access.Tenant != mine || access.Authority == db.AuthorityConsumer {
			t.Errorf("the Tenant read %+v", access)
		}
	}
	emergency, err := f.review.TenantList(asTenant, AccessQuery{Authority: db.AuthorityEmergency})
	if err != nil || len(emergency.Accesses) != 1 {
		t.Errorf("the emergency filter reads %+v, %v; want one row", emergency, err)
	}

	for name, query := range map[string]AccessQuery{
		"a Tenant":   {Tenant: theirs},
		"an actor":   {Actor: actor},
		"a consumer": {Authority: db.AuthorityConsumer},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := f.review.TenantList(asTenant, query); !errors.Is(err, ErrAccessQueryInvalid) {
				t.Errorf("answered %v, want ErrAccessQueryInvalid", err)
			}
		})
	}
}
