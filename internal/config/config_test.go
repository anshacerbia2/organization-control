package config

// The provisioning bounds and the one relationship between them that is worth refusing at startup.
//
// `Load` reads process-wide state, so these do not run in parallel: `t.Setenv` and `t.Parallel` are
// mutually exclusive by construction, and a parallel test here would read another's environment.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// required sets the eight variables that have no default, so a test can assert on the ones that do.
//
// It clears the ambient ORGANIZATION_ environment first, and that is not defensive tidiness. A
// developer runs this through `make`, which loads .env, so every one of these variables is already
// present in the shell -- and a variable this helper forgets is quietly supplied by the environment
// locally and absent in CI. ORGANIZATION_RESOLUTION_DATABASE_URL was added to Load and not to this
// list: the suite was green on this machine and red on the build, which is the worst direction for
// that asymmetry to run, because the local green is the one a change is judged by.
func required(t *testing.T) {
	t.Helper()

	for _, entry := range os.Environ() {
		if name, _, ok := strings.Cut(entry, "="); ok && strings.HasPrefix(name, "ORGANIZATION_") {
			t.Setenv(name, "")
		}
	}

	t.Setenv("ORGANIZATION_TENANT_DATABASE_URL", "postgres://organization_rt@localhost/control")
	t.Setenv("ORGANIZATION_PROVIDER_DATABASE_URL", "postgres://organization_provider_rt@localhost/control")
	t.Setenv("ORGANIZATION_RESOLUTION_DATABASE_URL", "postgres://organization_resolution_rt@localhost/control")
	t.Setenv("ORGANIZATION_TOKEN_ISSUER", "https://issuer.example")
	t.Setenv("ORGANIZATION_TOKEN_AUDIENCE", "organization-control-api")
	t.Setenv("ORGANIZATION_JWKS_URL", "https://issuer.example/jwks")
}

// TestTheProvisioningBoundsCarryTheirDocumentedDefaults pins the three rows of
// TDD-organization-control-003 §Configuration.
//
// Defaulted rather than required, unlike the credentials and the token terms: each has a value the
// design states, none decides who may call this service, and a process refusing to start without a
// sweep cadence would be harder to operate for no gain in safety.
func TestTheProvisioningBoundsCarryTheirDocumentedDefaults(t *testing.T) {
	required(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProvisioningTimeout != 30*time.Minute {
		t.Errorf("the provisioning timeout defaulted to %s, want 30m", cfg.ProvisioningTimeout)
	}
	if cfg.ProvisioningReconcileInterval != 15*time.Minute {
		t.Errorf("the reconcile interval defaulted to %s, want 15m", cfg.ProvisioningReconcileInterval)
	}
	if cfg.InvitationSweepInterval != time.Hour {
		t.Errorf("the invitation sweep interval defaulted to %s, want 1h (TDD-organization-control-004 §Configuration)",
			cfg.InvitationSweepInterval)
	}
	if cfg.TenantNameMax != 120 {
		t.Errorf("the Tenant name bound defaulted to %d, want 120", cfg.TenantNameMax)
	}
}

// TestAReconcileIntervalLongerThanTheTimeoutIsRefused is the one relationship between the two.
//
// A sweep slower than the timeout leaves a request sitting `requested` well past the age at which its
// outcome is meant to be declared unknown — which turns "ambiguous after thirty minutes" into a
// statement about nothing. It is the misconfiguration that produces no error anywhere else, which is
// the only kind worth checking here.
func TestAReconcileIntervalLongerThanTheTimeoutIsRefused(t *testing.T) {
	required(t)
	t.Setenv("ORGANIZATION_PROVISIONING_TIMEOUT", "10m")
	t.Setenv("ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL", "1h")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a reconcile interval six times the timeout")
	}
	if !strings.Contains(err.Error(), "ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL") {
		t.Errorf("the refusal did not name the variable an operator has to change:\n%v", err)
	}
}

// TestAPresentButUnparseableProvisioningValueIsAnError keeps a forgotten unit from being ignored.
//
// `ORGANIZATION_PROVISIONING_TIMEOUT=30` — minutes intended, unit forgotten — must not fall back
// silently to the default, because the operator would have no way to tell it had been ignored.
func TestAPresentButUnparseableProvisioningValueIsAnError(t *testing.T) {
	required(t)
	t.Setenv("ORGANIZATION_PROVISIONING_TIMEOUT", "30")

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a timeout with no unit")
	}
}

