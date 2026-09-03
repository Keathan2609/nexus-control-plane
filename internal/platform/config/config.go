// Package config loads process configuration from the environment.
//
// Configuration is read once at startup into an immutable struct rather than looked up
// from os.Getenv at the point of use. That makes the full set of knobs discoverable in
// one place, lets Validate reject a broken configuration before any connection is
// opened, and keeps tests from depending on process-global state.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Environment string
	LogLevel    string
	LogFormat   string

	HTTPAddr    string
	MetricsAddr string

	DatabaseURL string
	NATSURL     string

	JWTSecret     string
	WebhookSecret string
	TokenTTL      time.Duration

	OTELEndpoint    string
	ServiceName     string
	ServiceVersion  string
	TraceSampleRate float64

	WorkerConcurrency  int
	WorkerLeaseSeconds int
	WorkerPollInterval time.Duration

	ReconcileInterval time.Duration
	OutboxBatchSize   int
	OutboxInterval    time.Duration

	// DeployProvider selects how deployments are made real: "docker" runs containers on
	// the local daemon, "kubernetes" applies manifests to a cluster. Both are real; they
	// differ in what they target, not in whether they actually deploy.
	DeployProvider string
	KubeContext    string
	KindCluster    string
	// ImageRepository is the local repository built images are tagged into.
	ImageRepository string

	// AutoMigrate applies pending migrations at startup. On by default because a local
	// stack should come up with one command; the Helm chart turns it off and runs
	// migrations as a pre-install job instead.
	AutoMigrate bool

	CORSOrigins []string

	// TrustProxyHeaders makes the API believe X-Forwarded-For when identifying a client
	// for rate limiting. It defaults to false because the header is client-controlled:
	// trusting it when nothing overwrites it hands every caller an unlimited supply of
	// fresh rate-limit buckets. Enable it only behind an ingress that rewrites the
	// header rather than appending to it.
	TrustProxyHeaders bool
}

func Load(serviceName string) (Config, error) {
	c := Config{
		Environment: env("NEXUS_ENVIRONMENT", "development"),
		LogLevel:    env("NEXUS_LOG_LEVEL", "info"),
		LogFormat:   env("NEXUS_LOG_FORMAT", "json"),

		HTTPAddr:    env("NEXUS_HTTP_ADDR", ":8080"),
		MetricsAddr: env("NEXUS_METRICS_ADDR", ":9090"),

		DatabaseURL: env("NEXUS_DATABASE_URL", "postgres://nexus:nexus@localhost:5436/nexus?sslmode=disable"),
		NATSURL:     env("NEXUS_NATS_URL", "nats://localhost:4222"),

		JWTSecret:     os.Getenv("NEXUS_AUTH_JWT_SECRET"),
		WebhookSecret: os.Getenv("NEXUS_WEBHOOK_SECRET"),
		TokenTTL:      envDuration("NEXUS_TOKEN_TTL", 12*time.Hour),

		OTELEndpoint:    os.Getenv("NEXUS_OTEL_ENDPOINT"),
		ServiceName:     serviceName,
		ServiceVersion:  env("NEXUS_VERSION", "dev"),
		TraceSampleRate: envFloat("NEXUS_TRACE_SAMPLE_RATE", 1.0),

		WorkerConcurrency:  envInt("NEXUS_WORKER_CONCURRENCY", 4),
		WorkerLeaseSeconds: envInt("NEXUS_WORKER_LEASE_SECONDS", 30),
		WorkerPollInterval: envDuration("NEXUS_WORKER_POLL_INTERVAL", 500*time.Millisecond),

		ReconcileInterval: envDuration("NEXUS_RECONCILE_INTERVAL", 10*time.Second),
		OutboxBatchSize:   envInt("NEXUS_OUTBOX_BATCH_SIZE", 128),
		OutboxInterval:    envDuration("NEXUS_OUTBOX_INTERVAL", time.Second),

		DeployProvider:  strings.ToLower(env("NEXUS_DEPLOY_PROVIDER", "docker")),
		KubeContext:     os.Getenv("NEXUS_KUBE_CONTEXT"),
		KindCluster:     env("NEXUS_KIND_CLUSTER", "nexus-local"),
		ImageRepository: env("NEXUS_IMAGE_REPOSITORY", "nexus-demo"),

		AutoMigrate:       envBool("NEXUS_AUTO_MIGRATE", true),
		CORSOrigins:       envList("NEXUS_CORS_ORIGINS", []string{"http://localhost:3100"}),
		TrustProxyHeaders: envBool("NEXUS_TRUST_PROXY_HEADERS", false),
	}

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) IsProduction() bool { return c.Environment == "production" }

// Validate refuses configurations that would fail later in a less obvious place.
//
// The secret rules are the interesting part: a development default for a signing secret
// is a convenience locally and a serious vulnerability in production, so the check is
// conditional on Environment rather than absolute.
func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("NEXUS_DATABASE_URL is required")
	}
	if c.WorkerConcurrency < 1 {
		return fmt.Errorf("NEXUS_WORKER_CONCURRENCY must be at least 1, got %d", c.WorkerConcurrency)
	}
	if c.WorkerLeaseSeconds < 5 {
		// A lease shorter than a few multiples of the heartbeat interval will be reaped
		// out from under a healthy worker.
		return fmt.Errorf("NEXUS_WORKER_LEASE_SECONDS must be at least 5, got %d", c.WorkerLeaseSeconds)
	}
	switch c.DeployProvider {
	case "docker", "kubernetes":
	default:
		return fmt.Errorf("NEXUS_DEPLOY_PROVIDER must be docker or kubernetes, got %q", c.DeployProvider)
	}
	if c.TraceSampleRate < 0 || c.TraceSampleRate > 1 {
		return fmt.Errorf("NEXUS_TRACE_SAMPLE_RATE must be between 0 and 1, got %v", c.TraceSampleRate)
	}
	if c.IsProduction() {
		if len(c.JWTSecret) < 32 {
			return fmt.Errorf("NEXUS_AUTH_JWT_SECRET must be at least 32 bytes in production")
		}
		if c.WebhookSecret == "" {
			return fmt.Errorf("NEXUS_WEBHOOK_SECRET is required in production")
		}
	}
	return nil
}

// JWTSecretOrDevDefault returns the configured signing secret, falling back to a fixed
// development value outside production. Validate has already guaranteed a real secret
// exists in production, so this cannot silently weaken a production deployment.
func (c Config) JWTSecretOrDevDefault() []byte {
	if c.JWTSecret != "" {
		return []byte(c.JWTSecret)
	}
	return []byte("nexus-development-signing-key-not-for-production")
}

func (c Config) WorkerLease() time.Duration {
	return time.Duration(c.WorkerLeaseSeconds) * time.Second
}

// HeartbeatInterval is deliberately a third of the lease: it gives a worker two chances
// to renew before its lease expires, so a single slow database round trip does not cost
// it a job it is actively running.
func (c Config) HeartbeatInterval() time.Duration {
	return c.WorkerLease() / 3
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envList(key string, fallback []string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
