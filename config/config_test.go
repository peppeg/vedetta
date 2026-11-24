package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadLocalConfig_SharedFields(t *testing.T) {
	path := t.TempDir() + "/config.local"

	want := &Config{
		ContactEmail:          "ops@example.com",
		DefaultSearchProvider: "brave",
	}
	if err := SaveLocalConfig(path, want); err != nil {
		t.Fatalf("SaveLocalConfig failed: %v", err)
	}

	got := &Config{}
	loadLocalConfig(path, got)

	if got.ContactEmail != want.ContactEmail {
		t.Errorf("ContactEmail = %q, want %q", got.ContactEmail, want.ContactEmail)
	}
	if got.DefaultSearchProvider != want.DefaultSearchProvider {
		t.Errorf("DefaultSearchProvider = %q, want %q", got.DefaultSearchProvider, want.DefaultSearchProvider)
	}
}

func TestConfig_DefaultSearchProviderResolved(t *testing.T) {
	cfg := &Config{SearchProviders: []string{"brave", "searxng"}}
	Normalize(cfg)
	if cfg.DefaultSearchProvider != "brave" {
		t.Errorf("DefaultSearchProvider = %q, want %q", cfg.DefaultSearchProvider, "brave")
	}

	empty := &Config{}
	Normalize(empty)
	if empty.DefaultSearchProvider != "searxng" {
		t.Errorf("DefaultSearchProvider (empty providers) = %q, want %q", empty.DefaultSearchProvider, "searxng")
	}
}

