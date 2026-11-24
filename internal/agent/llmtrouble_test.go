package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"vedetta/internal/llm"
	"vedetta/internal/reader"
	"vedetta/internal/search"
)

// a
func TestClassifyLLMErr(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		cause llmCause
		ok    bool
	}{
		{"status 503", fmt.Errorf("extractor llm failed: %w", &llm.StatusError{Code: 503}), causeUnavailable, true},
		{"status 429 during a pause", &llm.StatusError{Code: 429}, causeUnavailable, true},
		{"dropped connection", fmt.Errorf("llm request failed: %w", &net.OpError{Op: "read", Err: errors.New("reset")}), causeUnavailable, true},
		{"stream cut", fmt.Errorf("reading llm stream: %w", io.ErrUnexpectedEOF), causeUnavailable, true},
		{"stage deadline", fmt.Errorf("llm decomposition failed: %w", context.DeadlineExceeded), causeTimeout, true},
		{"caller went away", context.Canceled, "", false},
		{"unreadable JSON", fmt.Errorf("json parse failed: %w", errors.New("unexpected end")), "", false},
		{"nil", nil, "", false},
	}
	for _, tc := range cases {
		cause, ok := classifyLLMErr(tc.err)
		if cause != tc.cause || ok != tc.ok {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, cause, ok, tc.cause, tc.ok)
		}
	}
}

// b
func TestLLMTrouble_Concurrent(t *testing.T) {
	tr := newLLMTrouble()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := error(&llm.StatusError{Code: 503})
			if i%2 == 1 {
				err = context.DeadlineExceeded
			}
			tr.note(stageExtraction, err)
			tr.note(stageCritic, &llm.StatusError{Code: 503})
		}()
	}
	wg.Wait()
	rec := tr.snapshot()
	if got := rec["extraction"]; got.Count != 8 || got.Cause != causeMixed {
		t.Errorf("extraction = %+v, want 8 mixed", got)
	}
	if got := rec["critic"]; got.Count != 8 || got.Cause != causeUnavailable {
		t.Errorf("critic = %+v, want 8 unavailable", got)
	}
	if got := formatLLMTrouble(rec); got != "critic:8/unavailable,extraction:8/mixed" {
		t.Errorf("formatLLMTrouble = %q", got)
	}
	if !newLLMTrouble().empty() || newLLMTrouble().snapshot() != nil {
		t.Error("a new record must be empty")
	}
}

// c
func TestLLMTroubleNote_AllLanguagesNoNumbers(t *testing.T) {
	digits := regexp.MustCompile(`[0-9]`)
	all := map[string]LLMTroubleCount{}
	for _, s := range stageOrder {
		all[string(s)] = LLMTroubleCount{Count: 7, Cause: causeUnavailable}
	}
	for _, lang := range []string{"it", "en", "de", "fr", "es"} {
		for _, rec := range []map[string]LLMTroubleCount{
			all,
			{"extraction": {Count: 3, Cause: causeTimeout}},
			{"critic": {Count: 1, Cause: causeUnavailable}, "synthesis": {Count: 2, Cause: causeTimeout}},
		} {
			note := llmTroubleNote(rec, lang)
			if !strings.Contains(note, llmTroubleTitle[lang]) || !strings.Contains(note, llmTroubleSubject[lang][overallCause(rec)]) {
				t.Errorf("%s: title or subject missing in %q", lang, note)
			}
			for _, s := range troubleStages(rec) {
				if !strings.Contains(note, llmTroubleStageText[lang][s]) {
					t.Errorf("%s: stage %s missing in %q", lang, s, note)
				}
			}
			if digits.MatchString(note) {
				t.Errorf("%s: the note must not count anything: %q", lang, note)
			}
		}
	}
	if llmTroubleNote(nil, "it") != "" {
		t.Error("no record, no note")
	}
	// Stages are listed in pipeline order.
	note := llmTroubleNote(map[string]LLMTroubleCount{"synthesis": {Cause: causeTimeout}, "planner": {Cause: causeTimeout}}, "en")
	if strings.Index(note, "searches were planned") > strings.Index(note, "writing the answer") {
		t.Errorf("stages out of order: %q", note)
	}
}

// d
func TestComputeProvenance_Degraded(t *testing.T) {
	if got := ComputeProvenance(1, false, false); got != ProvenanceGrounded {
		t.Errorf("no trouble: got %s, want grounded", got)
	}
	if got := ComputeProvenance(1, false, true); got != ProvenanceDegraded {
		t.Errorf("trouble: got %s, want degraded", got)
	}
}

func hasWarning(ws []map[string]string, kind WarningKind) bool {
	for _, w := range ws {
		if w["kind"] == string(kind) {
			return true
		}
	}
	return false
}

func TestExecuteDeepPath_NoAnswerSaysModelUnavailable(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"No available servers"}}`))
	}))
	defer llmSrv.Close()
	sp := &targetedSearchProvider{resultsByQuery: map[string][]search.Result{
		"zen": {{Title: "Zen 5 Specs", URL: "https://example.com/zen5", Content: "dual-pipe decoder 4+4"}},
	}}
	rd := &mockReader{docs: map[string]reader.Document{
		"https://example.com/zen5": {Title: "Zen 5 Specs", URL: "https://example.com/zen5", Content: "AMD Zen 5 features a dual-pipe decoder 4+4 pipeline."},
	}}
	engine := NewEngine(sp, rd, llm.NewClient(llmSrv.URL, "", "test-model").WithRetryPolicy(llm.NewRetryPolicy(0, 30)), nil)
	engine.SetDeepSessionTimeout(30)
	var errMsg string
	emit := func(ev Event) error {
		if ev.Type == EventError {
			if m, ok := ev.Data.(map[string]string); ok {
				errMsg = m["error"]
			}
		}
		return nil
	}
	err := engine.ExecuteDeepPath(context.Background(), SearchParams{Query: "Zen 5 decoder", UILanguage: "it", AnswerLanguage: "it", Mode: "deep"}, emit)
	if err == nil {
		t.Fatal("expected an error: no answer was written")
	}
	if errMsg != StepMessage("it", "error_llm_unavailable") {
		t.Errorf("error message = %q, want the plain-words one", errMsg)
	}
	if strings.Contains(errMsg, "status 503") {
		t.Errorf("raw HTTP error shown to the user: %q", errMsg)
	}
}
