package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedVersion_Format(t *testing.T) {
	v := effectiveVersion()
	if v == "" {
		t.Fatal("effectiveVersion() returned empty string")
	}
	re := regexp.MustCompile(`^\d+\.\d+\.\d+`)
	if !re.MatchString(v) {
		t.Fatalf("effectiveVersion() = %q, want semver-like format ^\\d+\\.\\d+\\.\\d+", v)
	}
}

func TestEffectiveVersion_LdflagsWins(t *testing.T) {
	prev := version
	defer func() { version = prev }()

	version = "9.9.9-ld"
	if got := effectiveVersion(); got != "9.9.9-ld" {
		t.Fatalf("effectiveVersion() = %q, want %q (ldflags override must win)", got, "9.9.9-ld")
	}
}

// Map R8: the container health check asks this machine when Vedetta listens everywhere.
func TestHealthAddr(t *testing.T) {
	for _, c := range []struct{ bind, want string }{
		{"0.0.0.0", "http://127.0.0.1:3030/api/health"},
		{"::", "http://127.0.0.1:3030/api/health"},
		{"[::]", "http://127.0.0.1:3030/api/health"},
		{"", "http://127.0.0.1:3030/api/health"},
		{"127.0.0.1", "http://127.0.0.1:3030/api/health"},
		{"192.168.1.10", "http://192.168.1.10:3030/api/health"},
		{"::1", "http://[::1]:3030/api/health"},
	} {
		if got := healthAddr(c.bind, "3030"); got != c.want {
			t.Errorf("healthAddr(%q) = %q, want %q", c.bind, got, c.want)
		}
	}
}

// Map R8: the composes pull the image of the version in VERSION, so a new version
// cannot ship with a compose that still pulls the old one.
func TestComposeImageMatchesVersion(t *testing.T) {
	want := "ghcr.io/peppeg/vedetta:" + strings.TrimSpace(versionFile)
	files, _ := filepath.Glob("deploy/*/compose.yml")
	if len(files) == 0 {
		t.Fatal("no deploy/*/compose.yml")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "image: "+want+"\n") {
			t.Errorf("%s does not pull %s", f, want)
		}
	}
}
