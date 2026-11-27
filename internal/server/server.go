package server

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"vedetta/config"
	"vedetta/internal/agent"
	"vedetta/web"
)

type Server struct {
	cfg      *config.Config
	engine   *agent.Engine
	handlers *Handlers
	srv      *http.Server
}

func NewServer(cfg *config.Config, engine *agent.Engine) *Server {
	return &Server{
		cfg:      cfg,
		engine:   engine,
		handlers: NewHandlers(cfg, engine),
	}
}

// Handler is every route behind the guard (map R6): host, other sites, body size,
// access code.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/health", s.handlers.Health)
	mux.HandleFunc("/api/search", s.handlers.Search)
	mux.HandleFunc("/api/sessions", s.handlers.ListSessions)
	mux.HandleFunc("/api/sessions/", s.handlers.SessionItem)
	mux.HandleFunc("/api/config", s.handlers.Config)
	mux.HandleFunc("/api/capabilities", s.handlers.Capabilities)
	mux.HandleFunc("/api/services", s.handlers.Services)
	mux.HandleFunc("/api/services/models", s.handlers.ServiceModels)
	mux.HandleFunc("/api/cache", s.handlers.Cache)
	mux.HandleFunc("/api/cache/clear", s.handlers.CacheClear)
	mux.HandleFunc("/api/data", s.handlers.Data)
	mux.HandleFunc("/api/data/clear-searches", s.handlers.DataClearSearches)
	mux.HandleFunc("/api/auth", s.handlers.AuthState)
	mux.HandleFunc("/api/login", s.handlers.Login)
	mux.HandleFunc("/api/access", s.handlers.SetCodes)
	mux.HandleFunc("/api/logout", s.handlers.Logout)
	mux.HandleFunc("/api/setup/discover", s.handlers.Discover) // map R8

	// Embedded Web UI
	webFS, err := web.GetFileSystem()
	if err != nil {
		log.Printf("Warning: failed to mount embedded UI: %v", err)
	} else {
		fileServer := http.FileServer(webFS)
		mux.Handle("/", fileServer)
	}

	return s.guard(mux)
}

func (s *Server) Start() error {
	writeTimeout := searchTimeLimit(s.cfg)
	addr := net.JoinHostPort(strings.Trim(s.cfg.Bind, "[]"), s.cfg.Port)

	s.srv = &http.Server{
		Addr:         addr,
		Handler:      s.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: writeTimeout, // Long-running agentic SSE streams
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("⚡ Vedetta server starting on http://%s", addr)
	switch {
	case !config.ListensBeyondMachine(s.cfg.ReachAddr()) && s.cfg.Publish != "":
		log.Printf("   Reachable only from this machine (port published on %s, VEDETTA_PUBLISH)", s.cfg.Publish)
	case !config.ListensBeyondMachine(s.cfg.ReachAddr()):
		log.Printf("   Reachable only from this machine (VEDETTA_BIND=%s)", s.cfg.Bind)
	case s.cfg.AccessCode == "" && s.cfg.AccessCodeHash == "":
		log.Printf("   WARNING: reachable from other devices without an access code: anyone on the network can search, read the Library and use paid services. Set an access code in Settings, or VEDETTA_ACCESS_CODE, to close it.")
	default:
		log.Printf("   Reachable from other devices, with an access code")
	}
	log.Printf("   SearXNG: %s", s.cfg.SearxngURL)
	if s.cfg.DegoogURL != "" {
		log.Printf("   degoog:  %s", s.cfg.DegoogURL)
	}
	log.Printf("   Jina:    %s", s.cfg.JinaURL)
	log.Printf("   LLM:     %s (Model: %s)", s.cfg.LLMBaseURL, s.cfg.LLMModel)

	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.handlers.runs.shutdown(ctx)
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}
