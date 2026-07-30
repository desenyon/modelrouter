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
	http            *http.Client
	baseURL         string
	frontendBaseURL string
	CacheDir        string
	TTL             time.Duration
}

type UnexpectedShapeError struct {
	Endpoint string
	Detail   string
}

func (e UnexpectedShapeError) Error() string {
	return fmt.Sprintf("%s returned an unexpected response: %s", e.Endpoint, e.Detail)
}

func New() *Client {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return &Client{
		http:            &http.Client{Timeout: 30 * time.Second},
		baseURL:         BaseURL,
		frontendBaseURL: frontendBaseURL,
		CacheDir:        filepath.Join(dir, "modelrouter"),
		TTL:             RankingsTTL,
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
	Prompt            string            `json:"prompt"`
	Completion        string            `json:"completion"`
	Request           string            `json:"request,omitempty"`
	Image             string            `json:"image,omitempty"`
	ImageOutput       string            `json:"image_output,omitempty"`
	ImageToken        string            `json:"image_token,omitempty"`
	Audio             string            `json:"audio,omitempty"`
	AudioOutput       string            `json:"audio_output,omitempty"`
	WebSearch         string            `json:"web_search,omitempty"`
	InternalReasoning string            `json:"internal_reasoning,omitempty"`
	InputCacheRead    string            `json:"input_cache_read,omitempty"`
	InputCacheWrite   string            `json:"input_cache_write,omitempty"`
	Discount          float64           `json:"discount,omitempty"`
	Overrides         []PricingOverride `json:"overrides,omitempty"`
}

type PricingOverride struct {
	MinPromptTokens *int   `json:"min_prompt_tokens,omitempty"`
	UTCStart        *int   `json:"utc_start,omitempty"`
	UTCEnd          *int   `json:"utc_end,omitempty"`
	Prompt          string `json:"prompt,omitempty"`
	Completion      string `json:"completion,omitempty"`
	InputCacheRead  string `json:"input_cache_read,omitempty"`
	InputCacheWrite string `json:"input_cache_write,omitempty"`
}

type ModelLinks struct {
	Details string `json:"details"`
}

type ReasoningConfig struct {
	Mandatory         bool     `json:"mandatory"`
	DefaultEnabled    bool     `json:"default_enabled"`
	SupportsMaxTokens bool     `json:"supports_max_tokens"`
	SupportedEfforts  []string `json:"supported_efforts"`
	DefaultEffort     string   `json:"default_effort"`
}

type TopProvider struct {
	ContextLength       int  `json:"context_length"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`
	IsModerated         bool `json:"is_moderated"`
}

type Model struct {
	ID                  string           `json:"id"`
	CanonicalSlug       string           `json:"canonical_slug"`
	HuggingFaceID       *string          `json:"hugging_face_id"`
	Name                string           `json:"name"`
	Created             int64            `json:"created"`
	Description         string           `json:"description"`
	ContextLength       int              `json:"context_length"`
	Architecture        Architecture     `json:"architecture"`
	Pricing             Pricing          `json:"pricing"`
	TopProvider         TopProvider      `json:"top_provider"`
	PerRequestLimits    any              `json:"per_request_limits"`
	SupportedParameters []string         `json:"supported_parameters"`
	DefaultParameters   map[string]any   `json:"default_parameters"`
	SupportedVoices     []string         `json:"supported_voices"`
	KnowledgeCutoff     *string          `json:"knowledge_cutoff"`
	ExpirationDate      *string          `json:"expiration_date"`
	Links               ModelLinks       `json:"links"`
	Benchmarks          *ModelBenchmarks `json:"benchmarks,omitempty"`
	Reasoning           *ReasoningConfig `json:"reasoning,omitempty"`
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
	Name               string             `json:"name"`
	DisplayName        string             `json:"displayName"`
	Slug               string             `json:"slug"`
	BaseURL            string             `json:"baseUrl"`
	Headquarters       string             `json:"headquarters"`
	Datacenters        []string           `json:"datacenters"`
	PrivacyPolicyURL   *string            `json:"privacy_policy_url"`
	TermsOfServiceURL  *string            `json:"terms_of_service_url"`
	StatusPageURL      *string            `json:"status_page_url"`
	DataPolicy         ProviderDataPolicy `json:"dataPolicy"`
	HasChatCompletions bool               `json:"hasChatCompletions"`
	HasCompletions     bool               `json:"hasCompletions"`
	IsAbortable        bool               `json:"isAbortable"`
	ModerationRequired bool               `json:"moderationRequired"`
	BYOKEnabled        bool               `json:"byokEnabled"`
	SendClientIP       bool               `json:"sendClientIp"`
	StatusPageURLV1    string             `json:"statusPageUrl"`
	PolicyAvailable    bool               `json:"policy_metadata_available"`
}

type ProviderDataPolicy struct {
	Training           bool   `json:"training"`
	TrainingOpenRouter bool   `json:"trainingOpenRouter"`
	RetainsPrompts     bool   `json:"retainsPrompts"`
	CanPublish         bool   `json:"canPublish"`
	RequiresUserIDs    bool   `json:"requiresUserIDs"`
	TermsOfServiceURL  string `json:"termsOfServiceURL"`
	PrivacyPolicyURL   string `json:"privacyPolicyURL"`
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
	return c.cachedFetchURL(name, c.baseURL+path, force, c.TTL)
}

// cachedFetchURL returns cached bytes when fresh, otherwise fetches and caches.
// Normal reads may fall back to stale cache for offline use. Forced refreshes
// are strict so --refresh can never report stale bytes as live.
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
		if !force {
			if stale, rerr := os.ReadFile(file); rerr == nil {
				return stale, nil
			}
		}
		return nil, err
	}
	_ = os.MkdirAll(c.CacheDir, 0o755)
	if err := os.WriteFile(file, b, 0o644); err != nil {
		if !force {
			if stale, rerr := os.ReadFile(file); rerr == nil {
				return stale, nil
			}
		}
		return nil, err
	}
	return b, nil
}

