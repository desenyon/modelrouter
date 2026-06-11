// Package api is a typed client for OpenRouter's public API with disk caching.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const BaseURL = "https://openrouter.ai/api/v1"

type Client struct {
	http     *http.Client
	CacheDir string
	TTL      time.Duration
}

func New() *Client {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return &Client{
		http:     &http.Client{Timeout: 30 * time.Second},
		CacheDir: filepath.Join(dir, "modelrouter"),
		TTL:      time.Hour,
	}
}

// Architecture describes a model's modalities and tokenizer.
type Architecture struct {
	Modality         string   `json:"modality"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	Tokenizer        string   `json:"tokenizer"`
	InstructType     *string  `json:"instruct_type"`
}

// Pricing values are USD per token (strings in the API).
type Pricing struct {
	Prompt            string  `json:"prompt"`
	Completion        string  `json:"completion"`
	Image             string  `json:"image,omitempty"`
	Audio             string  `json:"audio,omitempty"`
	WebSearch         string  `json:"web_search,omitempty"`
	InternalReasoning string  `json:"internal_reasoning,omitempty"`
	InputCacheRead    string  `json:"input_cache_read,omitempty"`
	InputCacheWrite   string  `json:"input_cache_write,omitempty"`
	Discount          float64 `json:"discount,omitempty"`
}

type TopProvider struct {
	ContextLength       int  `json:"context_length"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`
	IsModerated         bool `json:"is_moderated"`
}

type Model struct {
	ID                  string         `json:"id"`
	CanonicalSlug       string         `json:"canonical_slug"`
	HuggingFaceID       *string        `json:"hugging_face_id"`
	Name                string         `json:"name"`
	Created             int64          `json:"created"`
	Description         string         `json:"description"`
	ContextLength       int            `json:"context_length"`
	Architecture        Architecture   `json:"architecture"`
	Pricing             Pricing        `json:"pricing"`
	TopProvider         TopProvider    `json:"top_provider"`
	PerRequestLimits    any            `json:"per_request_limits"`
	SupportedParameters []string       `json:"supported_parameters"`
	DefaultParameters   map[string]any `json:"default_parameters"`
	SupportedVoices     []string       `json:"supported_voices"`
	KnowledgeCutoff     *string        `json:"knowledge_cutoff"`
	ExpirationDate      *string        `json:"expiration_date"`
}

type Endpoint struct {
	Name                    string   `json:"name"`
	ModelID                 string   `json:"model_id"`
	ProviderName            string   `json:"provider_name"`
	Tag                     string   `json:"tag"`
	Quantization            string   `json:"quantization"`
	ContextLength           int      `json:"context_length"`
	Pricing                 Pricing  `json:"pricing"`
	MaxCompletionTokens     *int     `json:"max_completion_tokens"`
	MaxPromptTokens         *int     `json:"max_prompt_tokens"`
	SupportedParameters     []string `json:"supported_parameters"`
	Status                  int      `json:"status"`
	UptimeLast5m            *float64 `json:"uptime_last_5m"`
	UptimeLast30m           *float64 `json:"uptime_last_30m"`
	UptimeLast1d            *float64 `json:"uptime_last_1d"`
	LatencyLast30m          *float64 `json:"latency_last_30m"`
	ThroughputLast30m       *float64 `json:"throughput_last_30m"`
	SupportsImplicitCaching bool     `json:"supports_implicit_caching"`
}

type ModelEndpoints struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Created      int64        `json:"created"`
	Description  string       `json:"description"`
	Architecture Architecture `json:"architecture"`
	Endpoints    []Endpoint   `json:"endpoints"`
}

type Provider struct {
	Name             string   `json:"name"`
	Slug             string   `json:"slug"`
	Headquarters     string   `json:"headquarters"`
	Datacenters      []string `json:"datacenters"`
	PrivacyPolicyURL *string  `json:"privacy_policy_url"`
	TermsOfServiceURL *string `json:"terms_of_service_url"`
	StatusPageURL    *string  `json:"status_page_url"`
}

func (c *Client) fetch(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "modelrouter-cli")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return body, nil
}

func (c *Client) cachedFetch(name, path string, force bool) ([]byte, error) {
	return c.cachedFetchURL(name, BaseURL+path, force, c.TTL)
}

// cachedFetchURL returns cached bytes when fresh, otherwise fetches and caches.
// On network failure a stale cache is still used as a fallback.
func (c *Client) cachedFetchURL(name, url string, force bool, ttl time.Duration) ([]byte, error) {
	file := filepath.Join(c.CacheDir, name+".json")
	if !force {
		if st, err := os.Stat(file); err == nil && time.Since(st.ModTime()) < ttl {
			if b, err := os.ReadFile(file); err == nil {
				return b, nil
			}
		}
	}
	b, err := c.fetch(url)
	if err != nil {
		if stale, rerr := os.ReadFile(file); rerr == nil {
			return stale, nil
		}
		return nil, err
	}
	_ = os.MkdirAll(c.CacheDir, 0o755)
	_ = os.WriteFile(file, b, 0o644)
	return b, nil
}

func (c *Client) Models(force bool) ([]Model, error) {
	b, err := c.cachedFetch("models", "/models", force)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

func (c *Client) Providers(force bool) ([]Provider, error) {
	b, err := c.cachedFetch("providers", "/providers", force)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []Provider `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

func (c *Client) Endpoints(modelID string) (*ModelEndpoints, error) {
	name := "endpoints-" + strings.NewReplacer("/", "_", ":", "_").Replace(modelID)
	b, err := c.cachedFetch(name, "/models/"+modelID+"/endpoints", false)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data ModelEndpoints `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

func (c *Client) ClearCache() error { return os.RemoveAll(c.CacheDir) }

// --- model helpers ---

func perTok(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// PromptPerM returns USD per million prompt tokens.
func (p Pricing) PromptPerM() float64     { return perTok(p.Prompt) * 1e6 }
func (p Pricing) CompletionPerM() float64 { return perTok(p.Completion) * 1e6 }
func (p Pricing) CacheReadPerM() float64  { return perTok(p.InputCacheRead) * 1e6 }
func (p Pricing) CacheWritePerM() float64 { return perTok(p.InputCacheWrite) * 1e6 }

func (m Model) Author() string {
	id := strings.TrimPrefix(m.ID, "~")
	if i := strings.Index(id, "/"); i > 0 {
		return id[:i]
	}
	return id
}

func (m Model) IsFree() bool {
	return perTok(m.Pricing.Prompt) == 0 && perTok(m.Pricing.Completion) == 0
}

func (m Model) HasParam(p string) bool {
	for _, s := range m.SupportedParameters {
		if s == p {
			return true
		}
	}
	return false
}

func (m Model) HasInput(mod string) bool {
	for _, s := range m.Architecture.InputModalities {
		if s == mod {
			return true
		}
	}
	return false
}

func (m Model) CreatedTime() time.Time { return time.Unix(m.Created, 0) }
