package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// hfClient talks to the public Hugging Face Hub API to validate candidate repo
// ids for native Bedrock FMs (which carry no authoritative HF link in AWS
// metadata). A token, if present in HF_TOKEN, is sent as a bearer credential
// so gated-org repos (meta-llama, mistralai) return a usable 200/404 signal
// instead of a blanket 401. The client degrades gracefully: with no token, the
// search endpoint still works for both gated and ungated orgs — only the
// exact-repo existence check loses reliability on gated orgs.
type hfClient struct {
	http  *http.Client
	token string

	mu    sync.Mutex
	cache map[string]hfModel // repoID -> lookup result (nil-value = confirmed-absent)
}

// hfModel is the subset of the HF model record we consume. Id is the canonical
// repo id as HF returns it (correct casing), which we trust over our guessed
// candidate string.
type hfModel struct {
	ID string `json:"id"`
}

func newHFClient() *hfClient {
	return &hfClient{
		http:  &http.Client{Timeout: 15 * time.Second},
		token: os.Getenv("HF_TOKEN"),
		cache: map[string]hfModel{},
	}
}

func (c *hfClient) hasToken() bool { return c.token != "" }

func (c *hfClient) do(rawURL string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.http.Do(req)
}

// searchAuthor returns repo ids under an org matching a free-text query. The
// HF search is case-insensitive and works without auth even for gated orgs.
func (c *hfClient) searchAuthor(org, query string) ([]string, error) {
	q := url.Values{}
	q.Set("author", org)
	q.Set("search", query)
	q.Set("limit", "50")
	resp, err := c.do("https://huggingface.co/api/models?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("hf search %s/%q: status %d: %s", org, query, resp.StatusCode, body)
	}
	var models []hfModel
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// exists reports whether a specific repo id exists, returning the canonical id
// from HF. Treats 200 and 403 (gated-but-real) as existing, 404 as absent.
// Without a token, gated orgs answer 401 for real and fake alike, so a false
// (not-found) result on a gated org is inconclusive rather than authoritative;
// callers should prefer searchAuthor there.
func (c *hfClient) exists(repoID string) (canonical string, ok bool, err error) {
	c.mu.Lock()
	if m, seen := c.cache[repoID]; seen {
		c.mu.Unlock()
		return m.ID, m.ID != "", nil
	}
	c.mu.Unlock()

	resp, err := c.do("https://huggingface.co/api/models/" + repoID)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var m hfModel
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return "", false, err
		}
		if m.ID == "" {
			m.ID = repoID
		}
		c.store(repoID, m)
		return m.ID, true, nil
	case http.StatusForbidden: // gated but real
		m := hfModel{ID: repoID}
		c.store(repoID, m)
		return m.ID, true, nil
	case http.StatusNotFound:
		c.store(repoID, hfModel{})
		return "", false, nil
	default:
		// 401 (untokened gated), 429, 5xx — inconclusive, don't cache.
		return "", false, fmt.Errorf("hf exists %s: status %d", repoID, resp.StatusCode)
	}
}

func (c *hfClient) store(repoID string, m hfModel) {
	c.mu.Lock()
	c.cache[repoID] = m
	c.mu.Unlock()
}
