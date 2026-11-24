package server

import (
	"testing"
	"time"

	"vedetta/config"
)

func TestPDFReaderConfig(t *testing.T) {
	if got := PDFReaderConfig(&config.Config{PDFReader: false}); got != nil {
		t.Errorf("disabled: got %+v", got)
	}
	got := PDFReaderConfig(&config.Config{PDFReader: true, PDFMaxBytes: 1 << 20, PDFMaxPages: 3, PDFTimeoutSecs: 7, PDFIdleSecs: 9, PDFCacheDir: "c"})
	if got == nil || got.MaxBytes != 1<<20 || got.MaxPages != 3 || got.Timeout != 7*time.Second || got.Idle != 9*time.Second || got.CacheDir != "c" {
		t.Errorf("enabled: got %+v", got)
	}
}

func TestBrowserReaderConfig(t *testing.T) {
	if got := BrowserReaderConfig(&config.Config{BrowserCDPURL: "  "}); got != nil {
		t.Errorf("no address: got %+v", got)
	}
	got := BrowserReaderConfig(&config.Config{BrowserCDPURL: " ws://h:9222 ", BrowserMaxPages: 3, BrowserPageTimeoutSecs: 15})
	if got == nil || got.CDPURL != "ws://h:9222" || got.MaxPages != 3 || got.PageTimeout != 15*time.Second {
		t.Errorf("got %+v", got)
	}
}
