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
	Models   ModelsConfig   `yaml:"models"`
}

// UpstreamConfig points at an OpenAI-compatible provider (OpenRouter, OpenAI, etc.).
type UpstreamConfig struct {
	BaseURL string        `yaml:"base_url"`
	APIKey  string        `yaml:"api_key"`
	Timeout time.Duration `yaml:"timeout"`
}

// RouterConfig controls classification and optimization.
type RouterConfig struct {
	DefaultMode   Mode    `yaml:"default_mode"`
	LunaMaxScore  float64 `yaml:"luna_max_score"`
	TerraMaxScore float64 `yaml:"terra_max_score"`
	ShowRouted    bool    `yaml:"show_routed"`
}

// ModelsConfig maps logical tiers to concrete upstream model IDs.
type ModelsConfig struct {
	Luna  string `yaml:"luna"`
	Terra string `yaml:"terra"`
	Sol   string `yaml:"sol"`
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
			DefaultMode:   ModeBalance,
			LunaMaxScore:  0.42,
			TerraMaxScore: 0.72,
			ShowRouted:    true,
		},
		Models: ModelsConfig{
			Luna:  "openai/gpt-5.6-luna",
			Terra: "openai/gpt-5.6-terra",
			Sol:   "openai/gpt-5.6-sol",
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
	if cfg.Upstream.Timeout <= 0 {
		cfg.Upstream.Timeout = 120 * time.Second
	}
	if cfg.Router.DefaultMode == "" {
		cfg.Router.DefaultMode = ModeBalance
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
		cfg.Models.Luna = v
	}
	if v := os.Getenv("MODELROUTER_TERRA"); v != "" {
		cfg.Models.Terra = v
	}
	if v := os.Getenv("MODELROUTER_SOL"); v != "" {
		cfg.Models.Sol = v
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
	if c.Models.Luna == "" || c.Models.Terra == "" || c.Models.Sol == "" {
		return fmt.Errorf("models.luna, models.terra, and models.sol are required")
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
