package reader

import (
	"fmt"
	"strings"
)

// ReaderConfig holds configuration for initializing content readers.
type ReaderConfig struct {
	Providers        []string
	JinaBaseURL      string
	FirecrawlAPIKey  string
	FirecrawlBaseURL string
	ExaAPIKey        string
	// PDF configures the built-in PDF reader that runs before the chain for PDF
	// documents (V25-PDF.4); nil disables it.
	PDF *PDFReaderConfig
	// Browser configures the "browser" reader (SPEC_BROWSER); nil = not configured.
	Browser *BrowserReaderConfig
}

// NewReader instantiates a Reader based on the ordered provider list.
// Supported provider names: "http" ("direct"), "jina" ("jina-reader"), "firecrawl", "exa", "browser".
func NewReader(cfg ReaderConfig) (Reader, error) {
	providers := cfg.Providers
	if len(providers) == 0 {
		providers = []string{"jina"}
	}

	var chain []Reader
	for _, p := range providers {
		pClean := strings.ToLower(strings.TrimSpace(p))
		switch pClean {
		case "http", "direct":
			chain = append(chain, NewDirectHTTPClient())
		case "jina", "jina-reader":
			chain = append(chain, NewJinaReader(cfg.JinaBaseURL))
		case "firecrawl":
			if strings.TrimSpace(cfg.FirecrawlAPIKey) == "" {
				return nil, fmt.Errorf("firecrawl reader requested but FIRECRAWL_API_KEY is not configured")
			}
			chain = append(chain, NewFirecrawlClient(cfg.FirecrawlAPIKey, cfg.FirecrawlBaseURL))
		case "browser":
			if cfg.Browser == nil || strings.TrimSpace(cfg.Browser.CDPURL) == "" {
				return nil, fmt.Errorf("browser reader requested but BROWSER_CDP_URL is not configured")
			}
			chain = append(chain, NewBrowserReader(*cfg.Browser))
		case "exa":
			if strings.TrimSpace(cfg.ExaAPIKey) == "" {
				return nil, fmt.Errorf("exa reader requested but EXA_API_KEY is not configured")
			}
			chain = append(chain, NewExaReader(cfg.ExaAPIKey))
		default:
			return nil, fmt.Errorf("unknown reader provider: %s (supported: http, browser, jina, firecrawl, exa)", p)
		}
	}

	var pdfReader Reader
	if cfg.PDF != nil {
		pdfReader = pdfReaderFor(*cfg.PDF)
	}
	return NewCompositeReader(CompositeReaderConfig{
		Chain:  chain,
		PubMed: NewEuropePMCReader(""),
		PDF:    pdfReader,
	}), nil
}
