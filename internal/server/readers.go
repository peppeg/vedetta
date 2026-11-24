package server

import (
	"strings"
	"time"

	"vedetta/config"
	"vedetta/internal/reader"
)

// PDFReaderConfig is the built-in PDF reader configuration for cfg, nil when
// it is disabled. Startup and the settings reload both use it, so saving the
// settings keeps the PDF reader (V25-PDF.4).
func PDFReaderConfig(cfg *config.Config) *reader.PDFReaderConfig {
	if !cfg.PDFReader {
		return nil
	}
	return &reader.PDFReaderConfig{
		MaxBytes: int64(cfg.PDFMaxBytes),
		MaxPages: cfg.PDFMaxPages,
		Timeout:  time.Duration(cfg.PDFTimeoutSecs) * time.Second,
		Idle:     time.Duration(cfg.PDFIdleSecs) * time.Second,
		CacheDir: cfg.PDFCacheDir,
	}
}

// BrowserReaderConfig is the "browser" reader configuration for cfg, nil when
// no browser address is configured (docs/SPEC_BROWSER_CDP.md).
func BrowserReaderConfig(cfg *config.Config) *reader.BrowserReaderConfig {
	if strings.TrimSpace(cfg.BrowserCDPURL) == "" {
		return nil
	}
	return &reader.BrowserReaderConfig{
		CDPURL:      strings.TrimSpace(cfg.BrowserCDPURL),
		MaxPages:    cfg.BrowserMaxPages,
		PageTimeout: time.Duration(cfg.BrowserPageTimeoutSecs) * time.Second,
	}
}
