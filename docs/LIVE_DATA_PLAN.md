# Live data modernization plan

Status: approved automatically from the user's request on 2026-07-29.

## Audit

The current release has a strong terminal UI and a small, readable API layer, but
its live-data contract has drifted:

- OpenRouter moved rankings and benchmark endpoints from
  `/api/frontend/rankings/*` to `/api/frontend/v1/rankings/*`. Every rankings and
  benchmark command currently fails with HTTP 404 when the cache is bypassed.
- The replacement benchmark payload wraps results in `data`, adds a non-row
  `percentilesBySlug` entry, and exposes 26 Design Arena categories instead of
  the eight categories hardcoded in the CLI.
- `/api/v1/models` defaults to text-output models. Passing
  `output_modalities=all` returns the complete catalog, including image, audio,
  speech, transcription, video, embedding, and reranking models.
- The official `/api/v1/providers` endpoint lists every provider but omits the
  policy and capability fields shown on OpenRouter. The current raw
  `/api/frontend/v1/providers` feed exposes those fields for the same slugs.
- Rankings refresh every five minutes in the TUI. Benchmarks, models, and
  providers only refresh on launch or a manual `R`.
- API parsing has no focused tests, so an upstream envelope or mixed-value shape
  change can silently break the app again.

## Product boundary

“All major benchmarks” means every benchmark OpenRouter currently publishes in
its model-comparison surface:

- all Artificial Analysis indexes published by the feed;
- every Design Arena arena/category returned by the feed, discovered
  dynamically rather than hardcoded.

The CLI will not scrape unrelated third-party leaderboards or merge
incomparable self-reported scores. That would create ambiguous model matching,
licensing, and freshness claims that OpenRouter itself does not make.

“All major providers” means the full provider directory returned by OpenRouter,
enriched with public routing capabilities, BYOK support, moderation, prompt
retention/training policy, regions/datacenters, legal links, and status links
where available.

## Implementation

1. Repair the live endpoint contract.
   - Move frontend requests to `/api/frontend/v1`.
   - Accept both `{ "data": ... }` envelopes and raw JSON responses.
   - Keep stale-cache fallback for normal reads, but make `--refresh` strict so
     stale bytes are never presented as a successful live refresh.
   - Reject syntactically valid but semantically empty upstream responses.
2. Expand the catalog and provider schema.
   - Request `output_modalities=all`.
   - Merge `/api/v1/providers` with `/api/frontend/v1/providers` by slug.
   - Let the rich feed own public capability/policy fields while the official
     feed owns official datacenter/legal fields; preserve the official response
     as a graceful fallback when enrichment is unavailable.
3. Make benchmark coverage dynamic.
   - Ignore non-leaderboard metadata in the mixed `aaData` object.
   - Discover and sort all Design Arena categories at runtime.
   - Show source counts and a visible refresh timestamp.
4. Deliver live terminal updates.
   - Refresh rankings, benchmarks, models, and providers every five minutes.
   - Rebuild views without resetting the active tab, filters, or selection.
   - Tag asynchronous refreshes with generations so an older response cannot
     overwrite a newer manual or timed refresh.
   - Keep `R` as an immediate force refresh.
5. Preserve and polish the current visual language.
   - Keep the violet/cyan/green palette, rounded tables, status segments, and
     responsive viewport behavior.
   - Make provider policy signals scannable and benchmark category labels
     explicit (`models · website`, `agents · full stack`, and so on).
   - Label text-token prices separately from non-text upstream billing units.
   - Retain readable narrow-terminal layouts.
6. Verify the whole path.
   - Add API fixture tests for envelopes, raw payloads, mixed benchmark values,
     dynamic categories, and provider merging.
   - Extend TUI tests for all-category rendering and timed refresh.
   - Run formatting, unit tests, race tests, vet, build, and real live commands.
   - Launch the TUI, exercise each affected tab, capture screenshots, and inspect
     them before committing.

## Success criteria

- `modelrouter models --refresh` includes non-text model families.
- `modelrouter rankings --refresh`, `modelrouter benchmarks --refresh`, and
  `modelrouter providers --refresh` succeed against live OpenRouter data.
- Benchmark JSON includes every category present in the current upstream feed.
- Provider output includes all provider slugs from the official directory and
  enriches them when the frontend directory is available.
- The TUI refreshes every live surface on its timer and remains responsive.
- Automated gates pass and inspected screenshots show no clipping, overlap,
  broken hierarchy, or unreadable policy/benchmark labels at desktop and narrow
  terminal sizes.
