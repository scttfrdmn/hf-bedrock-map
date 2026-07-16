// Command refresh enumerates both of Amazon Bedrock's model catalogs — the
// native serverless foundation models (bedrock:ListFoundationModels) and the
// Marketplace/JumpStart hub (sagemaker:ListHubContents/DescribeHubContent) —
// resolves each entry to a Hugging Face repo id where the evidence supports
// it, and writes the result to mapping.json.
//
// See README.md for the confidence-level taxonomy and CLAUDE.md for handoff
// context. The output is intentionally honest about how much each row can be
// trusted: only "confirmed" rows carry an HF id lifted directly from AWS
// metadata; "pattern" rows are regex guesses against native modelId strings
// and must be treated as leads, not facts.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/sagemaker"
	smtypes "github.com/aws/aws-sdk-go-v2/service/sagemaker/types"
)

const (
	// publicHub is the AWS-owned SageMaker hub that backs the Bedrock
	// Marketplace / JumpStart catalog.
	publicHub = "SageMakerPublicHub"

	// describeConcurrency bounds parallel DescribeHubContent calls. The
	// SageMaker DescribeHubContent limit is low and throttles readily. This is
	// a daily batch job with no wall-clock pressure, so we keep the pool small
	// (gentle on the API) and lean on the adaptive retryer in run() to absorb
	// any residual throttling.
	describeConcurrency = 2

	// listPageSize is the ListHubContents page size (100 is the API max).
	listPageSize = 100
)

// Confidence levels — see the table in README.md.
const (
	// confConfirmed: HF repo id lifted directly from an authoritative AWS
	// source — a JumpStart HubContentDocument.Url, a model-card EULA link
	// that points at huggingface.co, or a hand-verified curated override.
	confConfirmed = "confirmed"
	// confValidated: no direct AWS link, but a candidate HF repo under the
	// provider's own HF org was confirmed to exist via the HF API.
	confValidated = "validated"
	// confAmbiguous: the upstream model is open-weight and on HF, but several
	// real variants exist and the Bedrock modelId can't disambiguate which one
	// is served. Candidates are recorded in Evidence for curation.
	confAmbiguous = "ambiguous"
	// confProprietary: closed-weight provider; no HF equivalent by design.
	confProprietary = "proprietary"
	// confUnresolved: on Bedrock, but no HF id determinable.
	confUnresolved = "unresolved"
)

// Entry is one row of the published mapping: a single Bedrock catalog entry
// and whatever HF id we could (or could not) resolve for it.
type Entry struct {
	BedrockModelID string `json:"bedrockModelId"`
	Catalog        string `json:"catalog"` // "foundation-model" | "marketplace"
	Provider       string `json:"provider,omitempty"`
	ModelName      string `json:"modelName,omitempty"`
	HFID           string `json:"hfId,omitempty"`
	HFURL          string `json:"hfUrl,omitempty"`
	Confidence     string `json:"confidence"`
	// Regions lists the queried regions that serve this model, sorted. A model
	// counts as "on Bedrock" if any region serves it; the catalog varies by
	// region (e.g. us-east-1 carries models absent from us-west-2).
	Regions []string `json:"regions"`
	// Evidence records where HFID/Confidence came from, so a human can audit
	// any row without re-running the tool.
	Evidence string `json:"evidence,omitempty"`
}

// Mapping is the top-level shape of mapping.json.
type Mapping struct {
	GeneratedAt string         `json:"generatedAt"`
	Regions     []string       `json:"regions"`
	Counts      map[string]int `json:"counts"`
	Entries     []Entry        `json:"entries"`
}

// defaultRegions is the US Bedrock region set unioned when BEDROCK_REGIONS is
// unset. See the region-variance note in README.md: no single region carries
// the full catalog.
var defaultRegions = []string{"us-east-1", "us-east-2", "us-west-1", "us-west-2"}

// parseRegions reads a comma/space-separated region list from BEDROCK_REGIONS,
// falling back to defaultRegions. A single-element list reproduces the old
// single-region behavior.
func parseRegions(env string) []string {
	if strings.TrimSpace(env) == "" {
		return defaultRegions
	}
	var out []string
	for _, r := range strings.FieldsFunc(env, func(c rune) bool { return c == ',' || c == ' ' }) {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return defaultRegions
	}
	return out
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("refresh: %v", err)
	}
}

