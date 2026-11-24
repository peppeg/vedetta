package logquiet

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		`[deeploop] session=abc search lang=it query="prezzo bitcoin \"oggi\""`:                     `[deeploop] session=abc search lang=it query="…"`,
		`[deeploop] session=abc read url=https://www.coindesk.com/it/price/bitcoin status=ok`:       `[deeploop] session=abc read url=[address] status=ok`,
		`[reader] session=abc url=example.org/page served_by=http chars=20206`:                      `[reader] session=abc url=[address] served_by=http chars=20206`,
		`[search] error: Get "http://localhost:8888/search?q=mal+di+testa": dial tcp: refused`:      `[search] error: Get "…": dial tcp: refused`,
		`[deeploop] session=abc search relaxed: "\"x y\" site:a.org" -> "x y"`:                      `[deeploop] session=abc search relaxed: "…" -> "…"`,
		`[telemetry] session=abc deeploop turns=5 calls=7 searches=2 pages_read=5 closed_by=answer`: `[telemetry] session=abc deeploop turns=5 calls=7 searches=2 pages_read=5 closed_by=answer`,
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestWriterFollowsTheSwitch(t *testing.T) {
	var buf bytes.Buffer
	l := log.New(writer{&buf}, "", 0)
	Set(false)
	l.Printf("read url=%s", "https://a.example/x")
	Set(true)
	l.Printf("read url=%s", "https://a.example/y")
	Set(false)
	out := buf.String()
	if !strings.Contains(out, "a.example/x") || strings.Contains(out, "a.example/y") || !strings.Contains(out, "url=[address]") {
		t.Errorf("log = %q", out)
	}
}

// External audit 24/11/2026: the names of the sites in the errors of the network, and
// text whose closing quote is missing.
func TestRedact_HostNamesInErrors(t *testing.T) {
	for _, line := range []string{
		`[reader] http failed (Get "https://sito.example/pagina": dial tcp: lookup sito.example: no such host)`,
		`ssrf validation failed: failed to resolve host sito.example: lookup sito.example on 127.0.0.53:53: no such host`,
		`host sito.example resolves to private IP 10.0.0.1 (SSRF guard)`,
	} {
		if out := Redact(line); strings.Contains(out, "sito.example") {
			t.Errorf("%q -> %q", line, out)
		}
	}
	if out := Redact("lookup of the model: host unknown, resolve later"); out != "lookup of the model: host unknown, resolve later" {
		t.Errorf("an ordinary line changes: %q", out)
	}
}
