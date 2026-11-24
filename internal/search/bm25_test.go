package search

import (
	"testing"
)

func TestBM25_ScoringAndRanking(t *testing.T) {
	reranker := NewBM25Reranker()

	docs := []Result{
		{
			Title:   "Unrelated Topic",
			URL:     "https://example.com/other",
			Content: "This document is about gardening and planting vegetables in spring.",
		},
		{
			Title:   "Zen 5 Architecture Deep Dive",
			URL:     "https://example.com/zen5",
			Content: "AMD Zen 5 introduces a new dual-pipe instruction decoder and improved IPC benchmark.",
		},
		{
			Title:   "Intel Core Ultra Overview",
			URL:     "https://example.com/intel",
			Content: "Intel architecture details comparing Lunar Lake and Arrow Lake processors.",
		},
	}

	query := "Zen 5 instruction decoder"
	ranked := reranker.Rerank(query, docs, 0)

	if len(ranked) != 3 {
		t.Fatalf("expected 3 ranked results, got %d", len(ranked))
	}

	// The Zen 5 document must be #1 with the highest score
	if ranked[0].URL != "https://example.com/zen5" {
		t.Fatalf("expected top document to be Zen 5, got %s (score: %f)", ranked[0].URL, ranked[0].Score)
	}

	if ranked[0].Score <= ranked[1].Score || ranked[1].Score < ranked[2].Score {
		t.Errorf("scores not monotonically decreasing: %f, %f, %f", ranked[0].Score, ranked[1].Score, ranked[2].Score)
	}

	// Test threshold filtering
	filtered := reranker.Rerank(query, docs, ranked[0].Score-0.1)
	if len(filtered) != 1 || filtered[0].URL != "https://example.com/zen5" {
		t.Errorf("expected 1 result above high threshold, got %d", len(filtered))
	}
}

func TestBM25_EmptyQueryAndDocs(t *testing.T) {
	reranker := NewBM25Reranker()

	if res := reranker.Rerank("", nil, 0); len(res) != 0 {
		t.Errorf("expected empty result for nil docs")
	}

	docs := []Result{
		{Title: "Title", URL: "https://example.com", Content: "Content"},
	}
	if res := reranker.Rerank("", docs, 0); len(res) != 1 {
		t.Errorf("expected original docs when query is empty")
	}
}

