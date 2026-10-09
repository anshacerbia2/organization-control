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
	"net/url"
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
	TenantDSN     string
	ProviderDSN   string
	ResolutionDSN string

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

	// EnforceAccessTokenType is ORGANIZATION_TOKEN_TYPE=enforce: a token whose header typ is not at+jwt
	// is refused (STD-IAM-002 §3.5 step 5). The default, report, accepts it and logs it, while the
	// issuer's clients move to at+jwt.
	EnforceAccessTokenType bool

	// ConsumerDSN is the consumer's own credential, a login role inheriting organization_consumer_rt.
	// Optional: set, it is what enables consumer authority, because only then is there a pool to
	// serve a consumer. Never another pool's: a consumer on the provider credential could read and
	// write every table the control plane can.
	ConsumerDSN string

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

	// InvitationSweepInterval is the cadence of the scheduled invitation expiry
	// (TDD-organization-control-004 §Expiry Sweep). It and ProvisioningReconcileInterval are the
	// two schedules this process runs (TDD-organization-control-003 §Scheduled Sweeps).
	InvitationSweepInterval time.Duration

	LogLevel string

	// Production is ORGANIZATION_ENVIRONMENT=production, the default.
	Production bool

	// ProviderActivationMax is ORGANIZATION_PROVIDER_ACTIVATION_MAX, the longest provider
	// activation a request may ask for (ADR-ORG-002 §5.1).
	ProviderActivationMax time.Duration

	// ProviderActivationApproval is ORGANIZATION_PROVIDER_ACTIVATION_APPROVAL=required, the
	// default: an activation waits for another holder's approval. optional is refused in production.
	ProviderActivationApproval bool

	// Delivery is the consumers this service runs a dispatcher for, and what those dispatchers
	// need (ADR-GLB-018 §5.4, TDD-organization-control-005 §Technical Context). Empty Targets runs
	// none.
	Delivery Delivery

	// OTLPEndpoint is the OpenTelemetry Collector's OTLP/HTTP base URL. A deployment sets it; unset,
	// the process exports nothing and says so at startup, and the absent-telemetry alert fires
	// (TDD-foundation-platform-002 §Configuration).
	OTLPEndpoint string
}

// DeliveryTarget is one consumer's acceptance API.
type DeliveryTarget struct {
	Consumer string
	Endpoint string
}

// Delivery configures the in-process dispatchers.
type Delivery struct {
	Targets []DeliveryTarget

	// DispatchDSN connects as a login role inheriting organization_dispatch_rt.
	DispatchDSN string

	// The workload client the dispatchers authenticate as, with private_key_jwt.
	WorkloadClientID string
	WorkloadKeyFile  string
	WorkloadTokenURL string

	Timeout       time.Duration
	RetryInterval time.Duration
}

