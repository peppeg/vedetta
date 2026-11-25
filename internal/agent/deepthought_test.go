package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vedetta/internal/llm"
	"vedetta/internal/search"
	"vedetta/internal/storage"
)

func deepThoughtEngine(t *testing.T, calls *int) (*Engine, *storage.Storage, *mockSearchProvider) {
	t.Helper()
	old := deepThoughtTime
	deepThoughtTime = 20 * time.Millisecond
	t.Cleanup(func() { deepThoughtTime = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { *calls++; w.WriteHeader(400) }))
	t.Cleanup(srv.Close)
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "42.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sp := &mockSearchProvider{results: []search.Result{{Title: "r", URL: "https://real.test/r"}}}
	e := NewEngine(sp, &mockReader{}, llm.NewClient(srv.URL, "", "m"), store)
	return e, store, sp
}

func TestDeepThought_AnswersEverything(t *testing.T) {
	calls := 0
	e, store, _ := deepThoughtEngine(t, &calls)
	e.SetDeepThought(true)
	var answer, sid, last string
	started := time.Now()
	ask := func(q, session string) {
		answer = ""
		err := e.ExecuteDeepPath(context.Background(), SearchParams{Query: q, SessionID: session, UILanguage: "it", AnswerLanguage: "it"}, func(ev Event) error {
			switch ev.Type {
			case EventSession:
				sid = ev.Data.(map[string]string)["session_id"]
			case EventToken:
				answer += ev.Data.(string)
			case EventDone:
				last = ev.Data.(map[string]interface{})["status"].(string)
			case EventSources, EventBibliography, EventError:
				t.Errorf("unexpected event %s", ev.Type)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ask("Che tempo fa domani a Milano?", "")
	if time.Since(started) < deepThoughtTime {
		t.Error("it did not think")
	}
	if body := strings.TrimSpace(strings.Split(answer, NotesMarker)[0]); body != "**42**" || last != "completed" {
		t.Errorf("answer %q, status %q", body, last)
	}
	if !strings.Contains(answer, "Impostazioni → Sperimentale") || strings.Contains(answer, "Provenienza") {
		t.Errorf("the note must say what it is and where to turn it off: %q", answer)
	}
	ask("E dopodomani?", sid) // a follow-up too
	detail, err := store.GetSession(sid)
	if err != nil || len(detail.Messages) != 2 || detail.Messages[1].Content != answer || detail.Messages[1].Query != "E dopodomani?" {
		t.Fatalf("saved: %v %+v", err, detail)
	}
	if calls != 0 {
		t.Errorf("the model was called %d times", calls)
	}
}

func TestDeepThought_OffAndStopped(t *testing.T) {
	calls := 0
	e, store, _ := deepThoughtEngine(t, &calls)
	if e.DeepThought() {
		t.Fatal("on by default")
	}
	_ = e.ExecuteDeepPath(context.Background(), SearchParams{Query: "x", AnswerLanguage: "it"}, func(Event) error { return nil })
	if calls == 0 {
		t.Error("off: the research should have called the model")
	}

	e.SetDeepThought(true)
	deepThoughtTime = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	sid := ""
	err := e.ExecuteDeepPath(ctx, SearchParams{Query: "x", AnswerLanguage: "it"}, func(ev Event) error {
		switch ev.Type {
		case EventSession:
			sid = ev.Data.(map[string]string)["session_id"]
		case EventStep:
			cancel()
		case EventToken, EventDone:
			t.Errorf("stopped, yet %s", ev.Type)
		}
		return nil
	})
	if err == nil {
		t.Error("a stopped search ended well")
	}
	if detail, _ := store.GetSession(sid); detail != nil && len(detail.Messages) != 0 {
		t.Errorf("something was saved: %+v", detail.Messages)
	}
}

func TestDeepThought_EveryLanguage(t *testing.T) {
	for _, key := range []string{"deep_thought_step", "deep_thought_note"} {
		seen := map[string]bool{}
		for _, lang := range []string{"it", "en", "de", "fr", "es"} {
			m := StepMessage(lang, key)
			if m == "" || m == key || seen[m] {
				t.Errorf("%s in %s: %q", key, lang, m)
			}
			seen[m] = true
		}
	}
}
