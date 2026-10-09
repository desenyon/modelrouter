# modelrouter

**An OpenAI-compatible gateway that sends every request to the model and reasoning effort with the best expected value, across OpenAI, Anthropic and Google Gemini.**

Send `model: "auto"`. For each request the router estimates how hard it is and which skills it needs. It then predicts each candidate model's chance of success and picks the arm that maximizes

```
U = value × P(success) − λ·cost − μ·latency      subject to hard constraints and P ≥ quality floor
```

Each candidate arm is a (model, reasoning-effort) pair. Decisions run locally after the embedder is loaded, with no extra LLM call and no network on the decision path. Native adapters handle OpenAI Responses, Anthropic Messages, and Gemini streams; the client-facing API remains Chat Completions.

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

Requires **Go 1.26 or newer**, outbound HTTPS for the first embedder download, and at least one provider key for generation. No Python runtime, database, GPU, or external embedding service is required.

```sh
go install github.com/desenyon/modelrouter@latest
export OPENAI_API_KEY="your-provider-key"   # any subset of providers works
export MODELROUTER_LISTEN="127.0.0.1:8787"
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

Use an OpenAI Chat Completions SDK with `base_url` set to `http://127.0.0.1:8787/v1`. The gateway implements a subset of that API, listed under [compatibility](#compatibility-and-limitations); it does not expose `/v1/responses`.

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="local")
response = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Explain this race condition"}],
    extra_body={"router": {"mode": "quality", "max_cost_usd": 0.05}},
)
print(response.choices[0].message.content)
```

`local` is only a placeholder when gateway authentication is disabled. Set `MODELROUTER_API_KEY` (or `api_keys` in YAML) and use that key in clients when enabling authentication. **The default listen address is `:8787` and the default key list is empty**; set the loopback address as above for local use. Provider credentials stay on the gateway. Deployments exposed to other machines should use a TLS reverse proxy and configure client/admin authentication.

To build the checked-out source:

```sh
git clone https://github.com/desenyon/modelrouter.git
cd modelrouter
go build -o modelrouter .
./modelrouter version
./modelrouter -c config.example.yaml models
```

To provision the embedder separately (for an offline runtime):

```sh
export MODELROUTER_EMBEDDER_DIR="$PWD/.cache/embedder"
./modelrouter embedder fetch
./modelrouter embedder status
```

Set `embedder.auto_download: false` and point `embedder.dir` or `MODELROUTER_EMBEDDER_DIR` at the provisioned directory. Startup verifies the pinned files' SHA-256 digests and fails if required artifacts are missing or corrupt. A route preview needs the embedder but makes no generation call.


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

**Per-request controls.** Set fields inside the JSON `router` object. Headers override their numeric/mode/session equivalents; allow/deny headers append to body lists.

| Body field | Header | Meaning |
|---|---|---|
| `mode` | `X-Modelrouter-Mode` | `cost`, `balance`, `quality`, `fast`; overrides the model's mode suffix |
| `max_cost_usd` | `X-Modelrouter-Max-Cost` | Hard ceiling on **estimated cost of each candidate attempt**; 0 disables it |
| `max_latency_ms` | `X-Modelrouter-Max-Latency` | Soft estimated latency ceiling; may relax when no arm fits |
| `min_quality` | `X-Modelrouter-Min-Quality` | Probability floor in [0,1]; 0 uses the mode default |
| `allow`, `deny` | `X-Modelrouter-Allow`, `X-Modelrouter-Deny` | Arrays in JSON, comma-separated values in headers; model IDs, aliases, providers, or tiers; deny wins |
| `session_id` | `X-Modelrouter-Session` | Explicit conversation affinity/cache partition |
| `explain` | `X-Modelrouter-Explain: true` | Adds `modelrouter` decision data to non-stream responses, including cache replays |
| `no_cache` | `Cache-Control: no-cache` or `no-store` | Bypass exact cache and in-flight sharing |
| `force_cache` | `X-Modelrouter-Cache: force` | Opt in despite potentially stochastic generation; `no_cache` wins |

Cost caps apply to the chosen arm **and all fallbacks**, including pinned models and relaxed plans. If none fit, chat returns 503 without calling an upstream (route preview returns 400). Uncatalogued passthrough models have unknown prices and reject a positive cost cap with 400; add a priced catalog entry first. Estimates are not billing guarantees: retries/hedges are separate attempts, and actual tokens, provider audio pricing, cache use, and hidden reasoning can differ. `budget.usd_per_hour` is an adaptive spend target, not a hard payment limit.

Example combining a provider restriction, cap, and explanation:

```sh
curl -sS localhost:8787/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{
    "model": "auto", "temperature": 0,
    "messages": [{"role": "user", "content": "Summarize the following meeting notes: ..."}],
    "router": {"allow": ["openai", "gemini"], "max_cost_usd": 0.01, "explain": true}
  }'
