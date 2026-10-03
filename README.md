# modelrouter

**An OpenAI-compatible gateway that sends every request to the model and reasoning effort with the best expected value, across OpenAI, Anthropic and Google Gemini.**

Send `model: "auto"`. For each request the router estimates how hard it is and which skills it needs. It then predicts each candidate model's chance of success and picks the arm that maximizes

```
U = value × P(success) − λ·cost − μ·latency      subject to hard constraints and P ≥ quality floor
```

Each candidate arm is a (model, reasoning-effort) pair. The decision is made locally in about 35µs, with no extra LLM call and no network on the decision path.

```
POST /v1/chat/completions {"model":"auto", ...}
        │
        ▼
 features ─► embedder (Model2Vec, pure Go) ─► difficulty δ, skill mix, output length
        │                                         │
        ▼                                         ▼
 constraints (context, tools, vision, keys, health) ─► optimizer over ~45 (model, effort) arms
                                                         │  P = σ(9·(θ_model·skills − δ) + 1)
                                                         ▼
 dispatch: stream-peek failover · effort-scaled timeouts · hedging · quality escalation
        │
        ▼
 learn: feedback + regenerations + invalid outputs ─► ability offsets & new exemplars
```

## Quick start

```sh
go install github.com/desenyon/modelrouter@latest
export OPENAI_API_KEY=... ANTHROPIC_API_KEY=... GEMINI_API_KEY=...   # any subset works
modelrouter serve        # first start downloads the 30 MB embedder (sha256-pinned)
```

```sh
curl localhost:8787/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "auto",
  "messages": [{"role": "user", "content": "Write a Rust lock-free MPMC queue and explain the ABA problem"}]
}' -i
```

```
X-Modelrouter-Model: openai/gpt-6.1-sol
X-Modelrouter-Tier: sol
X-Modelrouter-Effort: medium
X-Modelrouter-Difficulty: 0.827
X-Modelrouter-P-Success: 0.812
X-Modelrouter-Est-Cost: 0.040200
X-Modelrouter-Cost: 0.038114
X-Modelrouter-Routing-Us: 74
```

Any OpenAI SDK works unchanged: point `base_url` at the gateway.

## Choosing models

| `model` | Meaning |
|---|---|
| `auto` | Balance mode: the cheapest model that is likely to succeed, upgraded when the gain in success justifies the extra cost |
| `auto:cost` · `auto:quality` · `auto:fast` | Other objectives (also settable via `router.mode` or the `X-Modelrouter-Mode` header) |
| `luna` · `terra` · `sol` · `astra` | Restrict to one tier; the router still picks the model and effort |
| `openai/auto` · `anthropic/auto` · `gemini/auto` | Restrict to one provider |
| `claude-sonnet-5-5`, `gpt-6.1-sol`, `gemini-3.8-flash`, … | Exactly that model (catalog id, upstream id or alias) |
| any other `gpt-*` / `claude-*` / `gemini-*` id | Passed through to that provider |

**Tiers.** The tier names follow OpenAI's own naming (gpt-6-luna < gpt-5.6-terra < gpt-6.1-sol < gpt-6-astra). Models from the other providers sit in the band they compete in:

| Tier | OpenAI | Anthropic | Gemini |
|---|---|---|---|
| astra | gpt-6-astra | claude-fable-5-1 | — |
| sol | gpt-6.1-sol | claude-opus-5-5 | gemini-3.1-pro-preview |
| terra | gpt-5.6-terra | claude-sonnet-5-5 | gemini-3.8-flash |
| luna | gpt-6-luna | claude-haiku-4-5 | gemini-3.1-flash-lite, gemini-2.5-flash-lite |

Run `modelrouter models` for prices, limits and supported efforts. Run `modelrouter doctor` to check every catalog id against each provider's live model list (`--probe` also sends one tiny request per model).

**Per-request controls.** Set these in the body `router` object or with headers:

- `mode`
- `max_cost_usd`
- `max_latency_ms`
- `min_quality`
- `allow` / `deny` (model ids, providers or tiers)
- `session_id`
- `explain` (adds the full decision to the response)
- `no_cache`

## How it decides

**1. Features (single pass).**
- Token estimate and attachments.
- Tool count and schema size; forced `tool_choice`; whether the request is inside an agent tool loop.
- Code volume, stack traces, diffs and math density.
- Count of constraint-style lines; explicit length requests ("2000 words").
- JSON schema complexity.

