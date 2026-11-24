package reader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeStatus is the honest health report for a single content reader.
// Status is one of: ok, down, unknown, configured.
type ProbeStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Problem names what is wrong, as a code the page says in words (map R8):
	// unreachable, not_jina.
	Problem string `json:"problem,omitempty"`
}

// ProbeReader probes the given reader provider name using the supplied
// configuration. Built-in readers are always "ok"; network readers are probed
// with a short timeout; keyed readers report "configured"/"unknown" based on
// key presence.
func ProbeReader(ctx context.Context, name string, cfg ReaderConfig) ProbeStatus {
	clean := strings.ToLower(strings.TrimSpace(name))
	switch clean {
	case "http", "direct":
		return ProbeStatus{Name: clean, Status: "ok", Detail: "built-in"}
	case "jina", "jina-reader":
		return probeJina(ctx, cfg.JinaBaseURL)
	case "firecrawl":
		if strings.TrimSpace(cfg.FirecrawlAPIKey) != "" {
			return ProbeStatus{Name: clean, Status: "configured", Detail: "key present"}
		}
		return ProbeStatus{Name: clean, Status: "unknown", Detail: "no api key"}
	case "browser":
		cdpURL := ""
		if cfg.Browser != nil {
			cdpURL = cfg.Browser.CDPURL
		}
		status, msg := BrowserDoctor(ctx, cdpURL)
		return ProbeStatus{Name: clean, Status: status, Detail: msg}
	case "exa":
		if strings.TrimSpace(cfg.ExaAPIKey) != "" {
			return ProbeStatus{Name: clean, Status: "configured", Detail: "key present"}
		}
		return ProbeStatus{Name: clean, Status: "unknown", Detail: "no api key"}
	default:
		return ProbeStatus{Name: clean, Status: "unknown", Detail: "unknown reader"}
	}
}

// probeJina is the Jina Reader doctor (map R8): its home page, self-hosted or
// the cloud one, is a short text that names r.jina.ai; any other page means
// something else answers at that address.
func probeJina(ctx context.Context, rawURL string) ProbeStatus {
	base := strings.TrimSpace(rawURL)
	if base == "" {
		base = "http://localhost:3001"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return ProbeStatus{Name: "jina", Status: "down", Problem: "unreachable", Detail: err.Error()}
	}
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeStatus{Name: "jina", Status: "down", Problem: "unreachable", Detail: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return ProbeStatus{Name: "jina", Status: "down", Detail: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	if !strings.Contains(string(body), "r.jina.ai") {
		return ProbeStatus{Name: "jina", Status: "down", Problem: "not_jina", Detail: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	return ProbeStatus{Name: "jina", Status: "ok"}
}