```

`max_completion_tokens` includes reasoning tokens and takes precedence over `max_tokens`. `max_tokens` requests a visible-output limit; the dispatcher adds a bounded reasoning allowance and clamps to the model output limit. These two forms have different cache identities. Omitted/zero limits use the router default (32,768, clamped by the model).

## How it decides

**1. Features (single pass).**
- Token estimate and attachments.
- Tool count and schema size; forced `tool_choice`; whether the request is inside an agent tool loop.
- Code volume, stack traces, diffs and math density.
- Count of constraint-style lines; explicit length requests ("2000 words").
- JSON schema complexity.

**2. Embedder (required).** [Model2Vec `potion-base-8M`](https://huggingface.co/minishlab/potion-base-8M), run in pure Go.
- WordPiece tokenizer, bit-exact with HuggingFace on the golden tests.
- Encoding is local; no service or subprocess per request.
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
- **Hard constraints:** context window, tools, vision, remote images, PDFs, audio adapter capability, JSON schema, credentials, tier/provider pins, and expected cost caps. Breakers are checked while planning and again immediately before every dispatch, including an explicit model pin.
- **Soft constraints** (relaxed only when nothing else fits): forced tool choice on models that reject it, prefill, sampling parameters.
- **Costing:** catalog prices at request time, including configured promotions and long-context surcharges. Cached input is priced as cheaper when the conversation's prefix is still warm on that model, so staying on the same model emerges from cost rather than a rule. A switching penalty and downgrade hysteresis protect coherence in agent loops.
- **Fallbacks:** prefer a different provider when a sufficiently capable eligible alternative exists; single-provider configurations and constraints can prevent that. Soft alternatives can fill a short fallback list, but cannot bypass a cost cap.

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
  - Client disconnects and canceled hedge losers never count against a model. A canceled half-open attempt releases its probe, allowing a later request to test recovery. First-token, idle, and total-deadline expiry are timeout failures.
- **Retry budget** (token bucket) so retries can't amplify a provider outage.
- **Hedging (optional):** races a second provider when the first is slow to produce its first token. Both starts count toward `max_attempts`; a limit of one disables hedging for that request. Losing attempts are canceled and their streams/probes released. Providers may still bill work already performed by a canceled loser.
- **Quality escalation (non-stream):** empty, refused, invalid-JSON or invalid-tool-argument outputs are retried on the strongest remaining arm. JSON wrapped in Markdown fences is repaired in place.
- **Exact cache:** bounded in-memory LRU + TTL, eligible for temperature 0, a fixed seed, or explicit force. Canonical generation parameters, inclusive-token semantics, user/session identity, and routing constraints enter the key. Stream presentation and explanations do not. Non-streaming identical in-flight requests are deduplicated; each follower can cancel or time out independently. The leader owns the upstream context; a failed leader allows still-active followers to retry independently. Streaming misses are not shared or populated, but can replay a previously cached non-stream response.
- **Replay accounting:** cache hits/shared answers get fresh feedback IDs, request/latency metrics, explanations, and decision-log entries. They report zero new upstream spend and do not add upstream token/attempt metrics or refresh an upstream prompt-cache timestamp. Explicit feedback works; replays do not create implicit regeneration signals.
- **Shutdown:** the CLI drains HTTP requests for up to 30 seconds and persists learning state. Decision-log closure drains queued records, tolerates concurrent writers/repeated closure, and reports encoding/flush errors in the shutdown log.

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

## Predictor evaluation

The original v3 evaluation below is a historical baseline, not a new benchmark or live provider measurement. Re-run `modelrouter eval` on your checkout to compare. It scores the predictor on 53 held-out prompts it has never seen, then simulates routing against their true labels:

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
| POST | `/v1/chat/completions` | Routed chat (streaming, tools, vision, PDFs, Gemini audio input, JSON schema); `/chat/completions` is an alias |
| POST | `/v1/route` | Decision preview without calling an upstream; accepts a chat body or `{"prompt": "..."}` |
| POST | `/v1/feedback` | Outcome feedback for a request id |
| GET | `/v1/models` | Virtual models + catalog models with configured credentials |
| GET | `/healthz`, `/readyz` | Liveness / readiness; readiness is 503 when no configured, enabled catalog model exists |
| GET | `/metrics` | Prometheus: requests, cost, tokens, attempts, TTFT, routing latency, breakers, λ |
| GET | `/admin/state` | Breakers, budget, learned offsets, cache stats |

Requests must contain one JSON value, at most 64 MiB for chat/route. Invalid numbers, negative limits, unsupported effort/format/tool kinds, malformed audio, and forced tool choices without a matching function return 400. Unknown top-level SDK fields remain tolerated rather than forwarded wholesale. Unsupported hosted tools are rejected instead of silently dropped.

Errors use `{"error":{"message":"...","type":"..."}}`. Authentication failures are 401; upstream rate limits are 429 (with `Retry-After` when available); upstream deadlines are 504; client cancellation uses 499; other upstream failures normally use 502. Once SSE is committed, a later error is delivered inside the stream and no replacement generation is appended.

Streaming example:

```sh
curl -N localhost:8787/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "auto", "stream": true, "stream_options": {"include_usage": true},
  "messages": [{"role": "user", "content": "Explain how a circuit breaker recovers"}]
}'
```

Feedback uses the `X-Modelrouter-Request-Id` header or the response ID (`chatcmpl-` prefix accepted):

```sh
curl localhost:8787/v1/feedback -H 'Content-Type: application/json' \
  -d '{"request_id":"REPLACE_WITH_REQUEST_ID","rating":"good"}'