func run() error {
	ctx := context.Background()

	// This deployment unions the US Bedrock regions only. The catalog varies
	// by region and no single region is a superset, so a US-focused
	// self-hosting detector must union across them. Override with
	// BEDROCK_REGIONS if you need a different set.
	regions := parseRegions(os.Getenv("BEDROCK_REGIONS"))
	log.Printf("unioning Bedrock catalog across US regions: %s", strings.Join(regions, ", "))

	// Native FMs carry no HF link in the AWS API, so resolution leans on two
	// external sources: the AWS model-card doc pages (authoritative EULA links)
	// and the Hugging Face Hub API (existence validation within a provider org).
	// Both are region-independent, so resolve once and reuse across regions.
	hf := newHFClient()
	if hf.hasToken() {
		log.Printf("HF_TOKEN present: gated-org repos will validate")
	} else {
		log.Printf("no HF_TOKEN: gated-org validation limited to search endpoint")
	}
	log.Printf("scraping AWS Bedrock model cards for provenance links...")
	cards, err := fetchModelCards(newDocsClient())
	if err != nil {
		return fmt.Errorf("fetch model cards: %w", err)
	}
	log.Printf("  %d modelIds mapped to a model card", len(cards))
	res, err := newResolver(cards, hf)
	if err != nil {
		return err
	}

	// byID accumulates the union across regions, keyed by bedrockModelId. The
	// first region to surface a model resolves + records it; later regions just
	// append themselves to its Regions list. This keeps HF resolution and the
	// throttle-prone DescribeHubContent calls at ~1x, not Nx.
	byID := map[string]*Entry{}

	// A single region blipping (throttle, transient outage) shouldn't sink the
	// whole daily union. Log-and-continue per region, but track which succeeded
	// so the output reports honest coverage — and fail loudly only if *every*
	// region failed (so we never overwrite good data with an empty mapping).
	var okRegions []string
	for _, region := range regions {
		cfg, err := loadRegionConfig(ctx, region)
		if err != nil {
			log.Printf("[%s] WARN skipping region: load AWS config: %v", region, err)
			continue
		}
		bedrockClient := bedrock.NewFromConfig(cfg)
		smClient := sagemaker.NewFromConfig(cfg)

		log.Printf("[%s] collecting native foundation models...", region)
		fmEntries, err := collectFoundationModels(ctx, bedrockClient, res)
		if err != nil {
			log.Printf("[%s] WARN skipping region: collect foundation models: %v", region, err)
			continue
		}
		log.Printf("[%s] collecting marketplace / JumpStart hub contents...", region)
		mpEntries, err := collectMarketplace(ctx, smClient)
		if err != nil {
			log.Printf("[%s] WARN skipping region: collect marketplace: %v", region, err)
			continue
		}

		added := 0
		for _, e := range append(fmEntries, mpEntries...) {
			if existing, ok := byID[e.BedrockModelID]; ok {
				existing.Regions = append(existing.Regions, region)
				continue
			}
			e.Regions = []string{region}
			ecopy := e
			byID[e.BedrockModelID] = &ecopy
			added++
		}
		okRegions = append(okRegions, region)
		log.Printf("[%s] %d native + %d marketplace; %d new to union (total %d)",
			region, len(fmEntries), len(mpEntries), added, len(byID))
	}

	if len(okRegions) == 0 {
		return fmt.Errorf("all %d region(s) failed; refusing to write an empty mapping", len(regions))
	}
	if len(okRegions) < len(regions) {
		log.Printf("WARN partial coverage: %d of %d regions succeeded (%s); mapping reflects only these",
			len(okRegions), len(regions), strings.Join(okRegions, ", "))
	}

	entries := make([]Entry, 0, len(byID))
	for _, e := range byID {
		sort.Strings(e.Regions)
		entries = append(entries, *e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Catalog != entries[j].Catalog {
			return entries[i].Catalog < entries[j].Catalog
		}
		return entries[i].BedrockModelID < entries[j].BedrockModelID
	})

	counts := map[string]int{
		confConfirmed: 0, confValidated: 0, confAmbiguous: 0,
		confProprietary: 0, confUnresolved: 0,
	}
	for _, e := range entries {
		counts[e.Confidence]++
	}
	counts["total"] = len(entries)

	m := Mapping{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Regions:     okRegions, // honest coverage: only regions actually queried
		Counts:      counts,
		Entries:     entries,
	}

	// OUTPUT_DIR controls where mapping.json and the api/ tree are written.
	// Local dev defaults to the repo root (gitignored); CI sets OUTPUT_DIR=docs
	// to publish directly under GitHub Pages without moving files.
	outDir := os.Getenv("OUTPUT_DIR")
	if outDir == "" {
		outDir = "."
	}

	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mapping: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "mapping.json"), out, 0o644); err != nil {
		return fmt.Errorf("write mapping.json: %w", err)
	}

	if err := writeAPI(m, outDir); err != nil {
		return fmt.Errorf("write api: %w", err)
	}

	log.Printf("wrote %s: %d entries (confirmed=%d validated=%d ambiguous=%d proprietary=%d unresolved=%d) + api/%s",
		filepath.Join(outDir, "mapping.json"), counts["total"], counts[confConfirmed], counts[confValidated],
		counts[confAmbiguous], counts[confProprietary], counts[confUnresolved], apiVersion)
	return nil
}

