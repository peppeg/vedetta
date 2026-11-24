package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListModels_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("expected /models, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "model-a")
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" {
		t.Fatalf("expected [model-a model-b], got %v", models)
	}
}

func TestListModels_OllamaShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen2"},{"name":"llama3"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "qwen2")
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 || models[0] != "qwen2" || models[1] != "llama3" {
		t.Fatalf("expected [qwen2 llama3], got %v", models)
	}
}

func TestListModels_Down(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed listener -> connection refused

	c := NewClient(srv.URL, "", "model-a")
	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("expected an error for an unreachable models endpoint")
	}
}

func TestParseModelInfo(t *testing.T) {
	body := `{"data":[
		{"id":"kilo-auto/free","isFree":true,"supported_parameters":["tools"],"mayTrainOnYourPrompts":true},
		{"id":"z-ai/glm","isFree":false,"pricing":{"prompt":"0.1","completion":"0.2"},"supported_parameters":["tools"],"mayTrainOnYourPrompts":false},
		{"id":"or/zero","pricing":{"prompt":"0","completion":"0"},"supported_parameters":["temperature"]},
		{"id":"or/x:free","pricing":{"prompt":"1","completion":"1"}},
		{"id":"or/auto","pricing":{"prompt":"-1","completion":"-1"}},
		{"id":"local/plain"},
		{"id":"odd/kinds","isFree":"yes","pricing":"none","supported_parameters":null,"mayTrainOnYourPrompts":null}
	]}`
	yes, no := true, false
	want := []ModelInfo{
		{ID: "kilo-auto/free", Free: &yes, Tools: &yes, MayTrain: &yes},
		{ID: "z-ai/glm", Free: &no, Tools: &yes, MayTrain: &no},
		{ID: "or/zero", Free: &yes, Tools: &no},
		{ID: "or/x:free", Free: &yes},
		{ID: "or/auto", Free: &no},
		{ID: "local/plain"},
		{ID: "odd/kinds"},
	}
	got := parseModelInfo([]byte(body))
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d", len(got), len(want))
	}
	same := func(a, b *bool) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	for i, w := range want {
		g := got[i]
		if g.ID != w.ID || !same(g.Free, w.Free) || !same(g.Tools, w.Tools) || !same(g.MayTrain, w.MayTrain) {
			t.Errorf("%s: got free=%v tools=%v train=%v", w.ID, g.Free, g.Tools, g.MayTrain)
		}
	}
}

func TestListModels_LongList(t *testing.T) {
	entry := `{"id":"m","description":"` + strings.Repeat("x", 2<<20) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[` + entry + `]}`))
	}))
	defer srv.Close()
	models, err := NewClient(srv.URL, "", "m").ListModels(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("a list over 1 MB must not come back empty: %v %v", models, err)
	}
}
