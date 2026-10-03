package catalog

import "time"

// Snapshot is the date the built-in catalog facts (ids, prices, limits,
// capabilities) were verified against provider documentation.
const Snapshot = "2026-10-02"

// ab builds an ability vector in axis order:
// reasoning, coding, math, knowledge, creative, instruction, agentic, long_context.
func ab(r, c, m, k, cr, in, ag, lc float64) Vec {
	return Vec{r, c, m, k, cr, in, ag, lc}
}

var (
	effortsOpenAIFull  = []string{"none", "low", "medium", "high", "xhigh", "max"}
	effortsOpenAINoOff = []string{"low", "medium", "high", "xhigh", "max"}
	effortsAnthropic   = []string{"low", "medium", "high", "xhigh", "max"}
	effortsHaiku       = []string{"none", "high"} // budget-token thinking off/on
	effortsGemini3     = []string{"low", "medium", "high"}
	effortsGeminiPro   = []string{"low", "high"}
	effortsGemini25    = []string{"none", "low", "medium", "high"}
)

// Builtin returns the default model catalog. Ability priors are hand-set
// from tier positioning and published benchmarks and are refined online by
// feedback; prices and limits come from provider docs as of Snapshot.
func Builtin() []*Model {
	geminiPromoEnd := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	openaiLong := func(p Pricing) Pricing {
		p.LongThreshold, p.LongInMult, p.LongOutMult = 272_000, 2, 1.5
		return p
	}
	return []*Model{
		// ───────────────────────────── OpenAI (Responses API) ─────────────────────────────
		{
			ID: "openai/gpt-6-astra", Provider: "openai", UpstreamID: "gpt-6-astra", Display: "GPT-6 Astra",
			Tier: Astra, Enabled: true, Context: 1_050_000, MaxInput: 922_000, MaxOutput: 128_000,
			Price:         openaiLong(Pricing{Input: 10, Output: 50, CachedIn: 1, CacheWrite: 12.5}),
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkOpenAI, Efforts: effortsOpenAINoOff, DefaultEffort: "medium",
			Ability: ab(.96, .95, .96, .95, .92, .95, .95, .93), TTFTms: 2500, TPS: 55, Verbosity: 1.1,
		},
		{
			ID: "openai/gpt-6.1-sol", Provider: "openai", UpstreamID: "gpt-6.1-sol", Display: "GPT-6.1 Sol",
			Tier: Sol, Enabled: true, Context: 1_050_000, MaxInput: 922_000, MaxOutput: 128_000,
			Price:         openaiLong(Pricing{Input: 2, Output: 10, CachedIn: 0.1, CacheWrite: 2.5}),
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkOpenAI, Efforts: effortsOpenAINoOff, DefaultEffort: "medium",
			Ability: ab(.90, .91, .91, .88, .85, .90, .90, .88), TTFTms: 1600, TPS: 75, Verbosity: 1.0,
		},
		{
			ID: "openai/gpt-5.6-terra", Provider: "openai", UpstreamID: "gpt-5.6-terra", Display: "GPT-5.6 Terra",
			Tier: Terra, Enabled: true, Context: 1_050_000, MaxInput: 922_000, MaxOutput: 128_000,
			Price:         openaiLong(Pricing{Input: 2, Output: 12, CachedIn: 0.2, CacheWrite: 2.5}),
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkOpenAI, Efforts: effortsOpenAIFull, DefaultEffort: "medium",
			Ability: ab(.78, .80, .79, .80, .76, .81, .78, .78), TTFTms: 1100, TPS: 90, Verbosity: 1.0,
			Notes: "dominated on price by gpt-6.1-sol; kept for explicit requests and provider diversity",
		},
		{
			ID: "openai/gpt-6-luna", Provider: "openai", UpstreamID: "gpt-6-luna", Display: "GPT-6 Luna",
			Tier: Luna, Enabled: true, Context: 1_050_000, MaxInput: 922_000, MaxOutput: 128_000,
			Price:         openaiLong(Pricing{Input: 0.10, Output: 0.50, CachedIn: 0.01, CacheWrite: 0.125}),
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkOpenAI, Efforts: effortsOpenAIFull, DefaultEffort: "medium",
			Ability: ab(.66, .67, .66, .64, .62, .70, .64, .66), TTFTms: 600, TPS: 150, Verbosity: 0.9,
		},
		// Known but not auto-routed (superseded or dominated); explicit requests work.
		{
			ID: "openai/gpt-6-sol", Provider: "openai", UpstreamID: "gpt-6-sol", Display: "GPT-6 Sol",
			Tier: Sol, Context: 1_050_000, MaxInput: 922_000, MaxOutput: 128_000,
			Price:         openaiLong(Pricing{Input: 2, Output: 10, CachedIn: 0.2, CacheWrite: 2.5}),
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkOpenAI, Efforts: effortsOpenAIFull, DefaultEffort: "medium",
			Ability: ab(.88, .89, .89, .87, .84, .88, .88, .86), TTFTms: 1500, TPS: 75,
		},
		{
			ID: "openai/gpt-5.6-sol", Provider: "openai", UpstreamID: "gpt-5.6-sol", Display: "GPT-5.6 Sol",
			Tier: Sol, Context: 1_050_000, MaxInput: 922_000, MaxOutput: 128_000,
			Price:         openaiLong(Pricing{Input: 4, Output: 20, CachedIn: 0.4, CacheWrite: 5}),
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkOpenAI, Efforts: effortsOpenAIFull, DefaultEffort: "medium",
			Ability: ab(.87, .88, .88, .86, .83, .87, .86, .85), TTFTms: 1500, TPS: 70,
		},
		{
			ID: "openai/gpt-5.6-luna", Provider: "openai", UpstreamID: "gpt-5.6-luna", Display: "GPT-5.6 Luna",
			Tier: Luna, Context: 1_050_000, MaxInput: 922_000, MaxOutput: 128_000,
			Price:         openaiLong(Pricing{Input: 0.20, Output: 1.20, CachedIn: 0.02, CacheWrite: 0.25}),
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkOpenAI, Efforts: effortsOpenAIFull, DefaultEffort: "medium",
			Ability: ab(.62, .63, .62, .61, .60, .66, .60, .62), TTFTms: 650, TPS: 140,
		},

		// ───────────────────────────── Anthropic (Messages API) ─────────────────────────────
		{
			ID: "anthropic/claude-fable-5-1", Provider: "anthropic", UpstreamID: "claude-fable-5-1", Display: "Claude Fable 5.1",
			Aliases: []string{"fable"},
			Tier:    Astra, Enabled: true, Context: 1_000_000, MaxOutput: 128_000,
			Price: Pricing{Input: 10, Output: 50, CachedIn: 0.25, CacheWrite: 12.5},
			// Forced tool_choice and sampling params are rejected (400); thinking always on.
			Caps:          Caps{Tools: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkAnthropicAdaptive, Efforts: effortsAnthropic, DefaultEffort: "high",
			Ability: ab(.97, .96, .95, .95, .95, .95, .97, .95), TTFTms: 3000, TPS: 50, Verbosity: 1.1,
			Notes: "requires 30-day retention (not ZDR); forced tool_choice unsupported",
		},
		{
			ID: "anthropic/claude-opus-5-5", Provider: "anthropic", UpstreamID: "claude-opus-5-5", Display: "Claude Opus 5.5",
			Aliases: []string{"opus"},
			Tier:    Sol, Enabled: true, Context: 1_000_000, MaxOutput: 128_000,
			Price:         Pricing{Input: 4, Output: 20, CachedIn: 0.20, CacheWrite: 5},
			Caps:          Caps{Tools: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkAnthropicAdaptive, Efforts: effortsAnthropic, DefaultEffort: "medium",
			Ability: ab(.93, .94, .91, .92, .93, .93, .94, .92), TTFTms: 1800, TPS: 65, Verbosity: 1.05,
		},
		{
			ID: "anthropic/claude-sonnet-5-5", Provider: "anthropic", UpstreamID: "claude-sonnet-5-5", Display: "Claude Sonnet 5.5",
			Aliases: []string{"sonnet"},
			Tier:    Terra, Enabled: true, Context: 1_000_000, MaxOutput: 128_000,
			Price:         Pricing{Input: 2, Output: 10, CachedIn: 0.20, CacheWrite: 2.5},
			Caps:          Caps{Tools: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkAnthropicAdaptive, Efforts: effortsAnthropic, DefaultEffort: "high",
			Ability: ab(.86, .89, .84, .86, .88, .88, .89, .86), TTFTms: 1100, TPS: 85,
		},
		{
			ID: "anthropic/claude-haiku-4-5", Provider: "anthropic", UpstreamID: "claude-haiku-4-5", Display: "Claude Haiku 4.5",
			Aliases: []string{"haiku"},
			Tier:    Luna, Enabled: true, Context: 200_000, MaxOutput: 64_000,
			Price:         Pricing{Input: 1, Output: 5, CachedIn: 0.10, CacheWrite: 1.25},
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true},
			ThinkingStyle: ThinkAnthropicBudget, Efforts: effortsHaiku, DefaultEffort: "none",
			Ability: ab(.64, .70, .60, .66, .68, .72, .70, .62), TTFTms: 500, TPS: 140,
		},
		{
			ID: "anthropic/claude-opus-5", Provider: "anthropic", UpstreamID: "claude-opus-5", Display: "Claude Opus 5",
			Tier: Sol, Context: 1_000_000, MaxOutput: 128_000,
			Price:         Pricing{Input: 5, Output: 25, CachedIn: 0.50, CacheWrite: 6.25},
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true},
			ThinkingStyle: ThinkAnthropicAdaptive, Efforts: effortsAnthropic, DefaultEffort: "high",
			Ability: ab(.91, .92, .89, .91, .91, .91, .92, .90), TTFTms: 1900, TPS: 60,
		},

		// ───────────────────────────── Google Gemini (generateContent) ─────────────────────────────
		{
			ID: "gemini/gemini-3.1-pro-preview", Provider: "gemini", UpstreamID: "gemini-3.1-pro-preview", Display: "Gemini 3.1 Pro (preview)",
			Aliases: []string{"gemini-pro"},
			Tier:    Sol, Enabled: true, Context: 1_048_576, MaxOutput: 65_536,
			Price:         Pricing{Input: 2, Output: 12, CachedIn: 0.20, LongThreshold: 200_000, LongInMult: 2, LongOutMult: 1.5},
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true, Seed: true},
			ThinkingStyle: ThinkGeminiLevel, Efforts: effortsGeminiPro, DefaultEffort: "high",
			Ability: ab(.89, .87, .91, .91, .85, .86, .85, .91), TTFTms: 2200, TPS: 80,
		},
		{
			ID: "gemini/gemini-3.8-flash", Provider: "gemini", UpstreamID: "gemini-3.8-flash", Display: "Gemini 3.8 Flash",
			Aliases: []string{"gemini-flash"},
			Tier:    Terra, Enabled: true, Context: 1_048_576, MaxOutput: 65_536,
			Price:         Pricing{Input: 1.50, Output: 7.50, CachedIn: 0.15},
			PromoPrice:    &Pricing{Input: 0.75, Output: 3.75, CachedIn: 0.075},
			PromoUntil:    geminiPromoEnd,
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true, Seed: true},
			ThinkingStyle: ThinkGeminiLevel, Efforts: effortsGemini3, DefaultEffort: "high",
			Ability: ab(.82, .83, .85, .84, .78, .80, .82, .86), TTFTms: 900, TPS: 160,
		},
		{
			ID: "gemini/gemini-3.1-flash-lite", Provider: "gemini", UpstreamID: "gemini-3.1-flash-lite", Display: "Gemini 3.1 Flash-Lite",
			Tier: Luna, Enabled: true, Context: 1_048_576, MaxOutput: 65_536,
			Price:         Pricing{Input: 0.25, Output: 1.50, CachedIn: 0.025},
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true, Seed: true},
			ThinkingStyle: ThinkGeminiLevel, Efforts: effortsGeminiPro, DefaultEffort: "low",
			Ability: ab(.62, .60, .65, .66, .60, .64, .58, .70), TTFTms: 450, TPS: 220,
		},
		{
			ID: "gemini/gemini-2.5-flash-lite", Provider: "gemini", UpstreamID: "gemini-2.5-flash-lite", Display: "Gemini 2.5 Flash-Lite",
			Tier: Luna, Enabled: true, Context: 1_048_576, MaxOutput: 65_536,
			Price:         Pricing{Input: 0.10, Output: 0.40, CachedIn: 0.01},
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true, Seed: true},
			ThinkingStyle: ThinkGeminiBudget, Efforts: effortsGemini25, DefaultEffort: "none",
			Ability: ab(.48, .46, .50, .55, .52, .55, .45, .58), TTFTms: 350, TPS: 250, Verbosity: 0.9,
		},
		{
			ID: "gemini/gemini-2.5-pro", Provider: "gemini", UpstreamID: "gemini-2.5-pro", Display: "Gemini 2.5 Pro",
			Tier: Terra, Context: 1_048_576, MaxOutput: 65_536,
			Price:         Pricing{Input: 1.25, Output: 10, CachedIn: 0.125, LongThreshold: 200_000, LongInMult: 2, LongOutMult: 1.5},
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true, Seed: true},
			ThinkingStyle: ThinkGeminiBudget, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium",
			Ability: ab(.78, .76, .80, .82, .76, .76, .72, .82), TTFTms: 2000, TPS: 90,
		},
		{
			ID: "gemini/gemini-2.5-flash", Provider: "gemini", UpstreamID: "gemini-2.5-flash", Display: "Gemini 2.5 Flash",
			Tier: Luna, Context: 1_048_576, MaxOutput: 65_536,
			Price:         Pricing{Input: 0.30, Output: 2.50, CachedIn: 0.03},
			Caps:          Caps{Tools: true, ForcedToolChoice: true, Vision: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true, Seed: true},
			ThinkingStyle: ThinkGeminiBudget, Efforts: effortsGemini25, DefaultEffort: "medium",
			Ability: ab(.62, .60, .66, .68, .62, .64, .58, .70), TTFTms: 700, TPS: 180,
		},
	}
}