// loadRegionConfig builds an AWS config pinned to a specific region, with the
// adaptive retryer DescribeHubContent needs (it throttles readily; the retryer
// rate-limits client-side on throttle responses).
func loadRegionConfig(ctx context.Context, region string) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithRetryer(func() aws.Retryer {
			return retry.NewAdaptiveMode(func(o *retry.AdaptiveModeOptions) {
				o.StandardOptions = append(o.StandardOptions, func(so *retry.StandardOptions) {
					so.MaxAttempts = 10
				})
			})
		}),
	)
}

// collectFoundationModels enumerates native serverless FMs. ListFoundationModels
// is not paginated — it returns the full catalog in a single call.
func collectFoundationModels(ctx context.Context, c *bedrock.Client, res *resolver) ([]Entry, error) {
	out, err := c.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(out.ModelSummaries))
	for _, s := range out.ModelSummaries {
		modelID := aws.ToString(s.ModelId)
		provider := aws.ToString(s.ProviderName)
		e := Entry{
			BedrockModelID: modelID,
			Catalog:        "foundation-model",
			Provider:       provider,
			ModelName:      aws.ToString(s.ModelName),
		}
		res.resolveNative(&e, modelID, provider)
		entries = append(entries, e)
	}
	return entries, nil
}

// collectMarketplace enumerates the JumpStart/Marketplace hub. Summaries that
// already carry an "@model-type:proprietary" keyword are classified without a
// DescribeHubContent round-trip; everything else is described to read the HF
// Url out of the content document.
func collectMarketplace(ctx context.Context, c *sagemaker.Client) ([]Entry, error) {
	all, err := listHubContents(ctx, c)
	if err != nil {
		return nil, err
	}

	// The public hub is a superset of Bedrock: it also holds SageMaker
	// JumpStart-only models (catboost, autogluon, classic HF training
	// recipes) that cannot be invoked from Bedrock. This tool only cares
	// about what Bedrock can serve right now, so keep only entries the hub
	// flags as Bedrock-capable.
	var summaries []smtypes.HubContentInfo
	for _, s := range all {
		if hasBedrockCapability(s.HubContentSearchKeywords) {
			summaries = append(summaries, s)
		}
	}
	log.Printf("  %d of %d hub models are Bedrock-servable (@capability:bedrock_console)", len(summaries), len(all))

	entries := make([]Entry, len(summaries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, describeConcurrency)
	var firstErr error
	var errMu sync.Mutex

	for i, s := range summaries {
		name := aws.ToString(s.HubContentName)
		e := Entry{
			BedrockModelID: name,
			Catalog:        "marketplace",
			ModelName:      aws.ToString(s.HubContentDisplayName),
		}

		// Fast path: proprietary flagged right in the summary keywords.
		if hasProprietaryKeyword(s.HubContentSearchKeywords) {
			e.Confidence = confProprietary
			e.Provider = frameworkFromKeywords(s.HubContentSearchKeywords)
			e.Evidence = "keyword @model-type:proprietary"
			entries[i] = e
			continue
		}

		// Slow path: describe to read the document Url.
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, e Entry, name string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := resolveHubContent(ctx, c, &e, name); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("describe %s: %w", name, err)
				}
				errMu.Unlock()
			}
			entries[i] = e
		}(i, e, name)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return entries, nil
}

