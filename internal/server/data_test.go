package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vedetta/config"
	"vedetta/internal/agent"
	"vedetta/internal/logquiet"
	"vedetta/internal/storage"
)

// Map R7: "I tuoi dati" shows the searches kept; "Cancella tutte le ricerche" needs
// the settings code when there is one, and leaves none.
func TestDataAPI_ClearSearches(t *testing.T) {
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 3; i++ {
		store.CreateSession("ricerca", "deep")
	}
	cfg := &config.Config{ConfigToken: "solo-peppe", ConfigPath: t.TempDir() + "/config.local"}
	h := NewHandlers(cfg, agent.NewEngine(&dummySearchProvider{}, nil, nil, store))

	rec := httptest.NewRecorder()
	h.Data(rec, httptest.NewRequest(http.MethodGet, "/api/data", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("state without the settings code: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.Header.Set("X-Vedetta-Token", "solo-peppe")
	rec = httptest.NewRecorder()
	h.Data(rec, req)
	var st DataResponse
	json.NewDecoder(rec.Body).Decode(&st)
	if st.Sessions != 3 || st.DBBytes == 0 || !filepath.IsAbs(st.DataDir) {
		t.Fatalf("state = %+v", st)
	}

	rec = httptest.NewRecorder()
	h.DataClearSearches(rec, httptest.NewRequest(http.MethodPost, "/api/data/clear-searches", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("clear without the settings code: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/data/clear-searches", nil)
	req.Header.Set("X-Vedetta-Token", "solo-peppe")
	rec = httptest.NewRecorder()
	h.DataClearSearches(rec, req)
	json.NewDecoder(rec.Body).Decode(&st)
	if rec.Code != 200 || st.Removed != 3 || st.Sessions != 0 {
		t.Fatalf("clear = %d %+v", rec.Code, st)
	}
}

// Map R7: the quiet log is chosen in Settings, takes effect at once and is saved.
func TestConfigHandler_QuietLog(t *testing.T) {
	defer logquiet.Set(false)
	path := t.TempDir() + "/config.local"
	cfg := &config.Config{
		SearchProviders: []string{"searxng"}, ReaderProviders: []string{"jina"}, LLMProviders: []string{"deepseek"},
		SearxngURL: "http://localhost:8888", JinaURL: "http://localhost:3001",
		LLMBaseURL: "http://localhost:8000/v1", LLMModel: "m", ConfigPath: path,
	}
	h := NewHandlers(cfg, agent.NewEngine(&dummySearchProvider{}, nil, nil, nil))
	on := true
	body, _ := json.Marshal(ConfigUpdateRequest{QuietLog: &on})
	rec := httptest.NewRecorder()
	h.Config(rec, httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body)))
	var resp ConfigResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if rec.Code != 200 || !resp.QuietLog || !logquiet.On() {
		t.Fatalf("quiet log: %d response=%t switch=%t", rec.Code, resp.QuietLog, logquiet.On())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"quiet_log": true`) {
		t.Errorf("not saved: %s", raw)
	}
}
