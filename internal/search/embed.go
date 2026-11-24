package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// EmbeddingClient communicates with an OpenAI-compatible /v1/embeddings endpoint
// (such as Ollama, vLLM, or cloud providers).
type EmbeddingClient struct {
	endpoint   string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewEmbeddingClient creates a client for the embeddings endpoint.
func NewEmbeddingClient(endpoint, apiKey, model string) *EmbeddingClient {
	endpoint = strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(endpoint, "/embeddings") {
		endpoint += "/embeddings"
	}
	if model == "" {
		model = "bge-m3" // default lightweight multi-lingual embedding model
	}

	return &EmbeddingClient{
		endpoint: endpoint,
		apiKey:   apiKey,
		model:    model,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type embedRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// FetchEmbeddings retrieves vector embeddings for a slice of texts.
func (c *EmbeddingClient) FetchEmbeddings(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody := embedRequest{
		Input: texts,
		Model: c.model,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed API returned status %d", resp.StatusCode)
	}

	var res embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("failed to decode embed response: %w", err)
	}

	if len(res.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, received %d", len(texts), len(res.Data))
	}

	vectors := make([][]float64, len(texts))
	for _, item := range res.Data {
		if item.Index >= 0 && item.Index < len(texts) {
			vectors[item.Index] = item.Embedding
		}
	}

	return vectors, nil
}

// CosineSimilarity calculates the cosine of the angle between two vectors.
func CosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0.0
	}

	var dot, normA, normB float64
	for i := 0; i < len(a); i++ {
		dot += a[i] * b[i]
	}
	for i := 0; i < len(a); i++ {
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0.0
	}

	return dot / denom
}

// EmbeddingReranker provides vector cosine similarity ranking with transparent BM25 fallback.
type EmbeddingReranker struct {
	client   *EmbeddingClient
	fallback *BM25Reranker
}

// NewEmbeddingReranker creates an EmbeddingReranker.
func NewEmbeddingReranker(client *EmbeddingClient, fallback *BM25Reranker) *EmbeddingReranker {
	return &EmbeddingReranker{
		client:   client,
		fallback: fallback,
	}
}

// Rerank reranks search results using embedding cosine similarity.
// If the embedding endpoint is not configured or fails, it cleanly falls back to BM25.
func (r *EmbeddingReranker) Rerank(query string, results []Result, minScore float64) []Result {
	if len(results) == 0 || query == "" {
		return results
	}

	if r.client == nil {
		return r.fallback.Rerank(query, results, minScore)
	}

	texts := make([]string, len(results)+1)
	texts[0] = query
	for i, res := range results {
		// Combine title and snippet
		texts[i+1] = res.Title + " - " + res.Content
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	vectors, err := r.client.FetchEmbeddings(ctx, texts)
	if err != nil {
		log.Printf("[reranker] embedding client error, falling back to pure Go BM25: %v", err)
		return r.fallback.Rerank(query, results, minScore)
	}

	queryVec := vectors[0]
	scored := make([]Result, len(results))
	copy(scored, results)

	for i := 0; i < len(results); i++ {
		docVec := vectors[i+1]
		sim := CosineSimilarity(queryVec, docVec)
		scored[i].Score = sim
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if minScore > 0 {
		var filtered []Result
		for _, s := range scored {
			if s.Score >= minScore {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) > 0 {
			return filtered
		}
		if len(scored) > 0 {
			return scored[:1]
		}
	}

	return scored
}