```

The feedback API accepts `rating: good|bad` or `score` in [0,1]. It returns 409 when learning is disabled and 404 for unknown/expired IDs. Cached answers receive distinct feedback IDs as well.

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

## Configuration and operation

[`config.example.yaml`](config.example.yaml) contains defaults and an extra-provider example. Load it with `modelrouter -c config.yaml serve`. YAML supports `${ENV_VAR}` expansion; dedicated environment variables override YAML. Changes take effect on restart; there is no live reload.

| Variables | Configuration |
|---|---|
| `MODELROUTER_LISTEN` | Bind address |
| `MODELROUTER_API_KEYS` (comma-separated) / `MODELROUTER_API_KEY` | Gateway client keys; plural takes precedence |
| `MODELROUTER_ADMIN_KEY` | Separate admin key |
| `MODELROUTER_MODE` | Default routing objective |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | OpenAI credentials/Responses endpoint root |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL` | Anthropic credentials/endpoint root |
| `GEMINI_API_KEY` (or `GOOGLE_API_KEY`), `GEMINI_BASE_URL` | Gemini credentials/endpoint root |
| `MODELROUTER_EMBEDDER_DIR` | Verified local model artifacts |
| `MODELROUTER_STATE_PATH` | Learned state JSON file |
| `MODELROUTER_DECISION_LOG` | Optional append-only JSONL decision log |
| `MODELROUTER_BUDGET_USD_PER_HOUR` | Adaptive spend target |