func (c *Client) cacheTime(name string) time.Time {
	if st, err := os.Stat(filepath.Join(c.CacheDir, name+".json")); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func (c *Client) Models(force bool) ([]Model, error) {
	b, err := c.cachedFetch("models-all", "/models?output_modalities=all", force)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	if len(env.Data) == 0 {
		return nil, UnexpectedShapeError{Endpoint: "/models", Detail: "expected a non-empty data array"}
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
	official := env.Data
	if len(official) == 0 {
		return nil, UnexpectedShapeError{Endpoint: "/providers", Detail: "expected a non-empty data array"}
	}
	rich, richErr := frontendGet[[]Provider](c, "providers-rich", "/providers", force)
	if richErr != nil || len(rich) == 0 {
		return official, nil
	}
	bySlug := make(map[string]Provider, len(rich))
	for _, p := range rich {
		p.PolicyAvailable = true
		bySlug[p.Slug] = p
	}
	for i, p := range official {
		if enriched, ok := bySlug[p.Slug]; ok {
			enriched.Datacenters = p.Datacenters
			enriched.PrivacyPolicyURL = p.PrivacyPolicyURL
			enriched.TermsOfServiceURL = p.TermsOfServiceURL
			enriched.StatusPageURL = p.StatusPageURL
			if enriched.Headquarters == "" {
				enriched.Headquarters = p.Headquarters
			}
			official[i] = enriched
			delete(bySlug, p.Slug)
		}
	}
	for _, p := range rich {
		if _, ok := bySlug[p.Slug]; ok {
			p.PolicyAvailable = true
			official = append(official, p)
		}
	}
	return official, nil
}

func (c *Client) Endpoints(modelID string, refresh ...bool) (*ModelEndpoints, error) {
	name := "endpoints-" + strings.NewReplacer("/", "_", ":", "_").Replace(modelID)
	force := len(refresh) > 0 && refresh[0]
	b, err := c.cachedFetch(name, "/models/"+modelID+"/endpoints", force)
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

func (p Provider) Label() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
}

func (p Provider) PrivacyURL() string {
	if p.DataPolicy.PrivacyPolicyURL != "" {
		return p.DataPolicy.PrivacyPolicyURL
	}
	if p.PrivacyPolicyURL != nil {
		return *p.PrivacyPolicyURL
	}
	return ""
}

func (p Provider) TermsURL() string {
	if p.DataPolicy.TermsOfServiceURL != "" {
		return p.DataPolicy.TermsOfServiceURL
	}
	if p.TermsOfServiceURL != nil {
		return *p.TermsOfServiceURL
	}
	return ""
}

func (p Provider) StatusURL() string {
	if p.StatusPageURLV1 != "" {
		return p.StatusPageURLV1
	}
	if p.StatusPageURL != nil {
		return *p.StatusPageURL
	}
	return ""
}

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
	if !m.HasOutput("text") && !m.HasOutput("embeddings") {
		return strings.HasSuffix(m.ID, ":free")
	}
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

func (m Model) HasOutput(mod string) bool {
	for _, s := range m.Architecture.OutputModalities {
		if s == mod {
			return true
		}
	}
	return false
}

func (m Model) InputPrice() string {
	if m.HasInput("text") {
		if perTok(m.Pricing.Prompt) < 0 {
			return "dynamic"
		}
		return FmtPrice(m.Pricing.PromptPerM()) + "/M"
	}
	if perTok(m.Pricing.Prompt) < 0 {
		return "dynamic"
	}
	if m.Pricing.Prompt == "" || perTok(m.Pricing.Prompt) == 0 {
		return "-"
	}
	return FmtPrice(perTok(m.Pricing.Prompt)*1e6) + "/Mu"
}

func (m Model) OutputPrice() string {
	if m.HasOutput("text") {
		if perTok(m.Pricing.Completion) < 0 {
			return "dynamic"
		}
		return FmtPrice(m.Pricing.CompletionPerM()) + "/M"
	}
	if m.HasOutput("image") && perTok(m.Pricing.ImageOutput) > 0 {
		return FmtPrice(perTok(m.Pricing.ImageOutput)*1e6) + "/Mu"
	}
	if m.Pricing.Completion == "" || perTok(m.Pricing.Completion) == 0 {
		return "-"
	}
	return FmtPrice(perTok(m.Pricing.Completion)*1e6) + "/Mu"
}

func (m Model) CreatedTime() time.Time { return time.Unix(m.Created, 0) }
