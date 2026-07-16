# hf-bedrock-map API

A static, no-backend reverse-lookup API served from GitHub Pages: **given a
Hugging Face repo id, is it already served by Amazon Bedrock (US regions)?**

Intended use: an external app that detects a model being loaded onto a GPU can
check whether Bedrock already serves that model — i.e. whether the self-hosted
GPU workload could be replaced by an on-demand Bedrock call.

- **Base URL:** `https://scttfrdmn.github.io/hf-bedrock-map/api/v1`
- **Scope:** US regions only (`us-east-1`, `us-east-2`, `us-west-1`, `us-west-2`),
  unioned. A model counts as "on Bedrock" if any of these regions serves it.
- **CORS:** `access-control-allow-origin: *` — callable from browsers too.
- **Caching:** GitHub Pages sends `cache-control: max-age=600`. Data refreshes
  at most daily, so caching for minutes to hours is safe.
- **Auth:** none. Everything is a public static file.

## Case sensitivity

HF ids are matched **case-insensitively**: lowercase the id before building a
URL or index key. `Qwen/Qwen3-32B` → `qwen/qwen3-32b`.

## Option A — per-model endpoint (one HTTP GET per model)

```
GET /api/v1/hf/{org}/{repo}.json      # e.g. /api/v1/hf/qwen/qwen3-32b.json
```

- **200** — the repo is served by Bedrock. Body:

  ```json
  {
    "hfId": "Qwen/Qwen3-32B",
    "onBedrock": true,
    "regions": ["us-east-1", "us-east-2", "us-west-1", "us-west-2"],
    "bedrock": [
      { "modelId": "qwen.qwen3-32b-v1:0", "catalog": "foundation-model",
        "confidence": "confirmed", "regions": ["us-east-1","us-east-2","us-west-2"] },
      { "modelId": "huggingface-reasoning-qwen3-32b", "catalog": "marketplace",
        "confidence": "confirmed", "regions": ["us-east-1","us-east-2","us-west-1","us-west-2"] }
    ]
  }
  ```

- **404** — not served by Bedrock in the US regions. Treat 404 as a definitive
  "no" (the file simply doesn't exist).

Best for apps that check a handful of models, or want the simplest possible
"is this one on Bedrock?" call with no client-side data handling.

### curl

```bash
id="Qwen/Qwen3-32B"
slug=$(echo "$id" | tr '[:upper:]' '[:lower:]')
if curl -sf "https://scttfrdmn.github.io/hf-bedrock-map/api/v1/hf/${slug}.json" >/tmp/r.json; then
  echo "on Bedrock:"; cat /tmp/r.json
else
  echo "not on Bedrock"
fi
```

### Python

```python
import requests

BASE = "https://scttfrdmn.github.io/hf-bedrock-map/api/v1"

def on_bedrock(hf_id: str):
    """Return the Bedrock record for an HF repo, or None if not served."""
    r = requests.get(f"{BASE}/hf/{hf_id.lower()}.json", timeout=10)
    if r.status_code == 404:
        return None
    r.raise_for_status()
    return r.json()

rec = on_bedrock("Qwen/Qwen3-32B")
if rec:
    print("Already on Bedrock via:", [b["modelId"] for b in rec["bedrock"]])
```

## Option B — bulk index (download once, look up locally)

```
GET /api/v1/index.json
```

```json
{
  "version": "v1",
  "generatedAt": "2026-07-16T00:25:52Z",
  "regions": ["us-east-1", "us-east-2", "us-west-1", "us-west-2"],
  "count": 132,
  "models": {
    "qwen/qwen3-32b": { "hfId": "Qwen/Qwen3-32B", "onBedrock": true, "regions": [...], "bedrock": [...] },
    "...": { ... }
  }
}
```

Keys are lowercased HF ids. A key's **absence means not served by Bedrock**.
Best for apps that check many models, check frequently, or run offline between
refreshes — download once, cache for the day, look up in memory.

### Python

```python
import requests

idx = requests.get(
    "https://scttfrdmn.github.io/hf-bedrock-map/api/v1/index.json", timeout=15
).json()
models = idx["models"]   # cache this; refresh daily

def on_bedrock(hf_id: str):
    return models.get(hf_id.lower())   # None if not served
```

## Notes

- `confidence` mirrors the values in the main mapping (`confirmed`, `validated`,
  `ambiguous`, `proprietary`, `unresolved`) — see the project README. Note that
  `proprietary`/`unresolved` entries have no HF id, so they never appear in this
  reverse API; only resolvable repos do.
- A single HF repo may map to multiple Bedrock `modelId`s (a native serverless
  FM and a Marketplace entry, and/or context-window variants). All are listed.
- The full forward table (every Bedrock model, including proprietary ones) is at
  [`/mapping.json`](mapping.json).
