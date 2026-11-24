package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vedetta/config"
	"vedetta/internal/agent"
	"vedetta/internal/llm"
	"vedetta/internal/storage"
)

// blockingLLM answers only after release is called, like a model still thinking.
func blockingLLM(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"an answer\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(release)
	return srv, release
}

func newRunHandlers(t *testing.T, llmURL string) (*Handlers, *storage.Storage) {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine := agent.NewEngine(&dummySearchProvider{}, nil, llm.NewClient(llmURL, "", "test-model"), store)
	return NewHandlers(&config.Config{Port: "3030"}, engine), store
}

// startAndLeave begins a search and drops the connection, as a phone does when its
// screen goes off, once the session exists. It returns the session id.
func startAndLeave(t *testing.T, h *Handlers) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": "What happens when the phone sleeps?", "mode": "standard"})
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(body)).WithContext(ctx)
	gone := make(chan struct{})
	go func() {
		h.Search(httptest.NewRecorder(), req)
		close(gone)
	}()

	var id string
	waitFor(t, "the session to exist", func() bool {
		h.runs.mu.Lock()
		defer h.runs.mu.Unlock()
		for k := range h.runs.bySession {
			id = k
		}
		return id != ""
	})
	leave()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not leave with the client")
	}
	return id
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getSession(t *testing.T, h *Handlers, id string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.SessionItem(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET session: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSearch_SurvivesClientLeaving(t *testing.T) {
	llmSrv, release := blockingLLM(t)
	h, store := newRunHandlers(t, llmSrv.URL)

	id := startAndLeave(t, h)

	// The search goes on, and whoever opens the session is told.
	if live, _ := getSession(t, h, id)["live"].(map[string]any); live == nil || live["query"] == "" {
		t.Fatalf("expected the session to say a search is running, got %v", getSession(t, h, id))
	}

	release()
	waitFor(t, "the answer to be saved", func() bool {
		d, err := store.GetSession(id)
		return err == nil && d != nil && len(d.Messages) == 1 && strings.TrimSpace(d.Messages[0].Content) != ""
	})
	waitFor(t, "the search to be over", func() bool { return h.runs.get(id).isFinished() })
	if _, running := getSession(t, h, id)["live"]; running {
		t.Error("a finished search must not be reported as running")
	}
}

func TestStream_ResumesFromWhereTheClientStopped(t *testing.T) {
	reg := newRunRegistry()
	proceed := make(chan struct{})
	run := reg.start("q", "s1", time.Minute, func(ctx context.Context, emit agent.Emitter) error {
		_ = emit(agent.Event{Type: agent.EventToken, Data: "one"})
		_ = emit(agent.Event{Type: agent.EventToken, Data: "two"})
		<-proceed
		_ = emit(agent.Event{Type: agent.EventToken, Data: "three"})
		return nil
	})
	waitFor(t, "two events", func() bool { f, _, _ := run.since(0); return len(f) == 2 })
	close(proceed)
	<-run.done

	h := &Handlers{runs: reg}
	rec := httptest.NewRecorder()
	h.SessionItem(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/stream?from=1", nil))
	got := rec.Body.String()
	want := "event: token\ndata: two\n\nevent: token\ndata: three\n\nevent: end\ndata: {}\n\n"
	if got != want {
		t.Errorf("stream from 1:\n got %q\nwant %q", got, want)
	}
}

func TestStream_UnknownSession404(t *testing.T) {
	h := &Handlers{runs: newRunRegistry()}
	rec := httptest.NewRecorder()
	h.SessionItem(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/nope/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d", rec.Code)
	}
}

func TestDeleteSession_StopsItsSearch(t *testing.T) {
	llmSrv, _ := blockingLLM(t) // never released: only the delete can end this search
	h, store := newRunHandlers(t, llmSrv.URL)

	id := startAndLeave(t, h)
	run := h.runs.get(id)

	rec := httptest.NewRecorder()
	h.SessionItem(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
	if !run.isFinished() {
		t.Error("the search should be over once its session is deleted")
	}
	if d, err := store.GetSession(id); err == nil && d != nil {
		t.Errorf("the session came back after being deleted: %+v", d.SessionSummary)
	}
}

func TestRun_PanicEndsTheSearchNotTheServer(t *testing.T) {
	reg := newRunRegistry()
	run := reg.start("q", "s1", time.Minute, func(context.Context, agent.Emitter) error { panic("boom") })
	<-run.done
	frames, finished, _ := run.since(0)
	if !finished || !bytes.Contains(bytes.Join(frames, nil), []byte("boom")) {
		t.Errorf("finished=%v frames=%q", finished, frames)
	}
}

func TestShutdown_StopsRunningSearches(t *testing.T) {
	reg := newRunRegistry()
	run := reg.start("q", "s1", time.Hour, func(ctx context.Context, _ agent.Emitter) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reg.shutdown(ctx)
	if !run.isFinished() {
		t.Error("the search should be over after shutdown")
	}
}

func TestStop_KeepsTheSession(t *testing.T) {
	llmSrv, _ := blockingLLM(t) // never released: only the stop can end this search
	h, store := newRunHandlers(t, llmSrv.URL)

	id := startAndLeave(t, h)
	run := h.runs.get(id)

	listLive := func() bool {
		rec := httptest.NewRecorder()
		h.ListSessions(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
		var list []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
			t.Fatalf("list: %v %s", err, rec.Body.String())
		}
		return list[0]["live"] == true
	}
	if !listLive() {
		t.Error("the list should mark the session as searching")
	}

	rec := httptest.NewRecorder()
	h.SessionItem(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/stop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the search did not stop")
	}
	if listLive() {
		t.Error("a stopped search must not be marked as searching")
	}
	if d, err := store.GetSession(id); err != nil || d == nil {
		t.Errorf("the session must stay after a stop: %v", err)
	}
}

func TestStop_NothingRunning404(t *testing.T) {
	h := &Handlers{runs: newRunRegistry()}
	rec := httptest.NewRecorder()
	h.SessionItem(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/nope/stop", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d", rec.Code)
	}
}

// External audit 24/11/2026, BUG-05: a second search in a conversation whose search is
// still running took its place, and the first could no longer be stopped.
func TestSearch_OneAtATimeInAConversation(t *testing.T) {
	llmSrv, release := blockingLLM(t)
	h, _ := newRunHandlers(t, llmSrv.URL)
	id := startAndLeave(t, h)
	first := h.runs.get(id)
	again := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"query": "And then?", "session_id": id})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		h.Search(rec, httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(body)).WithContext(ctx))
		return rec
	}
	if rec := again(); rec.Code != http.StatusConflict || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Errorf("a second search while the first runs: %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if h.runs.get(id) != first {
		t.Error("the first search can no longer be found")
	}
	release()
	select {
	case <-first.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the first search does not end")
	}
	if rec := again(); rec.Code != http.StatusOK {
		t.Errorf("a follow-up once it is over: %d", rec.Code)
	}
}
