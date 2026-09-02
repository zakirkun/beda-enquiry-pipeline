// Package config loads runtime configuration from the environment.
// Everything tunable lives here so thresholds are ops knobs, not literals
// buried in worker code (docs/04-ARCHITECTURE.md §8).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Supported providers. Adding one means adding a case in llm.newModel and a
// credential pair here — nothing in the worker or router changes.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

// Tier is one rung of the two-tier model setup: which provider serves it and
// which model. Neither tier is tied to a vendor — tier 2 can be OpenAI, both
// tiers can be the same provider, or they can be split across two.
type Tier struct {
	Provider string
	Model    string
}

func (t Tier) String() string { return t.Provider + "/" + t.Model }

// Credentials for one provider. Only the providers a tier actually names have
// to be configured, so running entirely on one vendor needs one key.
type Provider struct {
	Key     string
	BaseURL string
}

type Config struct {
	DatabaseURL string
	HTTPAddr    string

	// Webhook auth. Inbound endpoints are public-facing, so they require a
	// shared secret; boot fails if it is unset rather than exposing them open.
	WebhookSecret string
	// DashboardOrigin is the single allowed CORS origin for the Next.js app.
	DashboardOrigin string

	// Tier1 is the cheap first pass, Tier2 the escalation and drafting model.
	Tier1 Tier
	Tier2 Tier
	// Providers is keyed by provider name; only the ones a tier names are populated.
	Providers  map[string]Provider
	LLMTimeout time.Duration

	ConfidenceThreshold      float64
	AutoAttachMatchThreshold float64
	TrgmMatchFloor           float64

	DedupeWindow time.Duration
	WorkerCount  int
	MaxAttempts  int
	PollInterval time.Duration
	LockTTL      time.Duration

	RulesSeedFile string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:     env("DATABASE_URL", "postgres://beda:beda@localhost:5432/beda?sslmode=disable"),
		HTTPAddr:        env("HTTP_ADDR", ":8080"),
		WebhookSecret:   os.Getenv("WEBHOOK_SECRET"),
		DashboardOrigin: env("DASHBOARD_ORIGIN", "http://localhost:3000"),

		Tier1: Tier{
			Provider: strings.ToLower(env("TIER1_PROVIDER", ProviderOpenAI)),
			Model:    env("TIER1_MODEL", "gpt-4o-mini"),
		},
		Tier2: Tier{
			Provider: strings.ToLower(env("TIER2_PROVIDER", ProviderAnthropic)),
			Model:    env("TIER2_MODEL", "claude-sonnet-4-5"),
		},
		// Base URLs are overridable so a provider can be pointed at a gateway,
		// an Azure deployment, or a local mock without a code change.
		Providers: map[string]Provider{
			ProviderOpenAI:    {Key: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")},
			ProviderAnthropic: {Key: os.Getenv("ANTHROPIC_API_KEY"), BaseURL: os.Getenv("ANTHROPIC_BASE_URL")},
		},

		LLMTimeout:               envDuration("LLM_TIMEOUT", 60*time.Second),
		ConfidenceThreshold:      envFloat("CONFIDENCE_THRESHOLD", 0.72),
		AutoAttachMatchThreshold: envFloat("AUTO_ATTACH_MATCH_THRESHOLD", 0.95),
		TrgmMatchFloor:           envFloat("TRGM_MATCH_FLOOR", 0.45),
		DedupeWindow:             envDuration("DEDUPE_WINDOW", 24*time.Hour),
		WorkerCount:              envInt("WORKER_COUNT", 4),
		MaxAttempts:              envInt("MAX_ATTEMPTS", 3),
		PollInterval:             envDuration("POLL_INTERVAL", 2*time.Second),
		LockTTL:                  envDuration("LOCK_TTL", 5*time.Minute),
		RulesSeedFile:            env("RULES_SEED_FILE", "config/routing_rules.json"),
	}
	return c, c.validate()
}

// validate fails fast and loudly: a pipeline that boots without credentials, or
// pointed at a provider it cannot construct, would silently dead-letter every
// enquiry instead of processing it.
func (c *Config) validate() error {
	var missing []string
	if c.WebhookSecret == "" {
		missing = append(missing, "WEBHOOK_SECRET")
	}
	for _, t := range []struct {
		env  string
		tier Tier
	}{{"TIER1", c.Tier1}, {"TIER2", c.Tier2}} {
		p, known := c.Providers[t.tier.Provider]
		if !known {
			return fmt.Errorf("%s_PROVIDER=%q is not supported (want %s or %s)",
				t.env, t.tier.Provider, ProviderOpenAI, ProviderAnthropic)
		}
		if t.tier.Model == "" {
			return fmt.Errorf("%s_MODEL must not be empty", t.env)
		}
		// Only the providers a tier names need credentials, so a single-vendor
		// setup does not have to carry a key it never uses.
		if p.Key == "" {
			missing = append(missing, keyEnv(t.tier.Provider)+" (used by "+t.env+")")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %v", missing)
	}
	return nil
}

func keyEnv(provider string) string {
	if provider == ProviderAnthropic {
		return "ANTHROPIC_API_KEY"
	}
	return "OPENAI_API_KEY"
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}