// parseTargets reads consumer=url pairs separated by commas.
func parseTargets(raw string) ([]DeliveryTarget, error) {
	var targets []DeliveryTarget
	seen := map[string]bool{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		consumer, endpoint, ok := strings.Cut(pair, "=")
		consumer, endpoint = strings.TrimSpace(consumer), strings.TrimSpace(endpoint)
		parsed, err := url.Parse(endpoint)
		switch {
		case !ok || consumer == "" || endpoint == "":
			return nil, fmt.Errorf("ORGANIZATION_DELIVERY_TARGETS entry %q is not consumer=url", pair)
		case err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http"):
			return nil, fmt.Errorf("ORGANIZATION_DELIVERY_TARGETS entry for %s is not an absolute http(s) URL", consumer)
		case seen[consumer]:
			return nil, fmt.Errorf("ORGANIZATION_DELIVERY_TARGETS names %s twice; a consumer has one acceptance API", consumer)
		}
		seen[consumer] = true
		targets = append(targets, DeliveryTarget{Consumer: consumer, Endpoint: endpoint})
	}
	return targets, nil
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

		// The resolver's own credential. Required rather than defaulted to the provider one:
		// a deployment that fell back would put replay and closure behind one secret, which is
		// the separation the fourth role exists to create, and nothing would report it.
		"ORGANIZATION_RESOLUTION_DATABASE_URL": &cfg.ResolutionDSN,

		// Each of these is a term in an authentication decision. A default would be a default
		// answer to "who may call this service", which is not a question a fallback value gets to
		// answer.
		"ORGANIZATION_TOKEN_ISSUER":   &cfg.TokenIssuer,
		"ORGANIZATION_TOKEN_AUDIENCE": &cfg.TokenAudience,
		"ORGANIZATION_JWKS_URL":       &cfg.JWKSURL,
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

	// The same check for the resolution credential, and it guards a sharper property.
	//
	// The provider role replays an abandoned delivery and holds no UPDATE on platform.dead_letter;
	// the resolution role closes the incident. Pointed at the same DSN, one process would do both
	// under one credential, and the database separation that makes the evidence between them
	// meaningful would be gone — while every test still passed, because they would pass as the
	// wider role.
	if cfg.ResolutionDSN != "" &&
		(cfg.ResolutionDSN == cfg.ProviderDSN || cfg.ResolutionDSN == cfg.TenantDSN) {
		problems = append(problems, errors.New(
			"ORGANIZATION_RESOLUTION_DATABASE_URL matches another pool's DSN, so replaying an "+
				"abandoned delivery and closing the incident it left would be performed by one "+
				"credential — and the evidence the resolver reads would be evidence the same "+
				"process could have produced"))
	}

	cfg.ConsumerDSN = strings.TrimSpace(os.Getenv("ORGANIZATION_CONSUMER_DATABASE_URL"))
	if cfg.ConsumerDSN != "" && (cfg.ConsumerDSN == cfg.ProviderDSN ||
		cfg.ConsumerDSN == cfg.TenantDSN || cfg.ConsumerDSN == cfg.ResolutionDSN) {
		problems = append(problems, errors.New(
			"ORGANIZATION_CONSUMER_DATABASE_URL matches another pool's DSN, so a consumer would run "+
				"with that pool's privileges rather than its own"))
	}

	// Authority no longer comes from these (ADR-ORG-001 §5.11, TDD-organization-control-001 §Caller
	// Authority). Refused rather than ignored: a deployment still setting one expects provider or
	// consumer authority to come from a role or a claim name, and ignoring it would leave that
	// deployment believing so while every provider is refused for a reason it cannot see.
	for name, now := range RemovedSettings {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			problems = append(problems, fmt.Errorf("%s is no longer read: %s", name, now))
		}
	}

	cfg.ListenAddress = stringOr("ORGANIZATION_LISTEN_ADDRESS", ":8080")
	cfg.LogLevel = stringOr("LOG_LEVEL", "info")
	cfg.OTLPEndpoint = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))

	cfg.TokenMaxSkew = durationOr("ORGANIZATION_TOKEN_MAX_SKEW", 30*time.Second, &problems)
	switch mode := stringOr("ORGANIZATION_TOKEN_TYPE", "report"); mode {
	case "report":
	case "enforce":
		cfg.EnforceAccessTokenType = true
	default:
		problems = append(problems, fmt.Errorf("ORGANIZATION_TOKEN_TYPE is %q; it is report or enforce", mode))
	}
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
	cfg.InvitationSweepInterval = durationOr("ORGANIZATION_INVITATION_SWEEP_INTERVAL", time.Hour, &problems)

	// Each schedule is a transaction per replica per interval. A sub-minute cadence buys nothing --
	// the timeout and an invitation's lifetime are measured in minutes and days -- and a unit
	// forgotten in the other direction, `1ms` for `1m`, would hold the database in a loop.
	for _, schedule := range []struct {
		name     string
		interval time.Duration
	}{
		{"ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL", cfg.ProvisioningReconcileInterval},
		{"ORGANIZATION_INVITATION_SWEEP_INTERVAL", cfg.InvitationSweepInterval},
	} {
		if schedule.interval < MinimumSweepInterval {
			problems = append(problems, fmt.Errorf("%s (%s) is shorter than %s, the shortest schedule a sweep runs on",
				schedule.name, schedule.interval, MinimumSweepInterval))
		}
	}

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

	switch environment := stringOr("ORGANIZATION_ENVIRONMENT", "production"); environment {
	case "production":
		cfg.Production = true
	case "non-production":
	default:
		problems = append(problems, fmt.Errorf("ORGANIZATION_ENVIRONMENT is %q; it is production or non-production", environment))
	}
	cfg.ProviderActivationMax = durationOr("ORGANIZATION_PROVIDER_ACTIVATION_MAX", 8*time.Hour, &problems)
	if cfg.ProviderActivationMax > 24*time.Hour {
		problems = append(problems, errors.New(
			"ORGANIZATION_PROVIDER_ACTIVATION_MAX exceeds 24h, the longest activation Entra PIM allows (ADR-ORG-002 §5.1)"))
	}
	switch approval := stringOr("ORGANIZATION_PROVIDER_ACTIVATION_APPROVAL", "required"); approval {
	case "required":
		cfg.ProviderActivationApproval = true
	case "optional":
		// Outside production only: in production an activation is approved by another provider
		// (ADR-ORG-002 §5.1), and a setting that removed that would be the control turned off.
		if cfg.Production {
			problems = append(problems, errors.New(
				"ORGANIZATION_PROVIDER_ACTIVATION_APPROVAL=optional is refused in production"))
		}
	default:
		problems = append(problems, fmt.Errorf("ORGANIZATION_PROVIDER_ACTIVATION_APPROVAL is %q; it is required or optional", approval))
	}

	// The in-process dispatchers (ADR-GLB-018 §5.4). Nothing is required while no target is named:
	// a deployment that delivers to no consumer needs no dispatch credential and no workload key.
	targets, err := parseTargets(os.Getenv("ORGANIZATION_DELIVERY_TARGETS"))
	if err != nil {
		problems = append(problems, err)
	}
	cfg.Delivery = Delivery{
		Targets:          targets,
		DispatchDSN:      strings.TrimSpace(os.Getenv("ORGANIZATION_DISPATCH_DATABASE_URL")),
		WorkloadClientID: strings.TrimSpace(os.Getenv("ORGANIZATION_WORKLOAD_CLIENT_ID")),
		WorkloadKeyFile:  strings.TrimSpace(os.Getenv("ORGANIZATION_WORKLOAD_KEY_FILE")),
		WorkloadTokenURL: strings.TrimSpace(os.Getenv("ORGANIZATION_WORKLOAD_TOKEN_URL")),
		Timeout:          durationOr("ORGANIZATION_DELIVERY_TIMEOUT", 5*time.Second, &problems),
		RetryInterval:    durationOr("ORGANIZATION_DELIVERY_RETRY_INTERVAL", time.Minute, &problems),
	}
	if len(targets) > 0 {
		for name, value := range map[string]string{
			"ORGANIZATION_DISPATCH_DATABASE_URL": cfg.Delivery.DispatchDSN,
			"ORGANIZATION_WORKLOAD_CLIENT_ID":    cfg.Delivery.WorkloadClientID,
			"ORGANIZATION_WORKLOAD_KEY_FILE":     cfg.Delivery.WorkloadKeyFile,
			"ORGANIZATION_WORKLOAD_TOKEN_URL":    cfg.Delivery.WorkloadTokenURL,
		} {
			if value == "" {
				problems = append(problems, fmt.Errorf("%s is required while ORGANIZATION_DELIVERY_TARGETS names a consumer", name))
			}
		}
	}
	// The dispatch credential is its own, for the reason the resolution one is: a dispatcher on
	// another pool's role would deliver with that role's privileges.
	if dsn := cfg.Delivery.DispatchDSN; dsn != "" &&
		(dsn == cfg.TenantDSN || dsn == cfg.ProviderDSN || dsn == cfg.ResolutionDSN || dsn == cfg.ConsumerDSN) {
		problems = append(problems, errors.New(
			"ORGANIZATION_DISPATCH_DATABASE_URL matches another pool's DSN, so the dispatchers would deliver "+
				"with that pool's privileges rather than the dispatch role's"))
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
// MinimumSweepInterval is the shortest cadence either scheduled sweep accepts.
const MinimumSweepInterval = time.Minute

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

// RemovedSettings are the variables that named a claim or a role, with what replaced each.
var RemovedSettings = map[string]string{
	"ORGANIZATION_TENANT_CLAIM": "the Tenant is the tenant_id claim STD-IAM-002 §3.2 names; unset it",
	"ORGANIZATION_PROVIDER_ROLE": "a provider is a Principal holding a provider grant this service records " +
		"(organization-control bootstrap-provider makes the first); unset it",
	"ORGANIZATION_CONSUMER_ROLE": "a consumer is a workload registered with its principal_id, and " +
		"ORGANIZATION_CONSUMER_DATABASE_URL enables consumer authority; unset it",
	"ORGANIZATION_CONSUMER_CLAIM": "the consumer is found by the token's principal_id; unset it",
}
