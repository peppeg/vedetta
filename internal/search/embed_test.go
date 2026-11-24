package search

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCosineSimilarity(t *testing.T) {
	// Identical vectors: cosine = 1.0
	v1 := []float64{1.0, 2.0, 3.0}
	v2 := []float64{1.0, 2.0, 3.0}
	if sim := CosineSimilarity(v1, v2); math.Abs(sim-1.0) > 1e-6 {
		t.Errorf("expected cosine similarity 1.0, got %f", sim)
	}

	// Orthogonal vectors: cosine = 0.0
	v3 := []float64{1.0, 0.0}
	v4 := []float64{0.0, 1.0}
	if sim := CosineSimilarity(v3, v4); math.Abs(sim-0.0) > 1e-6 {
		t.Errorf("expected cosine similarity 0.0, got %f", sim)
	}

	// Opposite vectors: cosine = -1.0
	v5 := []float64{1.0, 0.0}
	v6 := []float64{-1.0, 0.0}
	if sim := CosineSimilarity(v5, v6); math.Abs(sim-(-1.0)) > 1e-6 {
		t.Errorf("expected cosine similarity -1.0, got %f", sim)
	}
}

func TestEmbeddingClient_FetchEmbeddings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := embedResponse{
			Data: []struct {
				Embedding []float64 `json:"embedding"`
				Index     int       `json:"index"`
			}{
				{Embedding: []float64{0.1, 0.2, 0.3}, Index: 0},
				{Embedding: []float64{0.4, 0.5, 0.6}, Index: 1},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewEmbeddingClient(srv.URL, "key", "test-embed")
	vecs, err := client.FetchEmbeddings(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("FetchEmbeddings failed: %v", err)
	}

	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vecs))
	}
	if len(vecs[0]) != 3 || len(vecs[1]) != 3 {
		t.Errorf("unexpected vector dimension: %v, %v", vecs[0], vecs[1])
	}
}

func TestEmbeddingReranker_FallbackBM25(t *testing.T) {
	// Server returns 500 error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewEmbeddingClient(srv.URL, "", "test-embed")
	bm25 := NewBM25Reranker()
	reranker := NewEmbeddingReranker(client, bm25)

	docs := []Result{
		{Title: "Gardening", Content: "Growing tomatoes and carrots"},
		{Title: "CPUs and Silicon", Content: "Semiconductor architectures and x86 decoders"},
	}

	// Should not error, but fallback to BM25
	ranked := reranker.Rerank("silicon decoders", docs, 0)
	if len(ranked) != 2 {
		t.Fatalf("expected 2 ranked results, got %d", len(ranked))
	}
	if ranked[0].Title != "CPUs and Silicon" {
		t.Errorf("expected BM25 fallback to rank 'CPUs and Silicon' first, got: %s", ranked[0].Title)
	}
}

func TestEmbeddingReranker_NilClientFallback(t *testing.T) {
	bm25 := NewBM25Reranker()
	reranker := NewEmbeddingReranker(nil, bm25)

	docs := []Result{
		{Title: "Unrelated", Content: "Cooking pasta"},
		{Title: "Physics", Content: "Quantum mechanics and wavefunctions"},
	}

	ranked := reranker.Rerank("quantum physics", docs, 0)
	if len(ranked) != 2 || ranked[0].Title != "Physics" {
		t.Errorf("expected BM25 to rank 'Physics' first, got: %s", ranked[0].Title)
	}
}
