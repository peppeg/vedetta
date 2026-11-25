package main

import (
	"context"
	_ "embed"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"vedetta/config"
	"vedetta/internal/agent"
	"vedetta/internal/cache"
	"vedetta/internal/llm"
	"vedetta/internal/logquiet"
	"vedetta/internal/reader"
	"vedetta/internal/search"
	"vedetta/internal/server"
	"vedetta/internal/storage"
)

//go:embed VERSION
var versionFile string

// version is overridable at build time via -ldflags "-X main.version=...".
var version string

func effectiveVersion() string {
	if v := strings.TrimSpace(version); v != "" {
		return v
	}
	return strings.TrimSpace(versionFile)
}

// healthAddr is where `vedetta health` asks: the bind address, or this machine
// when Vedetta listens on every interface.
func healthAddr(bind, port string) string {
	host := strings.Trim(strings.TrimSpace(bind), "[]")
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/health"
}

// healthCheck answers `vedetta health` (map R8): 0 when the server says it is
// up, 1 otherwise. The image has no curl; the container health check runs this.
func healthCheck() int {
	cfg := config.Load()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(healthAddr(cfg.Bind, cfg.Port))
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(healthCheck())
	}
	logquiet.Install()
	log.Println("Starting Vedetta Answering Engine...")

	cfg := config.Load()
	cfg.Version = effectiveVersion()
	logquiet.Set(cfg.QuietLog) // map R7

	store, err := storage.NewStorage("data/vedetta.db")
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}
	defer store.Close()

	searchProvider, err := search.NewProvider(search.ProviderConfig{
		Providers:       cfg.SearchProviders,
		FallbackOnEmpty: cfg.SearchFallbackOnEmpty,
		SearxngURL:      cfg.SearxngURL,
		BraveAPIKey:     cfg.BraveAPIKey,
		TavilyAPIKey:    cfg.TavilyAPIKey,
		SerperAPIKey:    cfg.SerperAPIKey,
		ExaAPIKey:       cfg.ExaAPIKey,
		ExaSearchType:   cfg.ExaSearchType,
		ExaUserLocation: cfg.ExaUserLocation,
	})
	if err != nil {
		log.Fatalf("Failed to initialize search provider chain %v: %v", cfg.SearchProviders, err)
	}
	log.Printf("Initialized search provider chain: %v (fallbackOnEmpty=%t)", cfg.SearchProviders, cfg.SearchFallbackOnEmpty)

	reader.SetHTTPReaderVersion(cfg.HTTPReaderVersion)
	if cfg.HTTPReaderVersion >= 2 {
		log.Printf("HTML extraction of the http and browser readers: version 2 (links, article header, table cells; VEDETTA_HTTP_READER)")
	}
	readerCfg := reader.ReaderConfig{
		Providers:        cfg.ReaderProviders,
		JinaBaseURL:      cfg.JinaURL,
		FirecrawlAPIKey:  cfg.FirecrawlAPIKey,
		FirecrawlBaseURL: cfg.FirecrawlBaseURL,
		ExaAPIKey:        cfg.ExaAPIKey,
		PDF:              server.PDFReaderConfig(cfg),
		Browser:          server.BrowserReaderConfig(cfg),
	}
	contentReader, err := reader.NewReader(readerCfg)
	if err != nil {
		log.Fatalf("Failed to initialize reader provider chain %v: %v", cfg.ReaderProviders, err)
	}
	log.Printf("Initialized reader provider chain: %v (built-in PDF reader: %t, max pages %d, 0 = all)", cfg.ReaderProviders, cfg.PDFReader, cfg.PDFMaxPages)
	{
		// Doctor for the fallback readers of the chain (docs/SPEC_BROWSER_CDP.md §3, V25-F0.2).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		name, status, msg := reader.ChainDoctor(ctx, readerCfg)
		cancel()
		if name == "" {
			name = "fallback reader"
		}
		log.Printf("[doctor] %s %s: %s", name, status, msg)
	}
	var reranker search.Reranker = search.NewBM25Reranker()
	var embedClient *search.EmbeddingClient
	if cfg.EmbeddingsURL != "" {
		embedClient = search.NewEmbeddingClient(cfg.EmbeddingsURL, cfg.EmbeddingsAPIKey, "")
		reranker = search.NewEmbeddingReranker(embedClient, search.NewBM25Reranker())
		log.Printf("Enabled BYOK Embedding Reranker with endpoint: %s", cfg.EmbeddingsURL)
	} else {
		log.Printf("Using built-in Pure Go BM25 Reranker (zero overhead)")
	}

	if cfg.PresetIT {
		search.SetPresetActive("it", true)
		log.Println("Active Level 3 Domain Preset: 'it' (Normativa & Impianti Italia)")
	}

	if cfg.IntentRerank {
		opts := search.IntentRerankerOptions{
			AuthorityBoost:   cfg.AuthorityBoost,
			ProjectPenalty:   cfg.ProjectPenalty,
			MunicipalPenalty: cfg.MunicipalPenalty,
			ProjectBoost:     0.50,
		}
		if cfg.PresetIT {
			opts.Preset = "it"
		}
		reranker = search.NewIntentReranker(reranker, opts)
		log.Printf("Wrapped with Deterministic Intent & Authority Reranker (boost=%.2f, proj_penalty=%.2f, mun_penalty=%.2f, preset=%s)",
			cfg.AuthorityBoost, cfg.ProjectPenalty, cfg.MunicipalPenalty, opts.Preset)
	}

	llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel).
		WithRetryPolicy(llm.NewRetryPolicy(cfg.LLMRetries, cfg.LLMRetryMaxWaitSecs)).
		WithMaxTokens(cfg.LLMMaxTokens).
		WithContextTokens(config.MainContextTokens(cfg))
	log.Printf("LLM retries: %d before the response, single wait at most %ds", cfg.LLMRetries, cfg.LLMRetryMaxWaitSecs)
	go func() {
		// Doctor for the model's memory (DL3, docs/SPEC_DL3_MEMORIA_DEL_MODELLO.md §3.5).
		// Apart from the start: a server that is slow to answer must not hold it.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := llmClient.ModelOutputCap(ctx); err != nil {
			log.Printf("[doctor] model memory: /models unusable (%v); read again at a later question", err)
			return
		}
		info := llmClient.ContextInfo(ctx)
		if info.Known() {
			log.Printf("[doctor] model memory: %d tokens (%s)", info.Tokens, info.Source)
		} else {
			log.Printf("[doctor] model memory: not stated by the server, using %d tokens; set it with VEDETTA_LLM_CONTEXT_TOKENS or the service's context_tokens", info.Tokens)
		}
	}()

	engine := agent.NewEngine(searchProvider, contentReader, llmClient, store)
	engine.SetReranker(reranker)
	if cfg.CacheMode != "off" {
		cacheStore, err := cache.NewStore(cfg.CacheDir)
		if err != nil {
			log.Fatalf("Failed to open search cache: %v", err)
		}
		c := cache.New(cacheStore, cfg.CacheMode, cfg.CacheSearchTTL, cfg.CachePageTTL)
		engine.SetCache(c)
		log.Printf("Search and page cache: mode=%s dir=%s (search ttl %v, page ttl %v, expired entries removed: %t)",
			cfg.CacheMode, cfg.CacheDir, cfg.CacheSearchTTL, cfg.CachePageTTL, cfg.CacheMode == cache.ModeRecord && cfg.CachePrune)
		// map R2: what has expired is never used again, so it goes (hourly)
		if cfg.CacheMode == cache.ModeRecord && cfg.CachePrune {
			go c.PruneEvery(time.Hour)
		}
	}
	engine.SetMaxResults(cfg.SearxngMaxResults)
	engine.SetDeepSessionTimeout(cfg.DeepSessionTimeoutSecs)
	engine.SetSynthesisTimeout(cfg.SynthesisTimeoutSecs)
	engine.SetSynthContentStall(time.Duration(cfg.SynthContentStallSecs) * time.Second)
	engine.SetDeepLoopMaxCalls(cfg.DeepLoopMaxCalls)
	engine.SetDeepLoopSampling(cfg.DeepLoopTemperature, cfg.DeepLoopTopP, cfg.DeepLoopTopK, cfg.DeepLoopKeepReasoning)
	engine.SetDeepLoopReview(cfg.DeepLoopReview)
	engine.SetDeepLoopCompass(cfg.DeepLoopCompass)
	engine.SetDeepLoopReviewPreviews(cfg.DeepLoopReviewPreviews)
	engine.SetDeepLoopCiteRead(cfg.DeepLoopCiteRead)
	engine.SetDeepLoopLinkCheck(cfg.DeepLoopLinkCheck)
	engine.SetDeepLoopRerank(cfg.DeepLoopRerank)
	engine.SetDeepLoopMaxResults(cfg.DeepLoopMaxResults)
	engine.SetDeepLoopSearchLang(cfg.DeepLoopSearchLang)
	engine.SetDeepLoopQuoteFallback(cfg.DeepLoopQuoteFallback)
	engine.SetDeepLoopSessionPages(cfg.DeepLoopSessionPages)
	engine.SetDeepLoopQuotes(cfg.DeepLoopQuotes)
	engine.SetDeepLoopPush(cfg.DeepLoopPush)
	engine.SetDeepLoopGrant(cfg.DeepLoopGrant)
	engine.SetDeepThought(cfg.DeepThought)
	engine.SetCacheReadDate(cfg.CacheReadDate)
	agent.SetNameWords(cfg.DeepLoopNameWords)
	agent.SetLangDetectV2(cfg.LangDetect >= 2)
	engine.SetDeepLoopReviewLines(cfg.DeepLoopReviewLines)
	log.Printf("   Deep a ciclo: max %d calls, temperature %q, top_p %g, top_k %d, keep reasoning %t, review %t, review previews %t, compass %t, cite read %t, link check %t, review lines %t",
		cfg.DeepLoopMaxCalls, cfg.DeepLoopTemperature, cfg.DeepLoopTopP, cfg.DeepLoopTopK, cfg.DeepLoopKeepReasoning, cfg.DeepLoopReview, cfg.DeepLoopReviewPreviews, cfg.DeepLoopCompass, cfg.DeepLoopCiteRead, cfg.DeepLoopLinkCheck, cfg.DeepLoopReviewLines)
	engine.SetDeepEffort(llm.ParseEffort(cfg.LLMEffortDeep))

	srv := server.NewServer(cfg, engine)

	// Channel for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(); err != nil && err.Error() != "http: Server closed" {
			log.Fatalf("Server failure: %v", err)
		}
	}()

	<-stop
	log.Println("\nShutting down Vedetta gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Println("Vedetta stopped. So long, and thanks for all the fish.")
}
