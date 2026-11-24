package search

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25Reranker implements in-memory BM25 lexical reranking for search results.
type BM25Reranker struct {
	k1 float64
	b  float64
}

// NewBM25Reranker creates a new reranker with standard BM25 parameters (k1=1.2, b=0.75).
func NewBM25Reranker() *BM25Reranker {
	return &BM25Reranker{
		k1: 1.2,
		b:  0.75,
	}
}

// Tokenize converts a text string into a list of lowercase alphanumeric tokens.
func Tokenize(text string) []string {
	var tokens []string
	var current strings.Builder

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
		} else {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}

// Rerank calculates BM25 scores for candidate results against the query and returns them
// sorted in descending order of relevance. Results below minScore are discarded.
func (r *BM25Reranker) Rerank(query string, results []Result, minScore float64) []Result {
	if len(results) == 0 {
		return nil
	}

	queryTokens := Tokenize(query)
	if len(queryTokens) == 0 {
		return results
	}

	// Remove duplicate query terms for IDF calculation
	uniqueQueryTerms := make(map[string]bool)
	for _, t := range queryTokens {
		uniqueQueryTerms[t] = true
	}

	N := float64(len(results))

	// Pre-tokenize and count doc lengths
	type docProfile struct {
		length  float64
		termFreq map[string]float64
	}
	profiles := make([]docProfile, len(results))
	var totalLength float64
	docFreq := make(map[string]float64)

	for i, res := range results {
		titleTokens := Tokenize(res.Title)
		contentTokens := Tokenize(res.Content)

		tf := make(map[string]float64)
		// Title tokens get 2x weight in term frequency
		for _, t := range titleTokens {
			tf[t] += 2.0
		}
		for _, t := range contentTokens {
			tf[t] += 1.0
		}

		docLen := float64(len(titleTokens)*2 + len(contentTokens))
		profiles[i] = docProfile{
			length:   docLen,
			termFreq: tf,
		}
		totalLength += docLen

		for q := range uniqueQueryTerms {
			if tf[q] > 0 {
				docFreq[q]++
			}
		}
	}

	avgdl := totalLength / N
	if avgdl == 0 {
		avgdl = 1.0
	}

	// Compute IDF for each query term: ln(1 + (N - df + 0.5) / (df + 0.5))
	idf := make(map[string]float64)
	for q := range uniqueQueryTerms {
		df := docFreq[q]
		idf[q] = math.Log(1.0 + (N-df+0.5)/(df+0.5))
	}

	scored := make([]Result, len(results))
	copy(scored, results)

	for i := range scored {
		prof := profiles[i]
		var score float64

		for _, q := range queryTokens {
			freq := prof.termFreq[q]
			if freq == 0 {
				continue
			}
			num := freq * (r.k1 + 1.0)
			den := freq + r.k1*(1.0-r.b+r.b*(prof.length/avgdl))
			score += idf[q] * (num / den)
		}

		scored[i].Score = score
	}

	// Sort descending by score
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	// Filter by minScore if specified
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
		// Fallback safeguard: if all results fell below threshold, keep at least the top result
		if len(scored) > 0 {
			return scored[:1]
		}
	}

	return scored
}

