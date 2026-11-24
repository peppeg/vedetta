package agent

import (
	"context"
	"strings"
	"testing"

	"vedetta/internal/search"
)

func TestExecuteDeepPath_EmptyQuery(t *testing.T) {
	engine := NewEngine(nil, nil, nil, nil)
	err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "  "}, func(e Event) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "empty query") {
		t.Fatalf("expected empty query error, got: %v", err)
	}
}

type targetedSearchProvider struct {
	resultsByQuery map[string][]search.Result
}

func (t *targetedSearchProvider) Name() string { return "targeted-search" }
func (t *targetedSearchProvider) Search(ctx context.Context, query string, opts search.Options) ([]search.Result, error) {
	for k, v := range t.resultsByQuery {
		if strings.Contains(strings.ToLower(query), strings.ToLower(k)) {
			return v, nil
		}
	}
	return nil, nil
}

type mockNetTimeoutErr struct{}

func (m *mockNetTimeoutErr) Error() string   { return "i/o timeout" }
func (m *mockNetTimeoutErr) Timeout() bool   { return true }
func (m *mockNetTimeoutErr) Temporary() bool { return true }
