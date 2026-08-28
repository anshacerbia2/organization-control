// Package config reads process configuration from the environment and nowhere else.
//
// Twelve-factor, per STD-GLB-009: no file, no flag for a value that differs between environments,
// and no default that would let a misconfigured process start and fail later. A required variable
// that is absent is a startup error, because a service that boots without its database URL and
// reports healthy is worse than one that never boots.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the whole configuration surface of the serving deployable.
type Config struct {
	// Deployable and System label every span, metric, and log line so load and failure are
	// attributable while several systems run the same foundation-platform code.
	Deployable string
	System     string

	ListenAddress string

	// TenantDSN connects as `organization_rt` and ProviderDSN as `organization_provider_rt`.
	//
	// Two variables, not one with a role parameter, because they are two credentials for two
	// PostgreSQL login roles with different policies — and the whole isolation posture rests on
	// ordinary tenant traffic being unable to authenticate as the cross-Tenant role. One DSN
	// reused for both would compile, pass every test that does not inspect `current_user`, and
	// silently run the estate's tenant traffic under the role that can read every Tenant.
	//
	// Both are required. Defaulting the provider DSN to the tenant one is exactly the collapse
	// above, and defaulting it to empty would produce a process whose provider routes fail at
	// request time rather than at startup.
	TenantDSN   string
	ProviderDSN string

	DBMaxConns        int32
	DBMaxConnLifetime time.Duration
	DBAcquireTimeout  time.Duration

	HTTPReadTimeout    time.Duration
	HTTPWriteTimeout   time.Duration
	HTTPRequestTimeout time.Duration
	HTTPMaxInFlight    int64
	HTTPShutdownGrace  time.Duration
	ReadinessTimeout   time.Duration

	// TokenIssuer and TokenAudience are the verifier's contract. The issuer is compared for exact
	// equality, so a value with a stray trailing slash rejects every token rather than accepting a
	// wrong one.
	//
	// JWKSURL is configuration and never read from a token: a token naming its own key source
	// would choose the key that validates it.
	TokenIssuer   string
	TokenAudience string
	JWKSURL       string

	// TokenMaxSkew tolerates clock drift, capped at 60 seconds by STD-IAM-002 §3.5.
	TokenMaxSkew time.Duration

	// TenantClaim is the claim naming the Tenant a caller administers, and ProviderRole is the
	// realm role that confers cross-Tenant authority.
	//
	// Both are configuration rather than constants because the claim namespace is a property of the
	// realm this deployable is pointed at, and hard-coding it would make the same binary unusable
	// against a second realm. Neither is defaulted: a default claim name that no token carries
	// would leave every caller with no Tenant, and a default provider role name that no token
	// carries would leave provider authority unreachable — both fail closed, and both fail in a way
	// an operator would debug as a permissions bug rather than as a missing variable.
	TenantClaim  string
	ProviderRole string

	// The three provisioning bounds of TDD-organization-control-003 §Configuration.
	//
	// ProvisioningTimeout is the age at which a request with no realized status becomes
	// `unresolved` — an ambiguous outcome, never inferred as success. ProvisioningReconcileInterval
	// is the cadence at which the sweep runs. TenantNameMax bounds the Tenant display name.
	//
	// All three are defaulted, unlike the credentials and the token terms above. Each has a value
	// the design states, none of them decides who may call this service, and a process that refused
	// to start without a sweep cadence would be harder to operate for no gain in safety.
	ProvisioningTimeout           time.Duration
	ProvisioningReconcileInterval time.Duration
	TenantNameMax                 int

	LogLevel string
}

