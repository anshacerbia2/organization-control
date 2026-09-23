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
	t.Setenv("ORGANIZATION_TOKEN_AUDIENCE", "organization-control")
	t.Setenv("ORGANIZATION_JWKS_URL", "https://issuer.example/jwks")
	t.Setenv("ORGANIZATION_TENANT_CLAIM", "https://scnehaux.com/tenant")
	t.Setenv("ORGANIZATION_PROVIDER_ROLE", "provider-admin")
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
