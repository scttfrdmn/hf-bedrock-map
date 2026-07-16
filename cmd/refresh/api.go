package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The static reverse-lookup API. GitHub Pages serves these files directly (no
// backend, permissive CORS), so an external app — e.g. one watching which model
// is being loaded onto a GPU — can ask "is HF repo X already served by Bedrock?"
// two ways:
//
//   A) Per-model endpoint: GET api/v1/hf/{org}/{repo}.json
//        -> 200 with details if on Bedrock, 404 if not.
//   B) Bulk index:         GET api/v1/index.json
//        -> the whole reverse map (normalized-hf-id -> details), for apps that
//           check many models or run offline between refreshes.
//
// Both are regenerated on every refresh, keyed by a normalized (lowercased) HF
// id so lookups are case-insensitive.

const apiVersion = "v1"

// apiRecord is the reverse-lookup answer for one HF repo. A single repo can be
// served by more than one Bedrock modelId (a native FM and a Marketplace entry,
// or several context-window variants), so bedrock is a list.
type apiRecord struct {
	HFID      string         `json:"hfId"`      // canonical (original-case) HF id
	OnBedrock bool           `json:"onBedrock"` // always true in a per-model file
	Regions   []string       `json:"regions"`   // union of serving US regions
	Bedrock   []apiBedrockID `json:"bedrock"`   // every Bedrock path to this repo
}

type apiBedrockID struct {
	ModelID    string   `json:"modelId"`
	Catalog    string   `json:"catalog"`
	Confidence string   `json:"confidence"`
	Regions    []string `json:"regions"`
}

// apiIndex is the bulk file: metadata + the full normalized-id -> record map.
type apiIndex struct {
	Version     string               `json:"version"`
	GeneratedAt string               `json:"generatedAt"`
	Regions     []string             `json:"regions"`
	Note        string               `json:"note"`
	Count       int                  `json:"count"`
	Models      map[string]apiRecord `json:"models"` // key = normalized (lowercased) hf id
}

// normalizeHFID lowercases an HF id for case-insensitive lookup. HF treats
// org/repo case-insensitively for resolution, so "Qwen/Qwen3-32B" and
// "qwen/qwen3-32b" must land on the same key.
func normalizeHFID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// writeAPI generates the static reverse-lookup API from the resolved entries,
// rooted at outDir (api/v1/... beneath it).
func writeAPI(m Mapping, outDir string) error {
	apiDir := filepath.Join(outDir, "api", apiVersion)
	// Group entries by normalized HF id. Entries without an HF id
	// (proprietary/unresolved) have nothing to reverse-look-up and are skipped
	// here — they remain discoverable in the full mapping.json.
	grouped := map[string]*apiRecord{}
	for _, e := range m.Entries {
		if e.HFID == "" {
			continue
		}
		key := normalizeHFID(e.HFID)
		rec, ok := grouped[key]
		if !ok {
			rec = &apiRecord{HFID: e.HFID, OnBedrock: true}
			grouped[key] = rec
		}
		rec.Bedrock = append(rec.Bedrock, apiBedrockID{
			ModelID:    e.BedrockModelID,
			Catalog:    e.Catalog,
			Confidence: e.Confidence,
			Regions:    e.Regions,
		})
		rec.Regions = unionSorted(rec.Regions, e.Regions)
	}

	// Stable ordering inside each record for deterministic output/diffs.
	for _, rec := range grouped {
		sort.Slice(rec.Bedrock, func(i, j int) bool {
			return rec.Bedrock[i].ModelID < rec.Bedrock[j].ModelID
		})
	}

	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", apiDir, err)
	}

	// Wipe a stale per-model tree so repos that dropped off Bedrock stop
	// returning 200 on the next refresh.
	hfRoot := filepath.Join(apiDir, "hf")
	if err := os.RemoveAll(hfRoot); err != nil {
		return fmt.Errorf("clean %s: %w", hfRoot, err)
	}

	// B) Bulk index.
	models := make(map[string]apiRecord, len(grouped))
	for k, rec := range grouped {
		models[k] = *rec
	}
	idx := apiIndex{
		Version:     apiVersion,
		GeneratedAt: m.GeneratedAt,
		Regions:     m.Regions,
		Note:        "Reverse Bedrock<->Hugging Face lookup, US regions only. Keys are lowercased HF ids. Absence of a key means not served by Bedrock in these regions.",
		Count:       len(models),
		Models:      models,
	}
	if err := writeJSON(filepath.Join(apiDir, "index.json"), idx); err != nil {
		return err
	}

	// A) Per-model endpoints at api/v1/hf/{org}/{repo}.json (lowercased path).
	for _, rec := range grouped {
		rel := normalizeHFID(rec.HFID)
		// Bare repos (no org) live directly under hf/.
		path := filepath.Join(hfRoot, filepath.FromSlash(rel)+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", rec.HFID, err)
		}
		if err := writeJSON(path, rec); err != nil {
			return err
		}
	}

	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// unionSorted returns the sorted set union of two string slices.
func unionSorted(a, b []string) []string {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		seen[s] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