// TestAResolutionCredentialSharedWithAnotherPoolIsRefused guards the one misconfiguration that
// would leave every test in the estate passing.
//
// The provider role replays an abandoned delivery and holds no UPDATE on platform.dead_letter; the
// resolution role closes the incident against the evidence that replay produced. Pointed at the
// same DSN, one credential does both, the separation that makes the evidence meaningful is gone --
// and nothing fails, because the resolver simply runs as the wider role.
func TestAResolutionCredentialSharedWithAnotherPoolIsRefused(t *testing.T) {
	for _, shared := range []string{
		"ORGANIZATION_PROVIDER_DATABASE_URL", "ORGANIZATION_TENANT_DATABASE_URL",
	} {
		t.Run(shared, func(t *testing.T) {
			required(t)
			t.Setenv("ORGANIZATION_RESOLUTION_DATABASE_URL", os.Getenv(shared))

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted a resolution DSN identical to %s", shared)
			}
			if !strings.Contains(err.Error(), "ORGANIZATION_RESOLUTION_DATABASE_URL") {
				t.Errorf("the refusal did not name the variable an operator has to change:\n%v", err)
			}
		})
	}
}

// The consumer's credential is what enables consumer authority, and is never another pool's. Falling
// back to the provider credential is the over-privilege the consumer role exists to remove.
func TestTheConsumerCredentialIsItsOwn(t *testing.T) {
	t.Run("accepted with its own credential", func(t *testing.T) {
		required(t)
		t.Setenv("ORGANIZATION_CONSUMER_DATABASE_URL", "postgres://organization_consumer_app@localhost/control")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ConsumerDSN == "" {
			t.Error("the consumer credential was not kept")
		}
	})
	t.Run("absent, consumer authority is off", func(t *testing.T) {
		required(t)
		cfg, err := Load()
		if err != nil || cfg.ConsumerDSN != "" {
			t.Fatalf("Load answered %v with consumer DSN %q", err, cfg.ConsumerDSN)
		}
	})
	for _, shared := range []string{
		"ORGANIZATION_PROVIDER_DATABASE_URL", "ORGANIZATION_TENANT_DATABASE_URL", "ORGANIZATION_RESOLUTION_DATABASE_URL",
	} {
		t.Run("refused when shared with "+shared, func(t *testing.T) {
			required(t)
			t.Setenv("ORGANIZATION_CONSUMER_DATABASE_URL", os.Getenv(shared))
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ORGANIZATION_CONSUMER_DATABASE_URL") {
				t.Fatalf("Load accepted a consumer DSN identical to %s: %v", shared, err)
			}
		})
	}
}

// A setting that named a claim or a role is refused, so a deployment expecting authority from one
// learns at startup that it no longer comes from there (TDD-organization-control-001 §Caller
// Authority).
func TestARemovedAuthoritySettingIsRefused(t *testing.T) {
	for name := range RemovedSettings {
		t.Run(name, func(t *testing.T) {
			required(t)
			t.Setenv(name, "anything")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Load accepted %s: %v", name, err)
			}
		})
	}
}

// TestEveryProblemIsReportedAtOnce is why `Load` collects rather than returning the first error.
//
// An operator fixing a deployment wants the whole list; returning them one per restart turns a
// five-minute correction into five deploys.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	required(t)
	t.Setenv("ORGANIZATION_TOKEN_ISSUER", "")
	t.Setenv("ORGANIZATION_JWKS_URL", "")
	t.Setenv("ORGANIZATION_TENANT_NAME_MAX", "nonsense")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted three problems at once")
	}
	for _, want := range []string{
		"ORGANIZATION_TOKEN_ISSUER", "ORGANIZATION_JWKS_URL", "ORGANIZATION_TENANT_NAME_MAX",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report omitted %s:\n%v", want, err)
		}
	}
}