// Load reads the environment and reports every problem at once.
//
// Collecting errors rather than returning the first is deliberate: an operator fixing a deployment
// wants the whole list, and returning them one per restart turns a five-minute correction into five
// deploys.
func Load() (Config, error) {
	var problems []error

	cfg := Config{
		Deployable: "organization-control",
		System:     "SAD-004",
	}

	required := map[string]*string{
		"ORGANIZATION_TENANT_DATABASE_URL":   &cfg.TenantDSN,
		"ORGANIZATION_PROVIDER_DATABASE_URL": &cfg.ProviderDSN,

		// Each of these is a term in an authentication decision. A default would be a default
		// answer to "who may call this service", which is not a question a fallback value gets to
		// answer.
		"ORGANIZATION_TOKEN_ISSUER":   &cfg.TokenIssuer,
		"ORGANIZATION_TOKEN_AUDIENCE": &cfg.TokenAudience,
		"ORGANIZATION_JWKS_URL":       &cfg.JWKSURL,
		"ORGANIZATION_TENANT_CLAIM":   &cfg.TenantClaim,
		"ORGANIZATION_PROVIDER_ROLE":  &cfg.ProviderRole,
	}
	for name, target := range required {
		*target = strings.TrimSpace(os.Getenv(name))
		if *target == "" {
			problems = append(problems, fmt.Errorf("%s is required", name))
		}
	}

	// The one relationship between two variables worth checking at startup.
	//
	// Identical DSNs mean both pools authenticate as the same role, which makes the two pool types
	// a compile-time distinction with no runtime difference behind it — the tenant policy and the
	// provider policy would both be evaluated for whichever role was supplied. Caught here because
	// it is the misconfiguration that produces no error anywhere else.
	if cfg.TenantDSN != "" && cfg.TenantDSN == cfg.ProviderDSN {
		problems = append(problems, errors.New(
			"ORGANIZATION_TENANT_DATABASE_URL and ORGANIZATION_PROVIDER_DATABASE_URL are identical, "+
				"so both pools would authenticate as one role and the isolation boundary between "+
				"them would exist only in the Go type system"))
	}

	cfg.ListenAddress = stringOr("ORGANIZATION_LISTEN_ADDRESS", ":8080")
	cfg.LogLevel = stringOr("LOG_LEVEL", "info")

	cfg.TokenMaxSkew = durationOr("ORGANIZATION_TOKEN_MAX_SKEW", 30*time.Second, &problems)
	if cfg.TokenMaxSkew > 60*time.Second {
		problems = append(problems, errors.New(
			"ORGANIZATION_TOKEN_MAX_SKEW exceeds the 60s ceiling STD-IAM-002 §3.5 sets"))
	}

	cfg.DBMaxConns = int32(intOr("DB_MAX_CONNS", 20, &problems))
	cfg.DBMaxConnLifetime = durationOr("DB_MAX_CONN_LIFETIME", 30*time.Minute, &problems)
	cfg.DBAcquireTimeout = durationOr("DB_ACQUIRE_TIMEOUT", 3*time.Second, &problems)

	cfg.HTTPReadTimeout = durationOr("HTTP_READ_TIMEOUT", 10*time.Second, &problems)
	cfg.HTTPWriteTimeout = durationOr("HTTP_WRITE_TIMEOUT", 30*time.Second, &problems)
	cfg.HTTPRequestTimeout = durationOr("HTTP_REQUEST_TIMEOUT", 5*time.Second, &problems)
	cfg.HTTPMaxInFlight = int64(intOr("HTTP_MAX_IN_FLIGHT", 256, &problems))
	cfg.HTTPShutdownGrace = durationOr("HTTP_SHUTDOWN_GRACE", 20*time.Second, &problems)
	cfg.ReadinessTimeout = durationOr("ORGANIZATION_READINESS_TIMEOUT", 2*time.Second, &problems)

	cfg.ProvisioningTimeout = durationOr("ORGANIZATION_PROVISIONING_TIMEOUT", 30*time.Minute, &problems)
	cfg.ProvisioningReconcileInterval = durationOr(
		"ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL", 15*time.Minute, &problems)
	cfg.TenantNameMax = intOr("ORGANIZATION_TENANT_NAME_MAX", 120, &problems)

	// A sweep that runs less often than the timeout is the normal configuration; one that runs more
	// often than the timeout is wasteful but harmless. The relationship worth refusing is neither:
	// it is a reconcile interval so long that a request sits unanswered for multiples of the timeout
	// before anybody looks, which turns "ambiguous after 30 minutes" into a statement about nothing.
	if cfg.ProvisioningReconcileInterval > cfg.ProvisioningTimeout {
		problems = append(problems, fmt.Errorf(
			"ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL (%s) is longer than "+
				"ORGANIZATION_PROVISIONING_TIMEOUT (%s), so a request would stay `requested` well past "+
				"the age at which its outcome is meant to be declared unknown",
			cfg.ProvisioningReconcileInterval, cfg.ProvisioningTimeout))
	}

	if len(problems) > 0 {
		return Config{}, errors.Join(problems...)
	}
	return cfg, nil
}

func stringOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// durationOr parses an optional duration.
//
// A present but unparseable value is an error rather than the fallback. Falling back silently would
// let `HTTP_REQUEST_TIMEOUT=5` — seconds intended, unit forgotten — start a process with a timeout
// nobody chose, and the operator would have no way to tell it had been ignored.
func durationOr(key string, fallback time.Duration, problems *[]error) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		*problems = append(*problems, fmt.Errorf("%s is not a duration: %q", key, raw))
		return fallback
	}
	if value <= 0 {
		*problems = append(*problems, fmt.Errorf("%s must be positive: %q", key, raw))
		return fallback
	}
	return value
}

func intOr(key string, fallback int, problems *[]error) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		*problems = append(*problems, fmt.Errorf("%s is not a number: %q", key, raw))
		return fallback
	}
	if value <= 0 {
		*problems = append(*problems, fmt.Errorf("%s must be positive: %q", key, raw))
		return fallback
	}
	return value
}
