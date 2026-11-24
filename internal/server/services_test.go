package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"vedetta/config"
)

// homeCfg mirrors prod: SearXNG only, http then Jina, cloud keys in .env.
func homeCfg() *config.Config {
	return &config.Config{
		SearchProviders:       []string{"searxng"},
		DefaultSearchProvider: "searxng",
		ReaderProviders:       []string{"http", "jina"},
		BraveAPIKey:           "brave-secret-123",
		TavilyAPIKey:          "tavily-secret-456",
		FirecrawlAPIKey:       "fire-secret-789",
		ExaAPIKey:             "exa-secret-000",
	}
}

func svc(t *testing.T, kind, id string) ServiceDef {
	t.Helper()
	d, ok := catalogEntry(kind, id)
	if !ok {
		t.Fatalf("no catalog entry %s:%s", kind, id)
	}
	return d
}

func TestServiceOn_DefaultsFreeOnPaidOff(t *testing.T) {
	cfg := homeCfg()
	if !serviceOn(cfg, svc(t, "search", "searxng")) || !serviceOn(cfg, svc(t, "reader", "jina")) {
		t.Fatal("free services must be on by default")
	}
	if serviceOn(cfg, svc(t, "search", "brave")) || serviceOn(cfg, svc(t, "reader", "firecrawl")) {
		t.Fatal("paid services must be off by default, even with a key")
	}
	// A paid service already in a chain (benchmark cloud profile) counts as on.
	cfg.SearchProviders = []string{"brave", "tavily"}
	if !serviceOn(cfg, svc(t, "search", "brave")) || !serviceOn(cfg, svc(t, "search", "tavily")) {
		t.Fatal("services listed in the chain must count as on")
	}
	// An explicit choice wins.
	cfg.Services = map[string]bool{"search:brave": false}
	if serviceOn(cfg, svc(t, "search", "brave")) {
		t.Fatal("explicit off must win over the chain")
	}
}

func TestRequestChain(t *testing.T) {
	cfg := homeCfg()
	if _, err := requestChain(cfg, "search", "brave"); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("brave off: want an 'off' error, got %v", err)
	}
	cfg.Services = map[string]bool{"search:brave": true, "search:serper": true, "reader:firecrawl": true, "reader:exa": true}
	got, err := requestChain(cfg, "search", "brave")
	if err != nil || !reflect.DeepEqual(got, []string{"brave", "searxng"}) {
		t.Fatalf("search brave: got %v, %v (chosen first, then the free fallback)", got, err)
	}
	if _, err := requestChain(cfg, "search", "serper"); err == nil || !strings.Contains(err.Error(), "no api key") {
		t.Fatalf("serper without key: got %v", err)
	}
	if got, _ := requestChain(cfg, "reader", "jina"); !reflect.DeepEqual(got, []string{"jina", "http"}) {
		t.Fatalf("reader jina: got %v", got)
	}
	if got, _ := requestChain(cfg, "reader", "firecrawl"); !reflect.DeepEqual(got, []string{"firecrawl", "http", "jina"}) {
		t.Fatalf("reader firecrawl: got %v", got)
	}
	if _, err := requestChain(cfg, "reader", "browser"); err == nil || !strings.Contains(err.Error(), "no address") {
		t.Fatalf("browser without CDP address: got %v", err)
	}
	if _, err := requestChain(cfg, "search", "nope"); err == nil || !strings.Contains(err.Error(), "unknown search provider") {
		t.Fatalf("unknown: got %v", err)
	}
}

func TestRequestChain_NoPaidFallbackUnlessAllowed(t *testing.T) {
	cfg := homeCfg()
	cfg.ReaderProviders = []string{"http", "exa", "jina"} // e.g. an old file with a paid reader in the chain
	if got, _ := requestChain(cfg, "reader", "jina"); !reflect.DeepEqual(got, []string{"jina", "http"}) {
		t.Fatalf("paid fallback off: got %v", got)
	}
	cfg.PaidFallback = true
	if got, _ := requestChain(cfg, "reader", "jina"); !reflect.DeepEqual(got, []string{"jina", "http", "exa"}) {
		t.Fatalf("paid fallback on: got %v", got)
	}
}