func TestTheTokenTypeIsReportedUnlessEnforcementIsAsked(t *testing.T) {
	for _, c := range []struct {
		value   string
		enforce bool
		ok      bool
	}{{"", false, true}, {"report", false, true}, {"enforce", true, true}, {"strict", false, false}} {
		required(t)
		t.Setenv("ORGANIZATION_TOKEN_TYPE", c.value)
		cfg, err := Load()
		if c.ok && (err != nil || cfg.EnforceAccessTokenType != c.enforce) {
			t.Errorf("%q: enforce %v, err %v", c.value, cfg.EnforceAccessTokenType, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%q was accepted", c.value)
		}
	}
}

// Provider activation settings (ADR-ORG-002 §5.1, TDD-organization-control-001 §Configuration).
func TestProviderActivationSettings(t *testing.T) {
	required(t)
	cfg, err := Load()
	if err != nil || !cfg.Production || cfg.ProviderActivationMax != 8*time.Hour || !cfg.ProviderActivationApproval {
		t.Fatalf("the defaults are production %v, max %s, approval %v, err %v",
			cfg.Production, cfg.ProviderActivationMax, cfg.ProviderActivationApproval, err)
	}

	for _, c := range []struct {
		environment, approval, max string
		ok                         bool
	}{
		{"non-production", "optional", "", true},
		{"production", "optional", "", false},
		{"", "", "25h", false},
		{"staging", "", "", false},
		{"", "maybe", "", false},
		{"", "", "1h", true},
	} {
		required(t)
		t.Setenv("ORGANIZATION_ENVIRONMENT", c.environment)
		t.Setenv("ORGANIZATION_PROVIDER_ACTIVATION_APPROVAL", c.approval)
		t.Setenv("ORGANIZATION_PROVIDER_ACTIVATION_MAX", c.max)
		_, err := Load()
		if (err == nil) != c.ok {
			t.Errorf("%+v: err %v", c, err)
		}
	}
}

// No target, no dispatcher, and nothing a dispatcher needs is required.
func TestADeploymentWithoutDeliveryTargetsNeedsNoDispatchCredential(t *testing.T) {
	required(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Delivery.Targets) != 0 || cfg.Delivery.Timeout != 5*time.Second || cfg.Delivery.RetryInterval != time.Minute {
		t.Errorf("delivery reads as %+v", cfg.Delivery)
	}
}

// The intervals the dispatchers are built with default inside TDD-002's 1 s commit-to-claim budget,
// rather than to foundation-platform's 5 s idle backoff, and a value inside it is read as given.
func TestTheDeliveryIntervalsDefaultInsideTheBudget(t *testing.T) {
	required(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Delivery.Interval != 500*time.Millisecond || cfg.Delivery.IdleInterval != time.Second {
		t.Errorf("the intervals defaulted to %s and %s, want 500ms and 1s", cfg.Delivery.Interval, cfg.Delivery.IdleInterval)
	}

	t.Setenv("ORGANIZATION_DELIVERY_INTERVAL", "200ms")
	t.Setenv("ORGANIZATION_DELIVERY_IDLE_INTERVAL", "300ms")
	if cfg, err = Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Delivery.Interval != 200*time.Millisecond || cfg.Delivery.IdleInterval != 300*time.Millisecond {
		t.Errorf("the intervals read as %s and %s, want 200ms and 300ms", cfg.Delivery.Interval, cfg.Delivery.IdleInterval)
	}
}

// An idle interval past the budget is foundation-platform's 5 s default restored by hand, and one
// below the poll period is a ceiling under the floor. Each is refused, naming the variable.
func TestADeliveryIntervalOutsideTheBudgetIsRefused(t *testing.T) {
	for _, c := range []struct{ interval, idle, names string }{
		{"", "5s", "ORGANIZATION_DELIVERY_IDLE_INTERVAL"},
		{"", "1001ms", "ORGANIZATION_DELIVERY_IDLE_INTERVAL"},
		{"800ms", "400ms", "ORGANIZATION_DELIVERY_INTERVAL"},
		{"0s", "", "ORGANIZATION_DELIVERY_INTERVAL"},
		{"", "-1s", "ORGANIZATION_DELIVERY_IDLE_INTERVAL"},
	} {
		required(t)
		t.Setenv("ORGANIZATION_DELIVERY_INTERVAL", c.interval)
		t.Setenv("ORGANIZATION_DELIVERY_IDLE_INTERVAL", c.idle)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.names) {
			t.Errorf("interval %q, idle %q answered %v", c.interval, c.idle, err)
		}
	}
}

// A named target needs the dispatch credential and the workload client, and each problem is named.
func TestADeliveryTargetRequiresTheDispatchCredentialAndTheWorkloadClient(t *testing.T) {
	required(t)
	t.Setenv("ORGANIZATION_DELIVERY_TARGETS", "identity-control=https://identity-control.internal/v1/deliveries")
	_, err := Load()
	for _, name := range []string{"ORGANIZATION_DISPATCH_DATABASE_URL", "ORGANIZATION_WORKLOAD_CLIENT_ID",
		"ORGANIZATION_WORKLOAD_KEY_FILE", "ORGANIZATION_WORKLOAD_TOKEN_URL"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("Load does not name %s as required: %v", name, err)
		}
	}

	t.Setenv("ORGANIZATION_DISPATCH_DATABASE_URL", "postgres://organization_dispatch_app@localhost/control")
	t.Setenv("ORGANIZATION_WORKLOAD_CLIENT_ID", "organization-control-workload")
	t.Setenv("ORGANIZATION_WORKLOAD_KEY_FILE", "/run/secrets/workload.pem")
	t.Setenv("ORGANIZATION_WORKLOAD_TOKEN_URL", "http://kernel:8080/realms/scnehaux/protocol/openid-connect/token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Delivery.Targets) != 1 || cfg.Delivery.Targets[0].Consumer != "identity-control" ||
		cfg.Delivery.Targets[0].Endpoint != "https://identity-control.internal/v1/deliveries" {
		t.Errorf("targets read as %+v", cfg.Delivery.Targets)
	}
}

func TestAMalformedDeliveryTargetIsRefused(t *testing.T) {
	for _, raw := range []string{
		"identity-control",
		"=https://x",
		"identity-control=/relative",
		"identity-control=ftp://x",
		"a=https://x,a=https://y",
	} {
		required(t)
		t.Setenv("ORGANIZATION_DELIVERY_TARGETS", raw)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ORGANIZATION_DELIVERY_TARGETS") {
			t.Errorf("%q answered %v", raw, err)
		}
	}
}

