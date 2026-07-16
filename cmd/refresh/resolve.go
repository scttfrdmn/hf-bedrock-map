package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
)

//go:embed native_overrides.json
var nativeOverridesRaw []byte

// providerOrg maps a Bedrock native provider prefix (the part of a modelId
// before the first ".") to the Hugging Face org that publishes that provider's
// open weights. Used to scope HF search so a candidate can only match within
// the correct org — this is what prevents a qwen3 modelId from resolving to a
// DeepSeek-distill lookalike. Providers absent here are treated as closed.
var providerOrg = map[string]string{
	"qwen":       "Qwen",
	"deepseek":   "deepseek-ai",
	"google":     "google",
	"openai":     "openai",
	"nvidia":     "nvidia",
	"zai":        "zai-org",
	"moonshot":   "moonshotai",
	"moonshotai": "moonshotai",
	"minimax":    "MiniMaxAI",
	"mistral":    "mistralai",
	"meta":       "meta-llama",
}

// override is one curated entry from native_overrides.json.
type override struct {
	HFID string `json:"hfId"`
}

type overridesFile struct {
	Overrides map[string]override `json:"overrides"`
}

// resolver holds the shared state used to classify native FMs: the curated
// overrides, the scraped AWS model cards, and an HF client for validation.
type resolver struct {
	overrides map[string]override
	cards     map[string]cardInfo
	hf        *hfClient
}

func newResolver(cards map[string]cardInfo, hf *hfClient) (*resolver, error) {
	var of overridesFile
	if err := json.Unmarshal(nativeOverridesRaw, &of); err != nil {
		return nil, fmt.Errorf("parse native_overrides.json: %w", err)
	}
	return &resolver{overrides: of.Overrides, cards: cards, hf: hf}, nil
}

// resolveNative classifies a single native serverless FM in place. Resolution
// precedence, most authoritative first:
//
//  1. curated override            -> confirmed (evidence notes curation)
//  2. model-card EULA is an HF url -> confirmed (AWS itself links the repo)
//  3. closed-weight provider       -> proprietary
//  4. HF search within provider org (existence-validated) -> validated,
//     or ambiguous when multiple real variants can't be disambiguated
//  5. otherwise                    -> unresolved
func (r *resolver) resolveNative(e *Entry, modelID, provider string) {
	prefix := providerPrefix(modelID)

	// 1. Curated override (keyed by base modelId, ignoring context-window tail).
	if ov, ok := r.lookupOverride(modelID); ok {
		e.setHF(ov.HFID)
		e.Confidence = confConfirmed
		e.Evidence = "curated override (verified against huggingface.co)"
		return
	}

	// 2. Model card EULA link that points straight at a HF repo.
	card, hasCard := lookupCard(r.cards, modelID)
	if hasCard {
		if id, ok := hfIDFromURL(card.EULA); ok {
			e.setHF(id)
			e.Confidence = confConfirmed
			e.Evidence = "model card EULA link: " + card.EULA
			return
		}
	}

	// 3. Closed-weight provider: no HF equivalent by design.
	if isClosedProvider(provider, prefix) {
		e.Confidence = confProprietary
		e.Evidence = "closed-weight provider"
		if hasCard && card.EULA != "" {
			e.Evidence += " (EULA " + card.EULA + ")"
		}
		return
	}

	// 4. Open-weight provider with no direct HF link on the card: validate a
	// candidate against the provider's HF org.
	org, known := providerOrg[prefix]
	if known {
		if id, conf, ev := r.searchValidate(org, modelID); conf != "" {
			if id != "" {
				e.setHF(id)
			}
			e.Confidence = conf
			e.Evidence = ev
			return
		}
	}

	// 5. Give up honestly.
	e.Confidence = confUnresolved
	if !known {
		e.Evidence = "no known HF org for provider " + prefix
	} else {
		e.Evidence = "no HF repo found under " + org + " matching modelId"
	}
}

// searchValidate looks for the HF repo an open-weight native FM corresponds to,
// scoped to the provider's org. It returns:
//   - (repo, confValidated, evidence) when it finds a confident match,
//   - ("", confAmbiguous, evidence)   when multiple real variants exist and the
//     modelId can't disambiguate them (per the "map to base family, flag
//     ambiguous" policy — no base repo existed to fall back to),
//   - ("", "", "")                    when nothing plausible matched.
func (r *resolver) searchValidate(org, modelID string) (repo, confidence, evidence string) {
	query := searchQuery(modelID)
	ids, err := r.hf.searchAuthor(org, strings.ReplaceAll(query, "-", " "))
	if err != nil {
		log.Printf("  warn: hf search %s/%q failed: %v", org, query, err)
		return "", "", ""
	}
	qtok := tokenSet(query)

	m := selectCandidate(org, qtok, ids)
	switch {
	case m.base != "":
		return m.base, confValidated, fmt.Sprintf("hf-validated: base repo %s exists under %s", m.base, org)
	case m.pick != "" && !m.ambiguous:
		return m.pick, confValidated, fmt.Sprintf("hf-validated: unique match %s under %s", m.pick, org)
	case m.ambiguous:
		return "", confAmbiguous, "multiple HF variants, modelId can't disambiguate: " + strings.Join(m.variants, ", ")
	default:
		return "", "", ""
	}
}

// --- candidate selection (pure, unit-tested) -------------------------------

