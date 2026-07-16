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
a live account in `us-west-2`. A representative run: 266 Bedrock-servable
entries — confirmed=125, validated=17, ambiguous=8, proprietary=114,
unresolved=2. Confirmed/validated HF ids were spot-checked to return HTTP 200 on
huggingface.co. `docs/mapping.json` is committed by CI; a local `go run` writes
to the repo root (gitignored).

## Data-shape notes (verified against live AWS)

- `bedrock:ListFoundationModels` is **not paginated**; returns the full catalog.
- `sagemaker:ListHubContents` pages at 100/call via `NextToken`.
- `DescribeHubContent` **throttles readily** — hence `describeConcurrency = 3`
  plus an adaptive retryer in `run()`. Don't raise concurrency without retesting.
- The Bedrock Marketplace subset is identified by the
  `@capability:bedrock_console` search keyword on hub summaries.
- Authoritative native-FM HF ids live in each model card's "End User License
  Agreements" link. HF-search validation is case-**sensitive** on exact-repo
  lookups but the search endpoint is case-insensitive; the resolver reads the
  canonical id back rather than trusting guessed casing.

## Known scope gaps / decisions to revisit

- `ambiguous` rows (multiple real HF variants, modelId can't disambiguate) are
  flagged, not asserted — per an explicit "map to base family, else flag"
  decision. They're the natural curation backlog: promote to `native_overrides`
  once the served variant is confirmed. See any row's `evidence` for candidates.
- Two Mistral natives (`mistral-large-2402`, `pixtral-large-2502`) are
  `unresolved` — no matching public HF repo (date-stamped repos differ, or
  API-only). Curate if the served checkpoint is confirmed.
- Model-card scraping depends on the AWS doc HTML structure; `cards.go` fails
  loudly if the index yields zero card links (structure changed).
- No dedup when the same HF id resolves from both a native FM row and a
  Marketplace row (intentional — both are valid Bedrock paths to that model).
- Region is a single value per run (`AWS_REGION`); the catalog is largely but
  not entirely region-invariant. Multi-region union is unimplemented.