// The dispatchers deliver with the dispatch role's privileges and no other pool's.
func TestADispatchCredentialSharedWithAnotherPoolIsRefused(t *testing.T) {
	required(t)
	t.Setenv("ORGANIZATION_DISPATCH_DATABASE_URL", "postgres://organization_provider_rt@localhost/control")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ORGANIZATION_DISPATCH_DATABASE_URL") {
		t.Errorf("a dispatch DSN equal to the provider DSN answered %v", err)
	}
}

// TestASubMinuteSweepIntervalIsRefused keeps a forgotten unit from running a sweep in a loop.
//
// `1ms` meant as `1m` is a valid duration, so only a floor catches it, and the refusal names the
// variable to change.
func TestASubMinuteSweepIntervalIsRefused(t *testing.T) {
	for _, name := range []string{"ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL", "ORGANIZATION_INVITATION_SWEEP_INTERVAL"} {
		t.Run(name, func(t *testing.T) {
			required(t)
			t.Setenv(name, "1ms")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=1ms", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal did not name %s:\n%v", name, err)
			}
		})
	}
}

// The address match is optional; set, it needs the dispatchers' workload client, its URL is an
// absolute http(s) URL, and its timeout defaults to 5s (TDD-organization-control-004 1.14.0
// §Configuration).
func TestTheIdentityControlURLRequiresTheWorkloadClient(t *testing.T) {
	required(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IdentityControl.URL != "" || cfg.IdentityControl.Timeout != 5*time.Second {
		t.Errorf("unset, IdentityControl = %+v, want no URL and a 5s timeout", cfg.IdentityControl)
	}

	t.Setenv("ORGANIZATION_IDENTITY_CONTROL_URL", "http://identity-control:8090")
	_, err = Load()
	for _, name := range []string{"ORGANIZATION_WORKLOAD_CLIENT_ID", "ORGANIZATION_WORKLOAD_KEY_FILE",
		"ORGANIZATION_WORKLOAD_TOKEN_URL"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("Load does not name %s as required: %v", name, err)
		}
	}
	if err != nil && strings.Contains(err.Error(), "ORGANIZATION_DISPATCH_DATABASE_URL") {
		t.Errorf("the match needs no dispatch credential: %v", err)
	}

	t.Setenv("ORGANIZATION_WORKLOAD_CLIENT_ID", "organization-control-workload")
	t.Setenv("ORGANIZATION_WORKLOAD_KEY_FILE", "/run/secrets/workload.pem")
	t.Setenv("ORGANIZATION_WORKLOAD_TOKEN_URL", "http://kernel:8080/realms/scnehaux/protocol/openid-connect/token")
	t.Setenv("ORGANIZATION_IDENTITY_CONTROL_TIMEOUT", "2s")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IdentityControl.URL != "http://identity-control:8090" || cfg.IdentityControl.Timeout != 2*time.Second {
		t.Errorf("IdentityControl = %+v", cfg.IdentityControl)
	}

	for _, raw := range []string{"identity-control:8090", "ftp://identity-control", "http://identity-control?x=1", "/v1"} {
		t.Setenv("ORGANIZATION_IDENTITY_CONTROL_URL", raw)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ORGANIZATION_IDENTITY_CONTROL_URL") {
			t.Errorf("%q answered %v", raw, err)
		}
	}
}
