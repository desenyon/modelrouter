// Package config loads modelrouter gateway settings from YAML and the environment.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode is how aggressively the router spends on frontier models.
type Mode string

const (
	ModeCost         Mode = "cost"
	ModeBalance      Mode = "balance"
	ModeIntelligence Mode = "intelligence"
)

// Config is the full gateway configuration.
type Config struct {
	Listen   string         `yaml:"listen"`
	APIKey   string         `yaml:"api_key"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Router   RouterConfig   `yaml:"router"`
	Policy   PolicyConfig   `yaml:"policy"`
	Cache    CacheConfig    `yaml:"cache"`
	Cascade  CascadeConfig  `yaml:"cascade"`
	Models   ModelsConfig   `yaml:"models"`
}

// UpstreamConfig points at an OpenAI-compatible provider.
type UpstreamConfig struct {
	BaseURL string        `yaml:"base_url"`
	APIKey  string        `yaml:"api_key"`
	Timeout time.Duration `yaml:"timeout"`
}

// RouterConfig controls classification thresholds and adaptation.
type RouterConfig struct {
	DefaultMode   Mode    `yaml:"default_mode"`
	LunaMaxScore  float64 `yaml:"luna_max_score"`
	TerraMaxScore float64 `yaml:"terra_max_score"`
	ShowRouted    bool    `yaml:"show_routed"`
	Adaptive      bool    `yaml:"adaptive"`
	SolShareTarget float64 `yaml:"sol_share_target"`
}

// PolicyConfig holds Luna-first guardrail knobs.
type PolicyConfig struct {
	ForceLunaEasy      bool    `yaml:"force_luna_easy"`
	MinTerraTools      int     `yaml:"min_terra_tools"`
	SuppressSolBelow   float64 `yaml:"suppress_sol_below"`
	MinSolHardMarkers  int     `yaml:"min_sol_hard_markers"`
	MinSolTokens       int     `yaml:"min_sol_tokens"`
}

// CacheConfig controls the fingerprint response cache.
type CacheConfig struct {
	Enabled  bool `yaml:"enabled"`
	Capacity int  `yaml:"capacity"`
}

// CascadeConfig controls one-shot escalate-on-failure.
type CascadeConfig struct {
	Enabled bool `yaml:"enabled"`
}

// TierPool is a primary model plus optional fallbacks for one tier.
type TierPool struct {
	Primary   string   `yaml:"primary"`
	Fallbacks []string `yaml:"fallbacks"`
}

// UnmarshalYAML accepts either a plain string or {primary,fallbacks}.
func (t *TierPool) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		t.Primary = s
		t.Fallbacks = nil
		return nil
	}
	type raw TierPool
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*t = TierPool(r)
	return nil
}

// Candidates returns primary followed by fallbacks.
func (t TierPool) Candidates() []string {
	out := make([]string, 0, 1+len(t.Fallbacks))
	if t.Primary != "" {
		out = append(out, t.Primary)
	}
	out = append(out, t.Fallbacks...)
	return out
}

// ModelsConfig maps logical tiers to candidate pools.
type ModelsConfig struct {
	Luna  TierPool `yaml:"luna"`
	Terra TierPool `yaml:"terra"`
	Sol   TierPool `yaml:"sol"`
}

// Default returns a Luna-first, production-ready configuration.
func Default() Config {
	return Config{
		Listen: ":8787",
		Upstream: UpstreamConfig{
			BaseURL: "https://openrouter.ai/api/v1",
			Timeout: 120 * time.Second,
		},
		Router: RouterConfig{
			DefaultMode:    ModeBalance,
			LunaMaxScore:   0.42,
			TerraMaxScore:  0.72,
			ShowRouted:     true,
			Adaptive:       true,
			SolShareTarget: 0.15,
		},
		Policy: PolicyConfig{
			ForceLunaEasy:     true,
			MinTerraTools:     3,
			SuppressSolBelow:  0.42,
			MinSolHardMarkers: 3,
			MinSolTokens:      1500,
		},
		Cache: CacheConfig{
			Enabled:  true,
			Capacity: 2048,
		},
		Cascade: CascadeConfig{
			Enabled: true,
		},
		Models: ModelsConfig{
			Luna:  TierPool{Primary: "openai/gpt-5.6-luna"},
			Terra: TierPool{Primary: "openai/gpt-5.6-terra"},
			Sol:   TierPool{Primary: "openai/gpt-5.6-sol"},
		},
	}
}

// Load reads YAML from path (optional) and overlays environment variables.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(&cfg)
	normalize(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalize(cfg *Config) {
	if cfg.Upstream.Timeout <= 0 {
		cfg.Upstream.Timeout = 120 * time.Second
	}
	if cfg.Router.DefaultMode == "" {
		cfg.Router.DefaultMode = ModeBalance
	}
	if cfg.Router.SolShareTarget <= 0 {
		cfg.Router.SolShareTarget = 0.15
	}
	if cfg.Cache.Capacity <= 0 {
		cfg.Cache.Capacity = 2048
	}
	if cfg.Policy.SuppressSolBelow <= 0 {
		cfg.Policy.SuppressSolBelow = cfg.Router.LunaMaxScore
	}
	if cfg.Policy.MinTerraTools <= 0 {
		cfg.Policy.MinTerraTools = 3
	}
	if cfg.Policy.MinSolHardMarkers <= 0 {
		cfg.Policy.MinSolHardMarkers = 3
	}
	if cfg.Policy.MinSolTokens <= 0 {
		cfg.Policy.MinSolTokens = 1500
	}
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("MODELROUTER_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("MODELROUTER_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("MODELROUTER_UPSTREAM_BASE_URL"); v != "" {
		cfg.Upstream.BaseURL = v
	}
	if v := firstEnv("MODELROUTER_UPSTREAM_API_KEY", "OPENROUTER_API_KEY", "OPENAI_API_KEY"); v != "" {
		cfg.Upstream.APIKey = v
	}
	if v := os.Getenv("MODELROUTER_MODE"); v != "" {
		cfg.Router.DefaultMode = Mode(strings.ToLower(v))
	}
	if v := os.Getenv("MODELROUTER_LUNA"); v != "" {
		cfg.Models.Luna = TierPool{Primary: v}
	}
	if v := os.Getenv("MODELROUTER_TERRA"); v != "" {
		cfg.Models.Terra = TierPool{Primary: v}
	}
	if v := os.Getenv("MODELROUTER_SOL"); v != "" {
		cfg.Models.Sol = TierPool{Primary: v}
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Validate checks required fields and sane thresholds.
func (c Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen address is required")
	}
	if c.Upstream.BaseURL == "" {
		return fmt.Errorf("upstream.base_url is required")
	}
	if c.Models.Luna.Primary == "" || c.Models.Terra.Primary == "" || c.Models.Sol.Primary == "" {
		return fmt.Errorf("models.luna, models.terra, and models.sol primaries are required")
	}
	switch c.Router.DefaultMode {
	case ModeCost, ModeBalance, ModeIntelligence, "":
	default:
		return fmt.Errorf("unknown router.default_mode %q (cost|balance|intelligence)", c.Router.DefaultMode)
	}
	if c.Router.LunaMaxScore <= 0 || c.Router.TerraMaxScore <= c.Router.LunaMaxScore || c.Router.TerraMaxScore >= 1 {
		return fmt.Errorf("router thresholds must satisfy 0 < luna_max_score < terra_max_score < 1")
	}
	return nil
}

// ModeOrDefault returns the configured default mode.
func (c Config) ModeOrDefault() Mode {
	if c.Router.DefaultMode == "" {
		return ModeBalance
	}
	return c.Router.DefaultMode
}
