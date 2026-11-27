package search

import (
	"fmt"
	"strings"
	"sync"
)

// ProviderConfig carries all configuration options needed to initialize search providers.
type ProviderConfig struct {
	ProviderName     string   // Legacy single provider name
	FallbackProvider string   // Legacy fallback provider name
	Providers        []string // Ordered list of providers (REV 2)
	FallbackOnEmpty  bool     // Advance chain on 0 results if true
	SearxngURL       string
	DegoogURL        string
	BraveAPIKey      string
	TavilyAPIKey     string
	SerperAPIKey     string
	ExaAPIKey        string
	ExaSearchType    string // "auto" (default) or "lite"
	ExaUserLocation  string // ISO country code sent as userLocation, "" = none
}

// FactoryFunc is a constructor function for a search Provider.
type FactoryFunc func(cfg ProviderConfig) (Provider, error)

var (
	registryMu sync.RWMutex
	factories  = make(map[string]FactoryFunc)
)

func init() {
	Register("searxng", func(cfg ProviderConfig) (Provider, error) {
		url := cfg.SearxngURL
		if url == "" {
			url = "http://localhost:8888"
		}
		return NewSearxngProvider(url), nil
	})

	Register("degoog", func(cfg ProviderConfig) (Provider, error) {
		if DegoogBase(cfg.DegoogURL) == "" {
			return nil, fmt.Errorf("degoog search requires DEGOOG_URL")
		}
		return NewDegoogProvider(cfg.DegoogURL), nil
	})

	Register("brave", func(cfg ProviderConfig) (Provider, error) {
		if strings.TrimSpace(cfg.BraveAPIKey) == "" {
			return nil, fmt.Errorf("brave search requires BRAVE_API_KEY")
		}
		return NewBraveProvider(cfg.BraveAPIKey), nil
	})

	Register("tavily", func(cfg ProviderConfig) (Provider, error) {
		if strings.TrimSpace(cfg.TavilyAPIKey) == "" {
			return nil, fmt.Errorf("tavily search requires TAVILY_API_KEY")
		}
		return NewTavilyProvider(cfg.TavilyAPIKey), nil
	})

	Register("serper", func(cfg ProviderConfig) (Provider, error) {
		if strings.TrimSpace(cfg.SerperAPIKey) == "" {
			return nil, fmt.Errorf("serper search requires SERP_API_KEY")
		}
		return NewSerperProvider(cfg.SerperAPIKey), nil
	})

	Register("exa", func(cfg ProviderConfig) (Provider, error) {
		if strings.TrimSpace(cfg.ExaAPIKey) == "" {
			return nil, fmt.Errorf("exa search requires EXA_API_KEY")
		}
		return NewExaProvider(cfg.ExaAPIKey, cfg.ExaSearchType, cfg.ExaUserLocation), nil
	})

	Register("europepmc", func(cfg ProviderConfig) (Provider, error) {
		return NewAcademicProvider(), nil
	})

	Register("academic", func(cfg ProviderConfig) (Provider, error) {
		return NewAcademicProvider(), nil
	})
}

// Register registers a new search provider factory.
func Register(name string, factory FactoryFunc) {
	registryMu.Lock()
	defer registryMu.Unlock()
	factories[strings.ToLower(strings.TrimSpace(name))] = factory
}

// Get instantiates a registered provider by name.
func Get(name string, cfg ProviderConfig) (Provider, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	fn, ok := factories[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("unknown search provider: %q", name)
	}
	return fn(cfg)
}

// NewProvider resolves and constructs a Provider based on configuration,
// implementing REV 2 B-R1.2 (removal of inferences) and B-R1.5 (ordered chain construction).
func NewProvider(cfg ProviderConfig) (Provider, error) {
	var names []string

	if len(cfg.Providers) > 0 {
		for _, raw := range cfg.Providers {
			n := strings.ToLower(strings.TrimSpace(raw))
			if n != "" && n != "none" {
				names = append(names, n)
			}
		}
	} else {
		// Legacy migration path
		provName := strings.ToLower(strings.TrimSpace(cfg.ProviderName))
		if provName == "" || provName == "composite" {
			provName = "searxng"
		}
		names = append(names, provName)

		fbName := strings.ToLower(strings.TrimSpace(cfg.FallbackProvider))
		if fbName != "" && fbName != "none" && fbName != provName {
			names = append(names, fbName)
		}
	}

	if len(names) == 0 {
		names = []string{"searxng"}
	}

	var chain []Provider
	for _, n := range names {
		p, err := Get(n, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize provider %q in chain: %w", n, err)
		}
		chain = append(chain, p)
	}

	academic := NewAcademicProvider()

	return NewCompositeProvider(CompositeConfig{
		Name:            chain[0].Name(),
		Providers:       chain,
		Academic:        academic,
		FallbackOnEmpty: cfg.FallbackOnEmpty,
	}), nil
}