// listHubContents pages through every Model in the public hub.
func listHubContents(ctx context.Context, c *sagemaker.Client) ([]smtypes.HubContentInfo, error) {
	var all []smtypes.HubContentInfo
	var next *string
	for {
		out, err := c.ListHubContents(ctx, &sagemaker.ListHubContentsInput{
			HubName:        aws.String(publicHub),
			HubContentType: smtypes.HubContentTypeModel,
			MaxResults:     aws.Int32(listPageSize),
			NextToken:      next,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, out.HubContentSummaries...)
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		next = out.NextToken
	}
	return all, nil
}

// resolveHubContent describes a single hub content item and fills in the HF id
// from the content document's Url field.
func resolveHubContent(ctx context.Context, c *sagemaker.Client, e *Entry, name string) error {
	out, err := c.DescribeHubContent(ctx, &sagemaker.DescribeHubContentInput{
		HubName:        aws.String(publicHub),
		HubContentType: smtypes.HubContentTypeModel,
		HubContentName: aws.String(name),
	})
	if err != nil {
		return err
	}

	// The document is JSON serialized into a string.
	var doc struct {
		Provider string `json:"Provider"`
		URL      string `json:"Url"`
	}
	if out.HubContentDocument != nil {
		_ = json.Unmarshal([]byte(*out.HubContentDocument), &doc)
	}
	if doc.Provider != "" {
		e.Provider = doc.Provider
	} else if fw := frameworkFromKeywords(out.HubContentSearchKeywords); fw != "" {
		e.Provider = fw
	}

	if id, ok := hfIDFromURL(doc.URL); ok {
		e.HFID = id
		e.HFURL = "https://huggingface.co/" + id
		e.Confidence = confConfirmed
		e.Evidence = "HubContentDocument.Url"
		return nil
	}

	// No HF url. If keywords say proprietary, honor that; else unresolved.
	if hasProprietaryKeyword(out.HubContentSearchKeywords) {
		e.Confidence = confProprietary
		e.Evidence = "keyword @model-type:proprietary"
		return nil
	}
	e.Confidence = confUnresolved
	if doc.URL != "" {
		e.Evidence = "document Url not a huggingface.co link: " + doc.URL
	} else {
		e.Evidence = "no Url in HubContentDocument"
	}
	return nil
}

// hfIDFromURL extracts an "org/repo" (or bare "repo") id from a huggingface.co
// URL. Returns ok=false for any non-HF url.
func hfIDFromURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	const marker = "huggingface.co/"
	i := strings.Index(raw, marker)
	if i < 0 {
		return "", false
	}
	path := raw[i+len(marker):]
	// Trim query/fragment and any /tree|/blob|/resolve suffix.
	if j := strings.IndexAny(path, "?#"); j >= 0 {
		path = path[:j]
	}
	path = strings.Trim(path, "/")
	for _, sep := range []string{"/tree/", "/blob/", "/resolve/"} {
		if k := strings.Index(path, sep); k >= 0 {
			path = path[:k]
		}
	}
	if path == "" {
		return "", false
	}
	// HF ids are either "repo" or "org/repo"; reject anything deeper as
	// probably not a clean repo id.
	if strings.Count(path, "/") > 1 {
		return "", false
	}
	return path, true
}

func hasProprietaryKeyword(keywords []string) bool {
	for _, k := range keywords {
		if strings.EqualFold(strings.TrimSpace(k), "@model-type:proprietary") {
			return true
		}
	}
	return false
}

// hasBedrockCapability reports whether a hub content item is invocable from
// Bedrock (as opposed to SageMaker JumpStart only). Bedrock Marketplace models
// carry an "@capability:bedrock_console" keyword.
func hasBedrockCapability(keywords []string) bool {
	for _, k := range keywords {
		if strings.EqualFold(strings.TrimSpace(k), "@capability:bedrock_console") {
			return true
		}
	}
	return false
}

// frameworkFromKeywords pulls the "@framework:<x>" value out of the search
// keywords, used as a provider hint when the document lacks one.
func frameworkFromKeywords(keywords []string) string {
	for _, k := range keywords {
		if v, ok := strings.CutPrefix(strings.TrimSpace(k), "@framework:"); ok {
			return v
		}
	}
	return ""
}