`/v1/*` uses client keys. `/admin/state` uses the admin key when set, otherwise client authentication. `/metrics` is public by default; `telemetry.metrics_auth: true` applies admin authentication. `/healthz` and `/readyz` are public probes. Readiness counts configured, enabled catalog models; it does not make upstream calls or evaluate current breaker state. Logs and admin data can contain upstream error messages and routing metadata; control access to them.

Native providers with no key (or `disabled: true`) are excluded. Extra OpenAI-compatible providers are configured under `providers.extra`; add matching entries under `models` with ID, provider, upstream ID, tier, context, and pricing. `caps` overrides replace the complete capability set, so include all capabilities you need. Audio input currently requires native Gemini and `caps.audio: true`; setting the flag on another adapter does not add serialization support. The built-in catalog is a snapshot dated `2026-10-02`, not a live price feed. `doctor` checks current availability; `doctor --probe` also sends billable requests. It is not part of automated verification.

Default state lives in the OS user configuration directory under `modelrouter/learned.json`; embedder artifacts live in the OS user cache directory under `modelrouter/potion-base-8M`. Cache entries, sessions, breaker windows, retry tokens, budget history, and recent feedback IDs are process-local. Persist the learned-state file and provision artifacts for restarts; multiple gateway replicas do not share these controls. Decision logs use a bounded queue and drop records on overflow rather than block requests, so they are diagnostics rather than a billing ledger.

### Package boundaries

| Package | Responsibility |
|---|---|
| `internal/api/openai`, `internal/canon` | Wire translation and common request validation |
| `internal/features`, `internal/embed`, `internal/predict` | Structural features, local Model2Vec, kNN/ridge prediction; predictor consumes a small concurrent embedder interface |
| `internal/catalog`, `internal/optimize`, `internal/router` | Immutable catalog data, constrained expected-value planning, session/learning orchestration |
| `internal/provider/*`, `internal/dispatch`, `internal/health` | Native protocols, streaming attempt lifecycle, timeout/failover/hedging and recovery |
| `internal/cache`, `internal/session`, `internal/learn`, `internal/control` | Bounded local caches/affinity, feedback updates, spend and retry control |
| `internal/gateway`, `internal/telemetry` | HTTP/auth, one finalization path for routed requests, metrics and asynchronous logs |

Provider adapters own wire formats; dispatch owns attempt lifetimes; the gateway owns request accounting. The production embedder remains required. The small embedder interface permits synthetic **test-only** vectors so HTTP/lifecycle regression tests do not depend on downloads; quality assertions continue to use the real pinned model.

## Verification and development

The repository has no separate type-checker: `go vet`, compilation, and Go tests provide static and type checks. No extra lint dependency is required.

```sh
./scripts/verify.sh
```

This checks formatting, runs `go vet`, runs the full race-enabled suite, builds the binary in a temporary directory, and smoke-tests `version`/`models`. By default it uses two workers; override `GOMAXPROCS` and `GO_TEST_PARALLEL` as needed. Local HTTP fixtures require permission to bind loopback ports. There are no live provider calls.

For full embedding, prediction-quality, learning, and legacy gateway coverage, provision artifacts and **require** them:

```sh
go run . embedder fetch
MODELROUTER_REQUIRE_EMBEDDER=1 ./scripts/verify.sh
```

All model-dependent test loaders honor `MODELROUTER_EMBEDDER_DIR`; without required mode they explicitly skip if artifacts are unavailable. Required mode makes missing/corrupt artifacts fail the tests and adds compiled `embedder status`/`route --json hi` smoke checks. The [CI workflow](.github/workflows/ci.yml) downloads the checksum-pinned artifacts and runs required mode on pushes and pull requests. Provider credentials are not needed. The optional hyperparameter sweep remains excluded unless `SWEEP=1`.

For fast, fully offline regressions:

```sh
go test -race ./internal/cache ./internal/api/openai ./internal/optimize ./internal/dispatch ./internal/health ./internal/telemetry
go test -race ./internal/gateway -run Offline -count=1
```

The offline gateway tests use real request decoding, prediction, optimization, cache, dispatch, metrics, feedback, and logging with synthetic vectors and a local fake provider. Adapter tests use HTTP/SSE fixtures to check native translation and stream assembly. These tests establish protocol/lifecycle behavior, not current provider availability or model quality.

