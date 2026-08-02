# ◆ modelrouter

**The intelligent routing gateway for multi-model apps.**

Point any OpenAI-compatible client at modelrouter with `model: "auto"`.
Every request is classified — **Luna** handles the work it can,
**Terra** covers the middle, and **Sol** is used only when the task
actually needs frontier intelligence.

Stop paying Sol prices for Luna jobs.

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![OpenAI Compatible](https://img.shields.io/badge/API-OpenAI%20compatible-10A37F)](https://platform.openai.com/docs/api-reference)
[![License: MIT](https://img.shields.io/badge/license-MIT-04B575)](LICENSE)

```
┌──────────────┐     classify      ┌─────────────┐
│  your app    │ ───────────────▶  │ modelrouter │
│  model=auto  │                   │   gateway   │
└──────────────┘                   └──────┬──────┘
                                          │
                    ┌─────────────────────┼─────────────────────┐
                    ▼                     ▼                     ▼
                 ◆ Luna               ◆ Terra               ◆ Sol
              efficient            balanced             frontier
              (default)           (mid load)         (only if needed)
```

---

## Why

Most traffic does not need your most expensive model. Renames, summaries,
boilerplate, short answers, light refactors — **Luna** is enough.
Reserve **Sol** for architecture, hard debugging, long-horizon agents,
and work where frontier quality actually moves the outcome.

modelrouter is the separate routing plane for your app: a drop-in gateway
that enforces that policy on every request.

## Highlights

- **Luna-first policy** — Sol is suppressed whenever the classifier says Luna fits
- **OpenAI-compatible gateway** — `/v1/chat/completions`, `/v1/models`, streaming
- **Three optimization modes** — `cost` · `balance` · `intelligence`
- **Virtual models** — `auto`, `luna`, `terra`, `sol` (or pass any upstream id through)
- **Route preview** — `modelrouter route "..."` and `POST /v1/route` without spending tokens
- **Observable** — `X-Modelrouter-*` headers, `/metrics`, structured access logs
- **Provider-agnostic** — OpenRouter, OpenAI, or any OpenAI-compatible upstream

## Quick start

```sh
go install github.com/desenyon/modelrouter@latest

export OPENROUTER_API_KEY=sk-or-...
modelrouter serve
```

Then point your client at the gateway:

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "auto",
    "optimize_for": "balance",
    "messages": [{"role":"user","content":"Rename this variable for clarity."}]
  }'
```

Response headers tell you what happened:

```
X-Modelrouter-Tier: luna
X-Modelrouter-Model: openai/gpt-5.6-luna
X-Modelrouter-Mode: balance
X-Modelrouter-Score: 0.120
```

## Routing policy

| Score band | Tier | When |
|------------|------|------|
| low | **Luna** | short prompts, easy markers, formatting, summaries |
| mid | **Terra** | multi-turn, tools, moderate context |
| high | **Sol** | hard markers, huge context, heavy agents — only then |

Modes shift the thresholds without abandoning Luna-first:

| Mode | Behavior |
|------|----------|
| `cost` | Maximum Luna share. Sol only for extreme scores. |
| `balance` | Default. Strong quality, Sol kept rare. |
| `intelligence` | Terra/Sol earlier for hard work — Luna still handles the trivial. |

Pin a tier when you want to override the classifier: `model: "luna" | "terra" | "sol"`.
Pass any concrete upstream id to bypass routing entirely.

## CLI

```sh
modelrouter serve                         # start gateway (default command)
modelrouter serve --mode cost             # Luna-aggressive
modelrouter serve -c config.yaml

modelrouter route "fix this race condition in the scheduler"
modelrouter route --mode cost --json "write a commit message"
modelrouter models                        # virtual models + upstream mappings
modelrouter version
```

## Configuration

Copy [`config.example.yaml`](config.example.yaml) or use env vars:

| Variable | Purpose |
|----------|---------|
| `MODELROUTER_LISTEN` | Bind address (default `:8787`) |
| `MODELROUTER_API_KEY` | Optional key protecting the gateway |
| `MODELROUTER_UPSTREAM_BASE_URL` | OpenAI-compatible base URL |
| `MODELROUTER_UPSTREAM_API_KEY` | Upstream key (also `OPENROUTER_API_KEY` / `OPENAI_API_KEY`) |
| `MODELROUTER_MODE` | `cost` \| `balance` \| `intelligence` |
| `MODELROUTER_LUNA` / `_TERRA` / `_SOL` | Upstream model ids per tier |

```yaml
listen: ":8787"
upstream:
  base_url: "https://openrouter.ai/api/v1"
router:
  default_mode: balance
  luna_max_score: 0.42
  terra_max_score: 0.72
models:
  luna:  "openai/gpt-5.6-luna"
  terra: "openai/gpt-5.6-terra"
  sol:   "openai/gpt-5.6-sol"
```

## HTTP API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Liveness + upstream status |
| `GET` | `/metrics` | Tier mix, latency, Sol share |
| `GET` | `/v1/models` | Virtual model catalog |
| `POST` | `/v1/chat/completions` | Routed chat (streaming supported) |
| `POST` | `/v1/route` | Classification preview only |

Wire it into any SDK:

```ts
const client = new OpenAI({
  baseURL: "http://127.0.0.1:8787/v1",
  apiKey: process.env.MODELROUTER_API_KEY ?? "local",
});

const res = await client.chat.completions.create({
  model: "auto",
  messages: [{ role: "user", content: "Summarize this diff in one sentence." }],
  // @ts-expect-error router extension
  optimize_for: "cost",
});
```

## Architecture

```
client  →  gateway  →  classifier  →  router (luna-first)  →  upstream
                         │                  │
                         └──── score ───────┘
```

This repo is the **routing plane** — a separable part of your stack.
Your product talks to one endpoint; modelrouter decides Luna vs Terra vs Sol.

## Install from source

```sh
git clone https://github.com/desenyon/modelrouter.git
cd modelrouter
go build -o modelrouter .
./modelrouter serve
```

## License

[MIT](LICENSE)