func TestParseCacheMode(t *testing.T) {
	cases := map[string]string{
		"off":      "off",
		"record":   "record",
		" Replay ": "replay",
		"":         "off",
		"recrod":   "off",
	}
	for in, want := range cases {
		if got := parseCacheMode(in); got != want {
			t.Errorf("parseCacheMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoad_CacheDefaultsAndOverrides(t *testing.T) {
	t.Setenv("VEDETTA_CONFIG_PATH", t.TempDir()+"/config.local")
	cfg := Load()
	if cfg.CacheMode != "record" || cfg.CacheDir != "data/cache" {
		t.Errorf("defaults: mode=%q dir=%q, want record and data/cache", cfg.CacheMode, cfg.CacheDir)
	}
	if cfg.CacheSearchTTL != 24*time.Hour || cfg.CachePageTTL != 3*time.Hour {
		t.Errorf("default TTLs: search=%v page=%v", cfg.CacheSearchTTL, cfg.CachePageTTL)
	}

	t.Setenv("VEDETTA_CACHE_MODE", "replay")
	t.Setenv("VEDETTA_CACHE_DIR", "/tmp/corpus")
	t.Setenv("VEDETTA_CACHE_SEARCH_TTL", "2h")
	t.Setenv("VEDETTA_CACHE_PAGE_TTL", "not-a-duration")
	cfg = Load()
	if cfg.CacheMode != "replay" || cfg.CacheDir != "/tmp/corpus" || cfg.CacheSearchTTL != 2*time.Hour {
		t.Errorf("overrides: mode=%q dir=%q search=%v", cfg.CacheMode, cfg.CacheDir, cfg.CacheSearchTTL)
	}
	if cfg.CachePageTTL != 3*time.Hour {
		t.Errorf("invalid page TTL should fall back to default, got %v", cfg.CachePageTTL)
	}
}

// Map R2: a new installation starts as the home one we test with: the Deep's checks
// on, medium effort, the sampling both Qwen and DeepSeek recommend, the reader chain
// http then Jina. Each check is a switch to turn off.
func TestLoad_StartingSettingsAreTheTestedOnes(t *testing.T) {
	t.Setenv("VEDETTA_CONFIG_PATH", t.TempDir()+"/config.local")
	cfg := Load()
	on := map[string]bool{
		"review": cfg.DeepLoopReview, "review_previews": cfg.DeepLoopReviewPreviews,
		"cite_read": cfg.DeepLoopCiteRead, "link_check": cfg.DeepLoopLinkCheck,
		"quotes": cfg.DeepLoopQuotes, "session_pages": cfg.DeepLoopSessionPages,
		"quote_fallback": cfg.DeepLoopQuoteFallback, "read_date": cfg.CacheReadDate,
		"keep_reasoning": cfg.DeepLoopKeepReasoning, "rerank": cfg.DeepLoopRerank,
	}
	for name, v := range on {
		if !v {
			t.Errorf("%s is off by default, want on", name)
		}
	}
	off := map[string]bool{
		"compass": cfg.DeepLoopCompass, "review_lines": cfg.DeepLoopReviewLines,
		"push": cfg.DeepLoopPush, "grant": cfg.DeepLoopGrant, "name_words": cfg.DeepLoopNameWords, "preset_it": cfg.PresetIT,
	}
	for name, v := range off {
		if v {
			t.Errorf("%s is on by default, want off", name)
		}
	}
	if cfg.LangDetect != 2 || cfg.HTTPReaderVersion != 1 {
		t.Errorf("lang_detect=%d http_reader=%d, want 2 and 1", cfg.LangDetect, cfg.HTTPReaderVersion)
	}
	if cfg.LLMEffortDeep != "medium" || cfg.DeepLoopTemperature != "1.0" || cfg.DeepLoopTopP != 0.95 || cfg.DeepLoopTopK != 0 {
		t.Errorf("effort=%q temperature=%q top_p=%v top_k=%d, want medium, 1.0, 0.95, 0",
			cfg.LLMEffortDeep, cfg.DeepLoopTemperature, cfg.DeepLoopTopP, cfg.DeepLoopTopK)
	}
	if len(cfg.ReaderProviders) != 2 || cfg.ReaderProviders[0] != "http" || cfg.ReaderProviders[1] != "jina" {
		t.Errorf("reader chain = %v, want [http jina]", cfg.ReaderProviders)
	}

	// a check can still be turned off
	t.Setenv("VEDETTA_DEEP_LOOP_LINK_CHECK", "false")
	t.Setenv("VEDETTA_DEEP_LOOP_TEMPERATURE", "model")
	cfg = Load()
	if cfg.DeepLoopLinkCheck || cfg.DeepLoopTemperature != "model" {
		t.Errorf("overrides: link_check=%t temperature=%q", cfg.DeepLoopLinkCheck, cfg.DeepLoopTemperature)
	}
}

func TestSaveLoadLocalConfig_ExaFields(t *testing.T) {
	path := t.TempDir() + "/config.local"
	want := &Config{ExaAPIKey: "exa-key", ExaSearchType: "lite", ExaUserLocation: "IT"}
	if err := SaveLocalConfig(path, want); err != nil {
		t.Fatalf("SaveLocalConfig failed: %v", err)
	}
	got := &Config{ExaSearchType: "auto"}
	loadLocalConfig(path, got)
	if got.ExaAPIKey != want.ExaAPIKey || got.ExaSearchType != "lite" || got.ExaUserLocation != "IT" {
		t.Errorf("got %q %q %q", got.ExaAPIKey, got.ExaSearchType, got.ExaUserLocation)
	}
}

func TestSaveLoadLocalConfig_BrowserCDPURL(t *testing.T) {
	path := t.TempDir() + "/config.local"
	if err := SaveLocalConfig(path, &Config{BrowserCDPURL: "ws://127.0.0.1:9222"}); err != nil {
		t.Fatalf("SaveLocalConfig failed: %v", err)
	}
	got := &Config{}
	loadLocalConfig(path, got)
	if got.BrowserCDPURL != "ws://127.0.0.1:9222" {
		t.Errorf("BrowserCDPURL = %q", got.BrowserCDPURL)
	}
}

// Map R3: the two experimental levers are saved only once chosen in Settings, and
// then they win over .env; not chosen, the .env value holds.
func TestLocalConfig_ExperimentalLevers(t *testing.T) {
	path := t.TempDir() + "/config.local"
	if err := SaveLocalConfig(path, &Config{DeepLoopPush: true}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "deep_loop_push") || strings.Contains(string(raw), "deep_loop_review_lines") {
		t.Fatalf("levers not chosen in Settings were saved: %s", raw)
	}

	t.Setenv("VEDETTA_CONFIG_PATH", path)
	t.Setenv("VEDETTA_DEEP_LOOP_PUSH", "true")
	if cfg := Load(); !cfg.DeepLoopPush || cfg.DeepLoopPushChosen {
		t.Fatalf("not chosen: push=%t chosen=%t, want the .env value", cfg.DeepLoopPush, cfg.DeepLoopPushChosen)
	}

	chosen := &Config{DeepLoopPush: false, DeepLoopPushChosen: true, DeepLoopReviewLines: true, DeepLoopReviewLinesChosen: true}
	if err := SaveLocalConfig(path, chosen); err != nil {
		t.Fatal(err)
	}
	cfg := Load()
	if cfg.DeepLoopPush || !cfg.DeepLoopPushChosen {
		t.Errorf("chosen off in Settings, on in .env: push=%t, want off", cfg.DeepLoopPush)
	}
	if !cfg.DeepLoopReviewLines || !cfg.DeepLoopReviewLinesChosen {
		t.Errorf("review lines chosen on: got %t", cfg.DeepLoopReviewLines)
	}
	if cfg.DeepLoopGrant || cfg.DeepLoopGrantChosen {
		t.Errorf("special permission never chosen: got %t", cfg.DeepLoopGrant)
	}
	if err := SaveLocalConfig(path, &Config{DeepLoopGrant: true, DeepLoopGrantChosen: true}); err != nil {
		t.Fatal(err)
	}
	if cfg := Load(); !cfg.DeepLoopGrant || !cfg.DeepLoopGrantChosen {
		t.Error("special permission chosen on was not loaded")
	}
}

// Map R6: a new installation answers only from this machine.
func TestBindDefaultAndReach(t *testing.T) {
	t.Setenv("VEDETTA_CONFIG_PATH", t.TempDir()+"/config.local")
	cfg := Load()
	if cfg.Bind != "127.0.0.1" || cfg.AccessCode != "" {
		t.Errorf("defaults: bind %q, code set %t", cfg.Bind, cfg.AccessCode != "")
	}
	for bind, want := range map[string]bool{
		"127.0.0.1": false, "localhost": false, "::1": false, "[::1]": false, "127.0.0.2": false,
		"0.0.0.0": true, "::": true, "192.168.1.10": true, "100.64.10.20": true, "": true,
	} {
		if got := ListensBeyondMachine(bind); got != want {
			t.Errorf("ListensBeyondMachine(%q) = %t, want %t", bind, got, want)
		}
	}
	// map R8: in a container the compose says where the port is published
	if got := (&Config{Bind: "0.0.0.0"}).ReachAddr(); got != "0.0.0.0" {
		t.Errorf("no publish: reach %q, want the bind address", got)
	}
	t.Setenv("VEDETTA_BIND", "0.0.0.0")
	t.Setenv("VEDETTA_PUBLISH", " 127.0.0.1 ")
	if c := Load(); c.ReachAddr() != "127.0.0.1" || ListensBeyondMachine(c.ReachAddr()) {
		t.Errorf("published on 127.0.0.1: reach %q", c.ReachAddr())
	}
	t.Setenv("VEDETTA_PUBLISH", "0.0.0.0")
	if c := Load(); !ListensBeyondMachine(c.ReachAddr()) {
		t.Errorf("published on 0.0.0.0: reach %q should count as reachable", c.ReachAddr())
	}
	t.Setenv("VEDETTA_CORS_ORIGINS", " https://a.example.org/ , http://b.lan:8080")
	if cfg := Load(); len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[0] != "https://a.example.org" || cfg.CORSOrigins[1] != "http://b.lan:8080" {
		t.Errorf("CORS origins = %v", cfg.CORSOrigins)
	}
}

// Map R8: the first run shows on a new installation only. A config.local from
// before R8 counts as done, and VEDETTA_SETUP=done skips it for scripts.
func TestSetupDone(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.local"
	t.Setenv("VEDETTA_CONFIG_PATH", path)
	if Load().SetupDone {
		t.Error("new installation: the first run should show")
	}
	t.Setenv("VEDETTA_SETUP", "done")
	if !Load().SetupDone {
		t.Error("VEDETTA_SETUP=done: the first run should be skipped")
	}
	t.Setenv("VEDETTA_SETUP", "")
	if err := os.WriteFile(path, []byte(`{"searxng_url":"http://server:8888"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !Load().SetupDone {
		t.Error("config.local from before R8: should count as done")
	}
	cfg := Load()
	cfg.SetupDone = false
	if err := SaveLocalConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if Load().SetupDone {
		t.Error("saved as not done (\"Rifai la prima accensione\"): the first run should show again")
	}
	t.Setenv("VEDETTA_SETUP", "done")
	if !Load().SetupDone {
		t.Error("VEDETTA_SETUP=done wins over the file")
	}
}