**2. Embedder (required).** [Model2Vec `potion-base-8M`](https://huggingface.co/minishlab/potion-base-8M), run in pure Go.
- WordPiece tokenizer, bit-exact with HuggingFace on the golden tests.
- 3.7µs per encode.
- The last user turn dominates the embedding; the system prompt and latest tool result add context.

**3. Predictor.**
- kNN over a labeled bank of ~260 prompts. Each has a difficulty on a shared 0–1 scale, weights over eight skill axes (reasoning, coding, math, knowledge, creative, instruction, agentic, long-context) and an output length.
- Blended with a ridge-regression head. The head adds lexical rarity features: Model2Vec's Zipf weighting makes rare-token vector norms a measure of how technical the vocabulary is.
- Structural features then adjust the estimate.
- The output carries an uncertainty, which risk-averse modes add to the difficulty.

**4. Success model.**
- An item-response-theory curve: P = σ(9·(θ − δ) + 1). θ is the model's ability dotted with the request's skill mix, plus a bonus for higher reasoning effort.
- Every reasoning-effort level is its own arm, priced with an estimate of its hidden reasoning tokens. Choosing the effort is often the biggest saving.

**5. Optimizer.**
- **Hard constraints:** context window, tools, vision, remote images, PDFs, JSON schema, credentials, breaker state, tier/provider pins.
- **Soft constraints** (relaxed only when nothing else fits): forced tool choice on models that reject it, prefill, sampling parameters.
- **Costing:** live prices, including promotional pricing and long-context surcharges. Cached input is priced as cheaper when the conversation's prefix is still warm on that model, so staying on the same model emerges from cost rather than a rule. A switching penalty and downgrade hysteresis protect coherence in agent loops.
- **Fallbacks:** the first fallback is always on a different provider, so a single provider outage can't take out both.

**6. Spend controller.** With `budget.usd_per_hour` set, multiplicative dual ascent adjusts λ so actual spend tracks the target.

## How it executes

- **Native adapters:**
  - OpenAI uses the Responses API, because GPT-6-class models can't call tools over Chat Completions.
  - Anthropic uses the Messages API with adaptive effort, schema sanitizing, prompt-cache markers and server-side refusal fallbacks.
  - Gemini uses `streamGenerateContent`, with thinking levels and budgets. Thought signatures are round-tripped through OpenAI-format tool-call ids.
  - Any OpenAI-compatible endpoint can be added from config.
- **Always streams upstream.** Even for non-streaming clients, every attempt is streamed, so time-to-first-token is always measured.
- **Stream peeking.** No bytes reach the client until the first visible token, so failover stays invisible until then.
- **Timeouts:**
  - First-token timeout scaled by reasoning effort.
  - Idle timeout between events. Thinking and ping events count as activity.
  - Total timeout.
- **Failover order:** a different provider first. A 429 respects `Retry-After` as a cooldown and is not counted as a health failure. A 400 skips other models on the same provider.
- **Circuit breakers:**
  - Trip on failure rate over a time window.
  - Exactly one probe in half-open, with exponential backoff after a failed probe.
  - Client disconnects never count against a model.
- **Retry budget** (token bucket) so retries can't amplify a provider outage.
- **Hedging (optional):** races a second provider when the first is slow to produce its first token.
- **Quality escalation (non-stream):** empty, refused, invalid-JSON or invalid-tool-argument outputs are retried on the strongest remaining arm. JSON wrapped in Markdown fences is repaired in place.
- **Exact cache:** keyed on every generation parameter; deterministic requests only (temperature 0 or a fixed seed). Identical in-flight requests are deduplicated.

## Learning without training data

The built-in exemplar bank is the starting point; the router then learns from use:

- **Signals:**
  - `POST /v1/feedback {"request_id": "...", "rating": "good"|"bad"}` or a `score` from 0 to 1.
  - **Regeneration:** the exact same conversation arriving again is a negative signal for the previous answer.
  - **Continuation:** the user moving on to a new turn is a weak positive.
  - Invalid JSON, refusals and empty outputs are negatives for the model that produced them.
- **What each signal updates:** the model's per-skill ability, by one gradient step of the IRT likelihood (capped at ±0.12), plus a new exemplar near this request labeled with the difficulty the outcome implies.
- **Persistence:** state is saved to `learning.state_path` and reloaded on restart.
- **Exploration (optional):** `router.explore_rate` sends a small share of traffic to the runner-up model, so the router learns about models it would otherwise never try.

## Measured quality

`modelrouter eval` scores the predictor on 53 held-out prompts it has never seen, then simulates routing against their true labels:

| | |
|---|---|
| Difficulty Spearman ρ | **0.84** |
| Difficulty MAE | 0.115 |
| Under-routed (≥0.2 too easy) | 5.7% |
| Balance mode | 98% cheaper than always using the top model; mean success P 0.925 vs 0.978 |
| Quality mode | 85% cheaper; mean success P 0.970 vs 0.978 |
| Decision latency | ~33µs |

Success probabilities come from the ability priors in `internal/catalog/builtin.go`. These priors were set by hand from each model's tier and published benchmarks, so the success figures are simulated, not measured on real traffic. Feedback refines them.

Known weak spot: static embeddings capture topic far better than hardness. A prompt that sits lexically close to an easy exemplar but is actually famous and hard (e.g. "prove the twin prime conjecture" next to "prove there are infinitely many primes") can be under-estimated until feedback corrects it.

## API

| Method | Path | |
|---|---|---|
| POST | `/v1/chat/completions` | Routed chat (streaming, tools, vision, PDFs, JSON schema) |
| POST | `/v1/route` | Decision preview without calling an upstream; accepts a chat body or `{"prompt": "..."}` |
| POST | `/v1/feedback` | Outcome feedback for a request id |
| GET | `/v1/models` | Virtual models + catalog models with configured credentials |
| GET | `/healthz`, `/readyz` | Liveness / readiness |
| GET | `/metrics` | Prometheus: requests, cost, tokens, attempts, TTFT, routing latency, breakers, λ |
| GET | `/admin/state` | Breakers, budget, learned offsets, cache stats |

## CLI

```sh
modelrouter serve [--mode quality]
modelrouter route "prove that every planar graph is 5-colorable"   # full decision table
modelrouter route --model terra --json "..."
modelrouter models
modelrouter doctor [--probe]
modelrouter eval
modelrouter embedder fetch|status
```

Configuration reference: [`config.example.yaml`](config.example.yaml). The catalog's prices, limits and capabilities were checked against provider documentation on 2026-10-02 (`catalog.Snapshot`).

## Development

```sh
go test -race ./...          # tests that need the embedder skip until `modelrouter embedder fetch`
SWEEP=1 go test ./internal/eval -run Sweep -v    # predictor hyperparameter sweep
```

## License

[MIT](LICENSE). The embedder (potion-base-8M) is MIT licensed by Minish Lab.