```sh
go run . eval                                  # actual pinned predictor + simulated routing
go test ./internal/eval -run Sweep -v           # skips unless SWEEP=1
SWEEP=1 go test ./internal/eval -run Sweep -v    # optional expensive tuning grid
```

### Migration

From the v3 optimizer: configuration shape and virtual models are retained. Requests that relied on an over-budget fallback now fail when no arm fits; uncatalogued models with a positive cost cap require a catalog/pricing entry. Fix malformed controls instead of relying on silent coercion or dropped tools. Invalid numeric headers now return 400. Audio input is routed only to native Gemini; explicitly pinned unsupported adapters fail before generation. Cache identities now separate inclusive token limits, latency caps, sessions, and users; the cache is memory-only and starts empty on restart.

Metrics now count all successful cache/shared requests (`outcome="cache_hit"` / `"cache_shared"`) while upstream token counters count all attempts, including retries, exactly once. Update dashboards that previously assumed one upstream attempt per request. Total deadlines return 504 rather than 499. Cached feedback IDs and cached non-stream explanations are now available. Feedback state remains in the existing persisted format; no migration command is required.

From v2: v3 was a breaking architectural rewrite. Replace old tier/upstream configuration with native `providers`, `models`, and `router` objectives from the example YAML; use Go 1.26+, provision the required embedder, and update clients to `auto`, the four current tiers, or explicit provider IDs. Do not reuse v2 tuning values as v3 ability priors.

### Compatibility and limitations

- Supports Chat Completions text, function tools/results, images, inline documents, JSON modes, and streaming. Audio input (`input_audio` with base64 `data`, `format: wav|mp3`) is currently native Gemini only. Audio-output generation, hosted tools, `file_id` references, and multiple choices (`n > 1`) are unsupported. The compatibility adapter handles text/images/tools; it does not serialize documents or audio.
- Quality, latency, input tokens, reasoning tokens, and audio costs are estimates. The quality floor relaxes to the best achievable probability if no eligible arm meets it. Output checking validates JSON syntax/tool-argument syntax, not arbitrary JSON Schema semantics or factual correctness.
- `temperature: 0` or a seed is cache eligibility, not a cross-provider determinism guarantee. Some models/adapters omit unsupported sampling/seed controls. Force-caching is an explicit opt-in to replay a potentially stochastic result.
- First-token retries can occur before client bytes; committed streams cannot transparently switch providers. Canceled hedge losers may have incurred cost that was never reported in upstream usage. Metrics cannot reconstruct that bill.
- Cache sharing is process-local and uses the leader's lifetime. A leader cancellation can cause active followers to retry. There is no distributed singleflight, global budget, durable request queue, or per-client quota system.
- Static embeddings capture topic better than difficulty; hand-set ability priors and historical simulation results are not live quality guarantees. The existing catalog must be maintained as provider IDs, prices, capabilities, and availability change.
- A positive cost cap constrains each candidate's estimated cost, not aggregate retry/hedge spend or a provider invoice. Use a provider-side account limit if a strict monetary boundary is required.

### Troubleshooting

| Symptom | Check |
|---|---|
| Startup says embedder missing/checksum mismatch | Run `embedder fetch`/`status` with the same directory and filesystem permissions as the server |
| `/readyz` returns 503 or routing has no eligible model | Check provider keys, disabled models, caps, allow/deny/pins, cost ceiling, and `/admin/state` cooldowns |
| 400 after upgrading | Check numeric ranges, named tool membership, supported tool/format types, and uncatalogued-model cost caps |
| Frequent 429 or 504 | Inspect attempt metrics/decision logs, upstream rate limits, reasoning effort and configured timeouts before increasing retry traffic |
| Tests appear green but report skips | Fetch the embedder and set `MODELROUTER_REQUIRE_EMBEDDER=1`; use `go test -v` to inspect individual cases |

## License

[MIT](LICENSE). The embedder (potion-base-8M) is MIT licensed by Minish Lab.
