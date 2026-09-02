package config

import (
	"strings"
	"testing"
)

// The tier/provider pairing is the one piece of branch logic here: a tier may
// name either provider, and only the providers actually named need credentials.
// Getting that wrong either demands a key nobody uses or boots without one.
func TestLoadTierProviderPairing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty means the load must succeed
		check   func(*testing.T, Config)
	}{
		{
			name: "defaults split the tiers across both providers",
			env:  map[string]string{"OPENAI_API_KEY": "o", "ANTHROPIC_API_KEY": "a"},
			check: func(t *testing.T, c Config) {
				if c.Tier1.String() != "openai/gpt-4o-mini" {
					t.Errorf("tier1 = %s", c.Tier1)
				}
				if c.Tier2.String() != "anthropic/claude-sonnet-4-5" {
					t.Errorf("tier2 = %s", c.Tier2)
				}
			},
		},
		{
			// The point of the change: tier 2 is not pinned to Anthropic.
			name: "both tiers on one provider needs only that provider's key",
			env: map[string]string{
				"OPENAI_API_KEY":    "o",
				"ANTHROPIC_API_KEY": "",
				"TIER2_PROVIDER":    "openai",
				"TIER2_MODEL":       "gpt-4o",
			},
			check: func(t *testing.T, c Config) {
				if c.Tier2.String() != "openai/gpt-4o" {
					t.Errorf("tier2 = %s", c.Tier2)
				}
			},
		},
		{
			name: "provider names are case-insensitive",
			env: map[string]string{
				"OPENAI_API_KEY": "", "ANTHROPIC_API_KEY": "a",
				"TIER1_PROVIDER": "Anthropic", "TIER1_MODEL": "claude-haiku-4-5",
			},
			check: func(t *testing.T, c Config) {
				if c.Tier1.Provider != ProviderAnthropic {
					t.Errorf("tier1 provider = %q", c.Tier1.Provider)
				}
			},
		},
		{
			name: "a tier without its provider's key is refused",
			env: map[string]string{
				"OPENAI_API_KEY": "", "ANTHROPIC_API_KEY": "a",
				"TIER2_PROVIDER": "openai", "TIER2_MODEL": "gpt-4o",
			},
			wantErr: "OPENAI_API_KEY",
		},
		{
			name: "an unknown provider is refused",
			env: map[string]string{
				"OPENAI_API_KEY": "o", "ANTHROPIC_API_KEY": "a",
				"TIER1_PROVIDER": "gemini",
			},
			wantErr: `TIER1_PROVIDER="gemini"`,
		},
		{
			name:    "a missing webhook secret is still refused",
			env:     map[string]string{"WEBHOOK_SECRET": "", "OPENAI_API_KEY": "o", "ANTHROPIC_API_KEY": "a"},
			wantErr: "WEBHOOK_SECRET",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Clear everything this test reads so the ambient environment cannot
			// make a case pass or fail by accident.
			for _, k := range []string{
				"WEBHOOK_SECRET", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
				"TIER1_PROVIDER", "TIER1_MODEL", "TIER2_PROVIDER", "TIER2_MODEL",
			} {
				t.Setenv(k, "")
			}
			t.Setenv("WEBHOOK_SECRET", "s")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			c, err := Load()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, c)
		})
	}
}

// Reachable only by constructing a Config directly, but a tier with no model
// would otherwise reach the provider SDK as an empty model name.
func TestValidateRejectsEmptyModel(t *testing.T) {
	c := Config{
		WebhookSecret: "s",
		Tier1:         Tier{Provider: ProviderOpenAI, Model: "gpt-4o-mini"},
		Tier2:         Tier{Provider: ProviderOpenAI, Model: ""},
		Providers:     map[string]Provider{ProviderOpenAI: {Key: "o"}},
	}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "TIER2_MODEL") {
		t.Fatalf("want a TIER2_MODEL error, got %v", err)
	}
}
