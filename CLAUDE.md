# CLAUDE.md

Context for picking this project up as an agent. Read this before making
changes.

## What this is

Scheduled job that enumerates the models **Amazon Bedrock can serve right now**
(native serverless FMs + the Bedrock Marketplace subset of the SageMaker
JumpStart hub), resolves each to a Hugging Face repo id, commits the result to
`docs/mapping.json`, and serves a static search page over it via GitHub Pages.
The purpose is a self-hosting/cost detector: given an HF repo someone is running
on their own GPUs, is Bedrock already serving it? Full design in `README.md`.

**Region scope: US only.** The catalog varies by region, so the tool unions the
four US regions (us-east-1, us-east-2, us-west-1, us-west-2) and records which
serve each model in the per-entry `regions` field. `BEDROCK_REGIONS` (comma/space
separated) overrides the set for forks; empty = US default.

## Layout

- `cmd/refresh/main.go` — entrypoint; enumerates both catalogs, writes mapping.
- `cmd/refresh/cards.go` — scrapes AWS Bedrock model-card doc pages; the card
  "EULA" link is the authoritative provenance pointer for native FMs.
- `cmd/refresh/hf.go` — Hugging Face Hub API client (search + existence check).
  Reads `HF_TOKEN` from env; degrades gracefully without one.
- `cmd/refresh/resolve.go` — native-FM resolver (override → card EULA → closed
  provider → HF-validated search → unresolved) and the pure candidate-selection
  logic.
- `cmd/refresh/native_overrides.json` — embedded curated modelId→HF map for the
  handful of un-derivable cases (Llama 4 expert counts). Override wins over all.
- `docs/index.html` — dependency-free client-side search page.
- `infra/setup.sh` — OIDC provider + read-only IAM role.
- `.github/workflows/refresh.yml` — daily cron + workflow_dispatch.

## Conventions to follow

- Go, Apache 2.0 license (already in `LICENSE`).
- Conventional commits.
- Once this repo exists on GitHub: track outstanding work as GitHub Issues, not
  as a TODO list in this file.
- No local markdown files as the system of record for project state.

## Status

Built, `go vet`-clean, unit-tested (`go test ./...`), and run end-to-end against
a live account. A representative US-union run: 291 Bedrock-servable entries —
confirmed=130, validated=17, ambiguous=8, proprietary=133, unresolved=3 (vs 266
for us-west-2 alone; the union adds ~25 region-specific models). Confirmed/
validated HF ids were spot-checked to return HTTP 200 on huggingface.co.
`docs/mapping.json` is committed by CI; a local `go run` writes to the repo root
(gitignored).

Deployed in AWS account `752123829273` (OIDC role `hf-bedrock-map-refresh`).
The IAM role's read-only permissions are region-agnostic (`Resource: "*"`), so
no per-region policy change is needed.

## Data-shape notes (verified against live AWS)

- `bedrock:ListFoundationModels` is **not paginated**; returns the full catalog.
- `sagemaker:ListHubContents` pages at 100/call via `NextToken`.
- `DescribeHubContent` **throttles readily** — hence `describeConcurrency = 2`
  plus an adaptive retryer in `run()`. This is a daily batch job; don't raise
  concurrency for speed.
- The Bedrock Marketplace subset is identified by the
  `@capability:bedrock_console` search keyword on hub summaries.
- Authoritative native-FM HF ids live in each model card's "End User License
  Agreements" link. HF-search validation is case-**sensitive** on exact-repo
  lookups but the search endpoint is case-insensitive; the resolver reads the
  canonical id back rather than trusting guessed casing.

## Backlog

Tracked as GitHub Issues + milestones, not here — see
<https://github.com/scttfrdmn/hf-bedrock-map/issues>. Open new work as issues
(labels: `data-quality`, `resilience`, `test-coverage`, `api`, `infra`); don't
reintroduce a TODO list in this file.

## Intentional design decisions (context, not bugs)

- **Ambiguous, not guessed.** When multiple real HF variants exist and the
  modelId can't disambiguate, the resolver emits `ambiguous` with candidates in
  `evidence` rather than asserting one — a wrong assert is worse than "unknown"
  for the detector use case. Curation promotes these to `native_overrides`.
- **No dedup across catalogs.** The same HF id can appear as both a native FM
  row and a Marketplace row; both are kept — they're distinct valid Bedrock
  paths to that model.
- **Region union dedups by `bedrockModelId`.** First region to surface a model
  resolves it; later regions only append to its `regions` list, so HF
  resolution and DescribeHubContent run ~1x, not once per region. If AWS ever
  served the same modelId with *different* metadata per region this would take
  the first; not observed in practice.
