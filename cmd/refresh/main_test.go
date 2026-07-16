package main

import (
	"reflect"
	"sort"
	"testing"
)

func TestHFIDFromURL(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"https://huggingface.co/openai-community/gpt2", "openai-community/gpt2", true},
		{"https://huggingface.co/google/gemma-3-27b-it", "google/gemma-3-27b-it", true},
		{"https://huggingface.co/gpt2", "gpt2", true},
		{"https://huggingface.co/Qwen/Qwen3-32B/blob/main/LICENSE", "Qwen/Qwen3-32B", true},
		{"https://huggingface.co/org/repo?library=transformers", "org/repo", true},
		{"https://huggingface.co/org/repo/", "org/repo", true},
		{"https://aws.amazon.com/marketplace/pp/prodview-xyz", "", false},
		{"https://www.llama.com/llama4/license/", "", false},
		{"https://catboost.ai/", "", false},
		{"", "", false},
		// Too deep to be a clean repo id.
		{"https://huggingface.co/a/b/c", "", false},
	}
	for _, tt := range tests {
		got, ok := hfIDFromURL(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("hfIDFromURL(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestParseRegions(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", defaultRegions},
		{"   ", defaultRegions},
		{"us-east-1", []string{"us-east-1"}},
		{"us-east-1,us-west-2", []string{"us-east-1", "us-west-2"}},
		{"us-east-1, us-west-2", []string{"us-east-1", "us-west-2"}},
		{"us-east-1 us-west-2", []string{"us-east-1", "us-west-2"}},
	}
	for _, tt := range tests {
		got := parseRegions(tt.in)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseRegions(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSearchQuery(t *testing.T) {
	tests := []struct{ in, want string }{
		{"qwen.qwen3-next-80b-a3b", "qwen3-next-80b-a3b"},
		{"qwen.qwen3-32b-v1:0", "qwen3-32b"},
		{"openai.gpt-oss-20b-1:0", "gpt-oss-20b-1"}, // trailing -1 is not a version tail; left as-is
		{"deepseek.r1-v1:0", "r1"},
		{"meta.llama4-scout-17b-instruct-v1:0", "llama4-scout-17b-instruct"},
	}
	for _, tt := range tests {
		if got := searchQuery(tt.in); got != tt.want {
			t.Errorf("searchQuery(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSelectCandidate(t *testing.T) {
	// Base family repo present -> pick it, no ambiguity.
	t.Run("base family wins", func(t *testing.T) {
		q := tokenSet("qwen3-32b")
		ids := []string{"Qwen/Qwen3-32B", "Qwen/Qwen3-32B-FP8", "Qwen/Qwen3-32B-Instruct"}
		m := selectCandidate("Qwen", q, ids)
		if m.base != "Qwen/Qwen3-32B" {
			t.Errorf("base = %q, want Qwen/Qwen3-32B", m.base)
		}
		if m.ambiguous {
			t.Errorf("unexpected ambiguity: %v", m.variants)
		}
	})

	// No base, single primary variant (+ quantized dupes) -> unique pick.
	t.Run("unique primary variant", func(t *testing.T) {
		q := tokenSet("qwen3-coder-30b-a3b")
		ids := []string{"Qwen/Qwen3-Coder-30B-A3B-Instruct", "Qwen/Qwen3-Coder-30B-A3B-Instruct-FP8"}
		m := selectCandidate("Qwen", q, ids)
		if m.ambiguous {
			t.Fatalf("unexpected ambiguity: %v", m.variants)
		}
		if m.pick != "Qwen/Qwen3-Coder-30B-A3B-Instruct" {
			t.Errorf("pick = %q, want Qwen/Qwen3-Coder-30B-A3B-Instruct", m.pick)
		}
	})

	// No base, two distinct primary variants -> ambiguous, both reported.
	t.Run("ambiguous instruct vs thinking", func(t *testing.T) {
		q := tokenSet("qwen3-next-80b-a3b")
		ids := []string{"Qwen/Qwen3-Next-80B-A3B-Instruct", "Qwen/Qwen3-Next-80B-A3B-Thinking"}
		m := selectCandidate("Qwen", q, ids)
		if !m.ambiguous {
			t.Fatalf("expected ambiguity, got base=%q pick=%q", m.base, m.pick)
		}
		want := []string{"Qwen/Qwen3-Next-80B-A3B-Instruct", "Qwen/Qwen3-Next-80B-A3B-Thinking"}
		got := append([]string(nil), m.variants...)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("variants = %v, want %v", got, want)
		}
	})

	// Lookalike that doesn't contain all query tokens must be rejected.
	t.Run("rejects lookalike", func(t *testing.T) {
		q := tokenSet("qwen3-32b")
		ids := []string{"deepseek-ai/DeepSeek-R1-Distill-Qwen-32B"} // wrong org anyway, but also missing "qwen3"
		m := selectCandidate("Qwen", q, ids)
		if m.base != "" || m.pick != "" || m.ambiguous {
			t.Errorf("expected no match, got %+v", m)
		}
	})
}

func TestLookupOverrideAndCard(t *testing.T) {
	r := &resolver{
		overrides: map[string]override{
			"meta.llama4-scout-17b-instruct-v1:0": {HFID: "meta-llama/Llama-4-Scout-17B-16E-Instruct"},
		},
	}
	// Exact and context-window-suffixed lookups both resolve.
	if ov, ok := r.lookupOverride("meta.llama4-scout-17b-instruct-v1:0"); !ok || ov.HFID == "" {
		t.Errorf("exact override lookup failed")
	}
	if ov, ok := r.lookupOverride("meta.llama4-scout-17b-instruct-v1:0:128k"); !ok ||
		ov.HFID != "meta-llama/Llama-4-Scout-17B-16E-Instruct" {
		t.Errorf("context-window override lookup failed: %+v %v", ov, ok)
	}

	cards := map[string]cardInfo{
		"meta.llama3-1-8b-instruct-v1:0": {EULA: "https://www.llama.com/llama3/license/"},
	}
	if _, ok := lookupCard(cards, "meta.llama3-1-8b-instruct-v1:0:128k"); !ok {
		t.Errorf("context-window card lookup failed")
	}
}

func TestIsClosedProvider(t *testing.T) {
	tests := []struct {
		prefix string
		closed bool
	}{
		{"anthropic", true},
		{"amazon", true},
		{"cohere", true},
		{"stability", true},
		{"writer", true},
		{"qwen", false},
		{"meta", false},
		{"deepseek", false},
		{"openai", false},
	}
	for _, tt := range tests {
		if got := isClosedProvider("", tt.prefix); got != tt.closed {
			t.Errorf("isClosedProvider(%q) = %v, want %v", tt.prefix, got, tt.closed)
		}
	}
}
