// Package config loads gateway settings from YAML plus environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/optimize"
)

// Config is the full gateway configuration.
type Config struct {
	Listen    string          `yaml:"listen"`
	APIKeys   []string        `yaml:"api_keys"`  // gateway client keys; empty = open
	AdminKey  string          `yaml:"admin_key"` // protects /admin/* (and /metrics if metrics_auth)
	Providers ProvidersConfig `yaml:"providers"`
	Models    []ModelOverride `yaml:"models"`
	Router    RouterConfig    `yaml:"router"`
	Budget    BudgetConfig    `yaml:"budget"`
	Dispatch  DispatchConfig  `yaml:"dispatch"`
	Cache     CacheConfig     `yaml:"cache"`
	Embedder  EmbedderConfig  `yaml:"embedder"`
	Learning  LearningConfig  `yaml:"learning"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
}

// ProviderConfig configures one native provider.
type ProviderConfig struct {
	APIKey          string `yaml:"api_key"`
	BaseURL         string `yaml:"base_url"`
	Organization    string `yaml:"organization"`     // OpenAI only
	ServerFallbacks *bool  `yaml:"server_fallbacks"` // Anthropic only (default true)
	Disabled        bool   `yaml:"disabled"`
}

// ExtraProvider is an OpenAI Chat Completions–compatible endpoint.
type ExtraProvider struct {
	Name      string            `yaml:"name"`
	BaseURL   string            `yaml:"base_url"`
	APIKey    string            `yaml:"api_key"`
	APIKeyEnv string            `yaml:"api_key_env"`
	Headers   map[string]string `yaml:"headers"`
}

// ProvidersConfig holds provider credentials and endpoints.
type ProvidersConfig struct {
	OpenAI    ProviderConfig  `yaml:"openai"`
	Anthropic ProviderConfig  `yaml:"anthropic"`
	Gemini    ProviderConfig  `yaml:"gemini"`
	Extra     []ExtraProvider `yaml:"extra"`
}

// ModelOverride patches a built-in model or defines a new one.
type ModelOverride struct {
	ID            string             `yaml:"id"`
	Provider      string             `yaml:"provider"`
	UpstreamID    string             `yaml:"upstream_id"`
	Aliases       []string           `yaml:"aliases"`
	Tier          string             `yaml:"tier"`
	Enabled       *bool              `yaml:"enabled"`
	Context       int                `yaml:"context"`
	MaxOutput     int                `yaml:"max_output"`
	Price         *catalog.Pricing   `yaml:"price"`
	Caps          *catalog.Caps      `yaml:"caps"`
	Thinking      string             `yaml:"thinking"`
	Efforts       []string           `yaml:"efforts"`
	DefaultEffort string             `yaml:"default_effort"`
	Ability       map[string]float64 `yaml:"ability"`
	TTFTms        float64            `yaml:"ttft_ms"`
	TPS           float64            `yaml:"tps"`
}

// RouterConfig controls routing objectives.
type RouterConfig struct {
	DefaultMode   string                        `yaml:"default_mode"`
	Objectives    map[string]optimize.Objective `yaml:"objectives"`
	ExploreRate   float64                       `yaml:"explore_rate"`
	SwitchPenalty float64                       `yaml:"switch_penalty_usd"`
	ShowHeaders   *bool                         `yaml:"show_headers"`
}

// BudgetConfig sets the spend target.
type BudgetConfig struct {
	USDPerHour float64 `yaml:"usd_per_hour"`
}

// DispatchConfig controls retries, hedging and timeouts.
type DispatchConfig struct {
	MaxAttempts       int           `yaml:"max_attempts"`
	RetryRatio        float64       `yaml:"retry_ratio"`
	ConnectTimeout    time.Duration `yaml:"connect_timeout"`
	FirstTokenTimeout time.Duration `yaml:"first_token_timeout"` // base; scaled by reasoning effort
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	TotalTimeout      time.Duration `yaml:"total_timeout"`
	Hedge             bool          `yaml:"hedge"`
	HedgeMinDelay     time.Duration `yaml:"hedge_min_delay"`
	QualityRetry      *bool         `yaml:"quality_retry"`
}

// CacheConfig controls the exact-match response cache.
type CacheConfig struct {
	Enabled  *bool         `yaml:"enabled"`
	MaxBytes int64         `yaml:"max_bytes"`
	TTL      time.Duration `yaml:"ttl"`
}

// EmbedderConfig locates the (required) embedding model.
type EmbedderConfig struct {
	Dir          string `yaml:"dir"`
	AutoDownload *bool  `yaml:"auto_download"`
}

// LearningConfig controls online learning.
type LearningConfig struct {
	Disabled      bool          `yaml:"disabled"`
	StatePath     string        `yaml:"state_path"`
	LearningRate  float64       `yaml:"learning_rate"`
	FlushInterval time.Duration `yaml:"flush_interval"`
}

// TelemetryConfig controls logs and metrics.
type TelemetryConfig struct {
	DecisionLog string  `yaml:"decision_log"`
	Sample      float64 `yaml:"sample"`
	MetricsAuth bool    `yaml:"metrics_auth"`
}

// Default returns the default configuration.
func Default() Config {
	t := true
	return Config{
		Listen: ":8787",
		Router: RouterConfig{DefaultMode: "balance", SwitchPenalty: 0.0005, ShowHeaders: &t},
		Dispatch: DispatchConfig{
			MaxAttempts: 3, RetryRatio: 0.2, ConnectTimeout: 10 * time.Second,
			FirstTokenTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, TotalTimeout: 20 * time.Minute,
			HedgeMinDelay: 2 * time.Second, QualityRetry: &t,
		},
		Cache:     CacheConfig{Enabled: &t, MaxBytes: 64 << 20, TTL: 10 * time.Minute},
		Embedder:  EmbedderConfig{AutoDownload: &t},
		Learning:  LearningConfig{LearningRate: 0.004, FlushInterval: 30 * time.Second},
		Telemetry: TelemetryConfig{Sample: 1},
	}
}

// Load reads YAML (optional) and overlays environment variables.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(b))), &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(&cfg)
	if cfg.Learning.StatePath == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			cfg.Learning.StatePath = filepath.Join(dir, "modelrouter", "learned.json")
		}
	}
	return cfg, cfg.Validate()
}

func applyEnv(c *Config) {
	set := func(dst *string, keys ...string) {
		for _, k := range keys {
			if v := os.Getenv(k); v != "" {
				*dst = v
				return
			}
		}
	}
	set(&c.Listen, "MODELROUTER_LISTEN")
	if v := os.Getenv("MODELROUTER_API_KEYS"); v != "" {
		c.APIKeys = splitList(v)
	} else if v := os.Getenv("MODELROUTER_API_KEY"); v != "" {
		c.APIKeys = []string{v}
	}
	set(&c.AdminKey, "MODELROUTER_ADMIN_KEY")
	set(&c.Router.DefaultMode, "MODELROUTER_MODE")
	set(&c.Providers.OpenAI.APIKey, "OPENAI_API_KEY")
	set(&c.Providers.OpenAI.BaseURL, "OPENAI_BASE_URL")
	set(&c.Providers.Anthropic.APIKey, "ANTHROPIC_API_KEY")
	set(&c.Providers.Anthropic.BaseURL, "ANTHROPIC_BASE_URL")
	set(&c.Providers.Gemini.APIKey, "GEMINI_API_KEY", "GOOGLE_API_KEY")
	set(&c.Providers.Gemini.BaseURL, "GEMINI_BASE_URL")
	set(&c.Embedder.Dir, "MODELROUTER_EMBEDDER_DIR")
	set(&c.Learning.StatePath, "MODELROUTER_STATE_PATH")
	set(&c.Telemetry.DecisionLog, "MODELROUTER_DECISION_LOG")
	if v := os.Getenv("MODELROUTER_BUDGET_USD_PER_HOUR"); v != "" {
		fmt.Sscanf(v, "%g", &c.Budget.USDPerHour)
	}
	for i := range c.Providers.Extra {
		e := &c.Providers.Extra[i]
		if e.APIKey == "" && e.APIKeyEnv != "" {
			e.APIKey = os.Getenv(e.APIKeyEnv)
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks settings.
func (c Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen address is required")
	}
	if _, ok := optimize.ParseMode(c.Router.DefaultMode); !ok {
		return fmt.Errorf("unknown router.default_mode %q (cost|balance|quality|fast)", c.Router.DefaultMode)
	}
	for k := range c.Router.Objectives {
		if _, ok := optimize.ParseMode(k); !ok {
			return fmt.Errorf("router.objectives: unknown mode %q", k)
		}
	}
	if c.Router.ExploreRate < 0 || c.Router.ExploreRate > 0.2 {
		return fmt.Errorf("router.explore_rate must be in [0, 0.2]")
	}
	for _, e := range c.Providers.Extra {
		if e.Name == "" || e.BaseURL == "" {
			return fmt.Errorf("providers.extra entries need name and base_url")
		}
		switch e.Name {
		case "openai", "anthropic", "gemini":
			return fmt.Errorf("providers.extra name %q collides with a native provider", e.Name)
		}
	}
	return nil
}

// Objectives merges configured objectives over defaults.
func (c Config) Objectives() map[optimize.Mode]optimize.Objective {
	out := optimize.DefaultObjectives()
	for k, v := range c.Router.Objectives {
		m, _ := optimize.ParseMode(k)
		base := out[m]
		if v.ValueUSD > 0 {
			base.ValueUSD = v.ValueUSD
		}
		if v.Floor > 0 {
			base.Floor = v.Floor
		}
		if v.LatencyUSD > 0 {
			base.LatencyUSD = v.LatencyUSD
		}
		if v.Risk > 0 {
			base.Risk = v.Risk
		}
		out[m] = base
	}
	return out
}

// BuildCatalog merges model overrides into the built-in catalog.
func (c Config) BuildCatalog() (*catalog.Catalog, error) {
	models := catalog.Builtin()
	byID := map[string]*catalog.Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	for _, o := range c.Models {
		m, exists := byID[o.ID]
		if !exists {
			if o.Provider == "" || o.UpstreamID == "" || o.Tier == "" || o.Context == 0 || o.Price == nil {
				return nil, fmt.Errorf("models: new model %q needs provider, upstream_id, tier, context, price", o.ID)
			}
			m = &catalog.Model{ID: o.ID, Provider: o.Provider, UpstreamID: o.UpstreamID, Enabled: true, Display: o.ID,
				Caps: catalog.Caps{Tools: true, JSONSchema: true, Sampling: true, Prefill: true}}
			models = append(models, m)
			byID[o.ID] = m
		}
		if o.Provider != "" {
			m.Provider = o.Provider
		}
		if o.UpstreamID != "" {
			m.UpstreamID = o.UpstreamID
		}
		if len(o.Aliases) > 0 {
			m.Aliases = o.Aliases
		}
		if o.Tier != "" {
			t, ok := catalog.ParseTier(o.Tier)
			if !ok {
				return nil, fmt.Errorf("models: %s: unknown tier %q", o.ID, o.Tier)
			}
			m.Tier = t
		}
		if o.Enabled != nil {
			m.Enabled = *o.Enabled
		}
		if o.Context > 0 {
			m.Context = o.Context
		}
		if o.MaxOutput > 0 {
			m.MaxOutput = o.MaxOutput
		}
		if o.Price != nil {
			m.Price = *o.Price
			m.PromoPrice = nil
		}
		if o.Caps != nil {
			m.Caps = *o.Caps
		}
		if o.Thinking != "" {
			m.ThinkingStyle = o.Thinking
		}
		if len(o.Efforts) > 0 {
			m.Efforts = o.Efforts
		}
		if o.DefaultEffort != "" {
			m.DefaultEffort = o.DefaultEffort
		}
		for k, v := range o.Ability {
			a, ok := catalog.ParseAxis(k)
			if !ok {
				return nil, fmt.Errorf("models: %s: unknown ability axis %q", o.ID, k)
			}
			m.Ability[a] = v
		}
		if !exists && len(o.Ability) == 0 {
			// Unknown model without abilities: seed from its tier's typical level.
			lvl := []float64{0.62, 0.8, 0.88, 0.94}[m.Tier.Rank()]
			for a := range m.Ability {
				m.Ability[a] = lvl
			}
		}
		if o.TTFTms > 0 {
			m.TTFTms = o.TTFTms
		}
		if o.TPS > 0 {
			m.TPS = o.TPS
		}
		if m.MaxOutput == 0 {
			m.MaxOutput = 8192
		}
	}
	return catalog.New(models)
}

// CacheEnabled reports whether the response cache is on.
func (c Config) CacheEnabled() bool { return c.Cache.Enabled == nil || *c.Cache.Enabled }

// ShowHeaders reports whether X-Modelrouter-* headers are emitted.
func (c Config) ShowHeaders() bool { return c.Router.ShowHeaders == nil || *c.Router.ShowHeaders }

// AutoDownload reports whether the embedder may be fetched automatically.
func (c Config) AutoDownload() bool {
	return c.Embedder.AutoDownload == nil || *c.Embedder.AutoDownload
}

// QualityRetry reports whether bad non-stream outputs are retried.
func (c Config) QualityRetry() bool {
	return c.Dispatch.QualityRetry == nil || *c.Dispatch.QualityRetry
}

// AnthropicServerFallbacks reports whether server-side refusal fallbacks are enabled.
func (c Config) AnthropicServerFallbacks() bool {
	return c.Providers.Anthropic.ServerFallbacks == nil || *c.Providers.Anthropic.ServerFallbacks
}
