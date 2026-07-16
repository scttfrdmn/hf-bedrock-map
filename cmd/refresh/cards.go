package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AWS publishes a per-model "model card" doc page for every Bedrock model. Each
// card's "End User License Agreements and Terms of Use" link is an authoritative
// provenance pointer: for many open-weight models it links directly to the
// Hugging Face repo AWS serves (exact variant included), and for others it links
// to the provider's own license (github.com/meta-llama, llama.com, nvidia.com,
// ai.google.dev/gemma, …), which still identifies the upstream model. Closed
// models point at the generic AWS third-party-models legal page.
//
// This is the ground truth the AWS *API* does not expose, so we scrape it.
const (
	cardsIndexURL = "https://docs.aws.amazon.com/bedrock/latest/userguide/model-cards.html"
	cardBaseURL   = "https://docs.aws.amazon.com/bedrock/latest/userguide/"

	cardsConcurrency = 12
)

// cardInfo is what we extract from one model card.
type cardInfo struct {
	Slug     string   // e.g. model-card-qwen-qwen3-32b.html
	EULA     string   // "End User License Agreements" href, if any
	ModelIDs []string // Bedrock modelId strings mentioned on the card
}

var (
	// Matches the doc filenames in the index and in cross-links.
	cardSlugRE = regexp.MustCompile(`model-card-[a-z0-9-]+\.html`)

	// Matches Bedrock modelId tokens: <provider>.<rest>. The provider set is
	// closed (these are the only providers Bedrock lists); this keeps stray
	// tokens like "qwen.png" from being mistaken for ids (they get filtered
	// again by interswith the authoritative API list downstream).
	modelIDRE = regexp.MustCompile(`(?:ai21|amazon|anthropic|cohere|deepseek|google|luma|meta|minimax|mistral|moonshot|moonshotai|nvidia|openai|qwen|stability|twelvelabs|writer|zai)\.[a-z0-9][a-z0-9._:-]*`)

	// The EULA anchor: "End User License Agreements ... <a href="URL"". The
	// (?s) flag lets .*? span the markup between the label and the anchor.
	eulaRE = regexp.MustCompile(`(?s)End User License Agreements.*?href="([^"]+)"`)
)

// fetchModelCards scrapes the card index and every linked card, returning a map
// from Bedrock modelId -> its card. A modelId may be keyed under several forms;
// callers resolve context-window variants via lookupCard.
func fetchModelCards(client *http.Client) (map[string]cardInfo, error) {
	idx, err := httpGetString(client, cardsIndexURL)
	if err != nil {
		return nil, fmt.Errorf("fetch cards index: %w", err)
	}
	slugs := uniqueStrings(cardSlugRE.FindAllString(idx, -1))
	if len(slugs) == 0 {
		return nil, fmt.Errorf("no model-card links found at %s (page structure changed?)", cardsIndexURL)
	}

	cards := make([]cardInfo, len(slugs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, cardsConcurrency)
	var firstErr error
	var errMu sync.Mutex

	for i, slug := range slugs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, slug string) {
			defer wg.Done()
			defer func() { <-sem }()
			html, err := httpGetString(client, cardBaseURL+slug)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("fetch card %s: %w", slug, err)
				}
				errMu.Unlock()
				return
			}
			cards[i] = parseCard(slug, html)
		}(i, slug)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	byID := map[string]cardInfo{}
	for _, c := range cards {
		for _, id := range c.ModelIDs {
			// Prefer a card whose EULA points at Hugging Face when the same
			// id shows up on more than one page.
			if existing, ok := byID[id]; ok && isHFURL(existing.EULA) && !isHFURL(c.EULA) {
				continue
			}
			byID[id] = c
		}
	}
	return byID, nil
}

// parseCard extracts modelIds and the EULA link from one card's HTML.
func parseCard(slug, html string) cardInfo {
	c := cardInfo{Slug: slug}
	if m := eulaRE.FindStringSubmatch(html); m != nil {
		c.EULA = strings.TrimSpace(m[1])
	}
	c.ModelIDs = uniqueStrings(modelIDRE.FindAllString(html, -1))
	return c
}

// lookupCard finds the card for a modelId, tolerating trailing context-window
// segments (e.g. "...-v1:0:128k" falls back to "...-v1:0").
func lookupCard(byID map[string]cardInfo, modelID string) (cardInfo, bool) {
	if c, ok := byID[modelID]; ok {
		return c, true
	}
	id := modelID
	for {
		i := strings.LastIndex(id, ":")
		if i < 0 {
			break
		}
		id = id[:i]
		if c, ok := byID[id]; ok {
			return c, true
		}
	}
	return cardInfo{}, false
}

func httpGetString(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func newDocsClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func isHFURL(url string) bool {
	return strings.Contains(url, "huggingface.co/")
}