// quantOrDerivedToken marks repo name tokens that indicate a quantized or
// otherwise-derived artifact rather than the primary released weights.
var quantOrDerivedToken = map[string]bool{
	"fp8": true, "fp16": true, "bf16": true, "nvfp4": true, "int4": true,
	"int8": true, "gptq": true, "awq": true, "gguf": true, "mlx": true,
}

type candidateMatch struct {
	base      string   // repo whose tokens exactly equal the query tokens
	pick      string   // best single non-base match
	ambiguous bool     // multiple distinct primary variants, no base
	variants  []string // the competing repo ids when ambiguous
}

// selectCandidate chooses the HF repo (under org) that best corresponds to a
// query token set. It prefers an exact base-family repo; failing that, a single
// primary (non-quantized) variant; and flags ambiguity when several distinct
// primary variants exist with no base to fall back to.
func selectCandidate(org string, qtok map[string]bool, ids []string) candidateMatch {
	var m candidateMatch
	type cand struct {
		id      string
		extras  map[string]bool // repo tokens not in the query, minus quant tokens
		nExtras int
	}
	var supersets []cand

	for _, id := range ids {
		name := repoName(id)
		rtok := tokenSet(name)
		if !subset(qtok, rtok) {
			continue // every query token must be present — no lookalikes
		}
		extras := map[string]bool{}
		hasQuant := false
		for t := range rtok {
			if qtok[t] {
				continue
			}
			if quantOrDerivedToken[t] {
				hasQuant = true
				continue
			}
			extras[t] = true
		}
		if len(extras) == 0 && !hasQuant {
			// Exact base family repo (Qwen/Qwen3-32B for query "qwen3 32b").
			if m.base == "" || len(id) < len(m.base) {
				m.base = id
			}
		}
		supersets = append(supersets, cand{id: id, extras: extras, nExtras: len(extras)})
	}

	if m.base != "" {
		return m // base family repo wins outright
	}
	if len(supersets) == 0 {
		return m
	}

	// Group the primary (non-quant) candidates by their extra-token signature.
	// Distinct signatures == genuinely different variants (Instruct vs Thinking).
	bySig := map[string]string{} // signature -> shortest repo id
	for _, c := range supersets {
		sig := signature(c.extras)
		if cur, ok := bySig[sig]; !ok || len(c.id) < len(cur) {
			bySig[sig] = c.id
		}
	}
	if len(bySig) == 1 {
		for _, id := range bySig {
			m.pick = id
		}
		return m
	}

	// More than one primary variant and no base repo: ambiguous.
	m.ambiguous = true
	for _, id := range bySig {
		m.variants = append(m.variants, id)
	}
	sort.Strings(m.variants)
	return m
}

func signature(tokens map[string]bool) string {
	keys := make([]string, 0, len(tokens))
	for t := range tokens {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// --- token / string helpers -------------------------------------------------

var tokenSplitRE = regexp.MustCompile(`[^a-z0-9.]+`)

// noiseToken drops Bedrock version cruft and generic words that don't help
// identify the upstream repo.
var noiseToken = map[string]bool{
	"v1": true, "v0": true, "": true,
}

// tokenSet splits a lowercased id/name into a set of comparison tokens.
func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokenSplitRE.Split(strings.ToLower(s), -1) {
		if t == "" || noiseToken[t] {
			continue
		}
		out[t] = true
	}
	return out
}

func subset(a, b map[string]bool) bool {
	for t := range a {
		if !b[t] {
			return false
		}
	}
	return true
}

// searchQuery reduces a modelId to the searchable, provider-stripped core:
// "qwen.qwen3-next-80b-a3b" -> "qwen3-next-80b-a3b".
func searchQuery(modelID string) string {
	rest := modelID
	if i := strings.Index(modelID, "."); i >= 0 {
		rest = modelID[i+1:]
	}
	rest = trimVersionTail(rest)
	return rest
}

var versionTailRE = regexp.MustCompile(`(-v\d+)?(:\d+)?(:\d+k)?$`)

// trimVersionTail strips a trailing Bedrock version/context-window suffix such
// as "-v1:0", ":0", ":128k".
func trimVersionTail(s string) string {
	for {
		trimmed := versionTailRE.ReplaceAllString(s, "")
		if trimmed == s {
			return trimmed
		}
		s = trimmed
	}
}

func providerPrefix(modelID string) string {
	if i := strings.Index(modelID, "."); i >= 0 {
		return modelID[:i]
	}
	return modelID
}

func repoName(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// lookupOverride finds a curated override for a modelId, tolerating a trailing
// context-window segment.
func (r *resolver) lookupOverride(modelID string) (override, bool) {
	if ov, ok := r.overrides[modelID]; ok {
		return ov, true
	}
	id := modelID
	for {
		i := strings.LastIndex(id, ":")
		if i < 0 {
			break
		}
		id = id[:i]
		if ov, ok := r.overrides[id]; ok {
			return ov, true
		}
	}
	return override{}, false
}

// isClosedProvider reports whether a native provider ships closed weights.
// A provider is closed iff we have no known HF org for it.
func isClosedProvider(provider, prefix string) bool {
	_, open := providerOrg[prefix]
	return !open
}

// setHF fills the HF id + canonical URL on an entry.
func (e *Entry) setHF(id string) {
	e.HFID = id
	e.HFURL = "https://huggingface.co/" + id
}