func TestApplyServiceChoices(t *testing.T) {
	cfg := homeCfg()
	on, off := true, false
	// Turning Exa on as a reader, without paid fallback: the chain does not change.
	if err := applyServiceChoices(cfg, &ConfigUpdateRequest{Services: map[string]bool{"reader:exa": true}, PaidFallback: &off}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ReaderProviders, []string{"http", "jina"}) {
		t.Fatalf("reader chain: %v", cfg.ReaderProviders)
	}
	if cfg.Services["search:brave"] || !cfg.Services["search:searxng"] {
		t.Fatalf("first save must freeze the implicit states: %v", cfg.Services)
	}
	// With paid fallback, Exa joins after the free readers.
	if err := applyServiceChoices(cfg, &ConfigUpdateRequest{PaidFallback: &on}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ReaderProviders, []string{"http", "jina", "exa"}) {
		t.Fatalf("reader chain with paid fallback: %v", cfg.ReaderProviders)
	}
	// Order from Settings, Jina first.
	if err := applyServiceChoices(cfg, &ConfigUpdateRequest{PaidFallback: &off, ReaderProviders: []string{"jina", "http"}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ReaderProviders, []string{"jina", "http"}) {
		t.Fatalf("reader order: %v", cfg.ReaderProviders)
	}
	// A paid default is a choice, so it is allowed first.
	brave := "brave"
	if err := applyServiceChoices(cfg, &ConfigUpdateRequest{Services: map[string]bool{"search:brave": true}, DefaultSearchProvider: &brave}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.SearchProviders, []string{"brave", "searxng"}) || cfg.DefaultSearchProvider != "brave" {
		t.Fatalf("search chain: %v default %s", cfg.SearchProviders, cfg.DefaultSearchProvider)
	}
	// Errors: every reader off; unknown service; default that is off.
	if err := applyServiceChoices(cfg, &ConfigUpdateRequest{Services: map[string]bool{"reader:http": false, "reader:jina": false, "reader:exa": false}}); err == nil {
		t.Fatal("all readers off must be refused")
	}
	if err := applyServiceChoices(homeCfg(), &ConfigUpdateRequest{Services: map[string]bool{"search:altavista": true}}); err == nil {
		t.Fatal("unknown service must be refused")
	}
	tav := "tavily"
	if err := applyServiceChoices(homeCfg(), &ConfigUpdateRequest{Services: map[string]bool{}, DefaultSearchProvider: &tav}); err == nil {
		t.Fatal("a default that is off must be refused")
	}
}

func TestServicesEndpoint_NoSecretsNoChat(t *testing.T) {
	var chats int64
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			atomic.AddInt64(&chats, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer llmSrv.Close()
	cfg := homeCfg()
	cfg.SearxngURL = llmSrv.URL
	cfg.JinaURL = llmSrv.URL
	paid := true
	cfg.LLMEndpoints = []config.LLMEndpoint{{ID: "cloud", Name: "cloud", BaseURL: llmSrv.URL, Model: "m1", APIKey: "llm-secret-999", Paid: &paid}}
	cfg.LLMDefaultID = "cloud"
	h := NewHandlers(cfg, nil)

	rec := httptest.NewRecorder()
	h.Services(rec, httptest.NewRequest(http.MethodGet, "/api/services", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"brave-secret-123", "tavily-secret-456", "fire-secret-789", "exa-secret-000", "llm-secret-999"} {
		if strings.Contains(body, secret) {
			t.Fatalf("a key leaked into /api/services: %s", secret)
		}
	}
	if n := atomic.LoadInt64(&chats); n != 0 {
		t.Fatalf("/api/services sent %d chat requests: probes must never be billable", n)
	}
	var resp ServicesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var brave ServiceView
	for _, s := range resp.Search {
		if s.ID == "brave" {
			brave = s
		}
	}
	if !brave.Paid || brave.On || !brave.HasKey || brave.Reason != "off" {
		t.Fatalf("brave view: %+v", brave)
	}
	if len(resp.LLM) != 1 || resp.LLM[0].Status != "ok" || !resp.LLM[0].Paid || len(resp.LLM[0].Models) != 2 {
		t.Fatalf("llm view: %+v", resp.LLM)
	}
	if len(resp.LLMPresets) == 0 {
		t.Fatal("presets missing")
	}
}

// A model whose key comes from a missing variable is "missing_key", not
// "responds", even when its service lists models without a key (OpenRouter).
func TestServicesEndpoint_ModelWithoutKey(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()
	cfg := homeCfg()
	cfg.SearxngURL, cfg.JinaURL = srv.URL, srv.URL
	paid := true
	cfg.LLMEndpoints = []config.LLMEndpoint{{ID: "or", BaseURL: "https://openrouter.ai/api/v1", Model: "x", KeyEnv: "OPENROUTER_API_KEY", Paid: &paid}}
	cfg.LLMDefaultID = "or"
	resp := buildServices(httptest.NewRequest(http.MethodGet, "/", nil).Context(), cfg)
	if resp.LLM[0].Status != "missing_key" || resp.LLM[0].HasKey {
		t.Fatalf("got %+v", resp.LLM[0])
	}
}

// With a settings code, the bar still gets the list, but without addresses,
// key variable names or presets; the code unlocks the full view.
func TestServicesEndpoint_TokenPublicView(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()
	cfg := homeCfg()
	cfg.ConfigToken = "the-code"
	cfg.SearxngURL, cfg.JinaURL = srv.URL, srv.URL
	cfg.LLMEndpoints = []config.LLMEndpoint{{ID: "home", BaseURL: srv.URL + "/v1", Model: "m1", KeyEnv: "HOME_KEY", APIKey: "k"}}
	cfg.LLMDefaultID = "home"
	h := NewHandlers(cfg, nil)

	get := func(token string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/services?probe=1", nil)
		if token != "" {
			req.Header.Set("X-Vedetta-Token", token)
		}
		rec := httptest.NewRecorder()
		h.Services(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, tok := range []string{"", "wrong"} {
		code, body := get(tok)
		if code != http.StatusOK {
			t.Fatalf("token %q: status %d", tok, code)
		}
		for _, hidden := range []string{srv.URL, "HOME_KEY", "llm_presets\":[{"} {
			if strings.Contains(body, hidden) {
				t.Fatalf("token %q: public view shows %q:\n%s", tok, hidden, body)
			}
		}
		if !strings.Contains(body, `"id":"home"`) || !strings.Contains(body, `"id":"searxng"`) {
			t.Fatalf("token %q: the bar needs the list:\n%s", tok, body)
		}
	}
	code, body := get("the-code")
	if code != http.StatusOK || !strings.Contains(body, srv.URL) || !strings.Contains(body, "HOME_KEY") || !strings.Contains(body, "openrouter") {
		t.Fatalf("with the code the full view is expected: %d\n%s", code, body)
	}
}

func modelsCall(h *Handlers, body, origin, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/services/models", strings.NewReader(body))
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServiceModels(w, r)
	return w
}

func TestServiceModels(t *testing.T) {
	t.Setenv("CASA_MODELS_KEY", "env-secret")
	var gotAuth atomic.Value
	gotAuth.Store("")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[{"id":"m:free","mayTrainOnYourPrompts":true}]}`))
	}))
	defer upstream.Close()
	h := &Handlers{cfg: &config.Config{LLMEndpoints: []config.LLMEndpoint{{ID: "saved", BaseURL: upstream.URL + "/", Model: "m", APIKey: "saved-secret"}}}}
	const host = "vedetta.test"

	body := func(endpoint, base, key string) string {
		b, _ := json.Marshal(modelsRequest{EndpointID: endpoint, BaseURL: base, APIKey: key})
		return string(b)
	}
	decode := func(t *testing.T, w *httptest.ResponseRecorder) modelsResponse {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		var resp modelsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("a new row lists its models through a key reference", func(t *testing.T) {
		resp := decode(t, modelsCall(h, body("", upstream.URL, "${CASA_MODELS_KEY}"), "http://"+host, host))
		if resp.Status != "ok" || len(resp.Models) != 1 || resp.Models[0].MayTrain == nil {
			t.Fatalf("%+v", resp)
		}
		if gotAuth.Load() != "Bearer env-secret" {
			t.Fatalf("auth %v", gotAuth.Load())
		}
	})
	t.Run("a variable that is not set is a missing key, and nothing is asked", func(t *testing.T) {
		gotAuth.Store("untouched")
		resp := decode(t, modelsCall(h, body("", upstream.URL, "${CASA_NONE_KEY}"), "", host))
		if resp.Status != "missing_key" || gotAuth.Load() != "untouched" {
			t.Fatalf("%+v %v", resp, gotAuth.Load())
		}
	})
	t.Run("a saved row uses its saved key", func(t *testing.T) {
		decode(t, modelsCall(h, body("saved", upstream.URL, ""), "", host))
		if gotAuth.Load() != "Bearer saved-secret" {
			t.Fatalf("auth %v", gotAuth.Load())
		}
	})
	t.Run("a saved key does not follow an edited address", func(t *testing.T) {
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth.Store(r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer other.Close()
		decode(t, modelsCall(h, body("saved", other.URL, ""), "", host))
		if gotAuth.Load() != "" {
			t.Fatalf("the saved key went to another address: %v", gotAuth.Load())
		}
	})
	t.Run("another site is refused and nothing is sent", func(t *testing.T) {
		gotAuth.Store("untouched")
		w := modelsCall(h, body("", upstream.URL, "${CASA_MODELS_KEY}"), "https://evil.example", host)
		if w.Code != http.StatusForbidden || gotAuth.Load() != "untouched" {
			t.Fatalf("status %d, auth %v", w.Code, gotAuth.Load())
		}
		if w := modelsCall(h, body("", upstream.URL, ""), "null", host); w.Code != http.StatusForbidden {
			t.Fatalf("origin null: status %d", w.Code)
		}
	})
	t.Run("a service that does not answer is down, without details", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "wrong key env-secret", http.StatusUnauthorized)
		}))
		defer bad.Close()
		w := modelsCall(h, body("", bad.URL, "${CASA_MODELS_KEY}"), "", host)
		if resp := decode(t, w); resp.Status != "down" || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("%s", w.Body)
		}
	})
	t.Run("only http addresses", func(t *testing.T) {
		if w := modelsCall(h, body("", "file:///etc/passwd", ""), "", host); w.Code != http.StatusBadRequest {
			t.Fatalf("status %d", w.Code)
		}
	})
	t.Run("the settings code is required when set", func(t *testing.T) {
		locked := &Handlers{cfg: &config.Config{ConfigToken: "code"}}
		if w := modelsCall(locked, body("", upstream.URL, ""), "", host); w.Code != http.StatusUnauthorized {
			t.Fatalf("status %d", w.Code)
		}
	})
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		origin, forwarded string
		want              bool
	}{
		{"", "", true}, // not a browser page
		{"http://vedetta.test:3030", "", true},
		{"HTTP://VEDETTA.TEST:3030", "", true},
		{"https://proxy.example", "proxy.example", true},
		{"http://vedetta.test:9999", "", false}, // another port is another site
		{"https://evil.example", "", false},
		{"https://evil.example", "proxy.example", false},
		{"null", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Host = "vedetta.test:3030"
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.forwarded != "" {
			r.Header.Set("X-Forwarded-Host", c.forwarded)
		}
		if got := sameOrigin(r); got != c.want {
			t.Errorf("origin %q, forwarded %q: got %v", c.origin, c.forwarded, got)
		}
	}
}

func TestConfig_OtherSitesCannotSave(t *testing.T) {
	h := &Handlers{cfg: &config.Config{ContactEmail: "before@example.invalid"}}
	r := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"contact_email":"after@example.invalid"}`))
	r.Host = "vedetta.test"
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.Config(w, r)
	if w.Code != http.StatusForbidden || h.cfg.ContactEmail != "before@example.invalid" {
		t.Fatalf("status %d, email %q", w.Code, h.cfg.ContactEmail)
	}
}
