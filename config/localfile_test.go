package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// External audit 24/11/2026, SEC-05: a damaged config.local must not start Vedetta
// without its codes.
func TestLoadLocalConfig_DamagedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.local")
	if err := loadLocalConfig(path, &Config{}); err != nil {
		t.Errorf("no file yet, a new installation: %v", err)
	}
	for name, raw := range map[string]string{
		"cut":   `{"access_code_hash":"pbkdf2-sha256$1$a$b","setup_done":true`,
		"empty": "",
		"other": "niente",
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{}
		if err := loadLocalConfig(path, cfg); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: error %v, want one that names the file", name, err)
		}
	}
	if err := loadLocalConfig(dir, &Config{}); err == nil {
		t.Error("a file that cannot be read is not a new installation")
	}
}

func TestSaveLocalConfig_WholeOrNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.local")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{AccessCodeHash: "pbkdf2-sha256$1$a$b", LLMBaseURL: "http://m/v1", LLMModel: "m"}
	if err := SaveLocalConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v: the file holds keys, 0600 also when it was wider", fi.Mode(), err)
	}
	back := &Config{}
	if err := loadLocalConfig(path, back); err != nil || back.AccessCodeHash != cfg.AccessCodeHash {
		t.Errorf("read back: %v %q", err, back.AccessCodeHash)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	// a link to the file stays a link
	link := filepath.Join(t.TempDir(), "config.local")
	if err := os.Symlink(path, link); err != nil {
		t.Skip(err)
	}
	if err := SaveLocalConfig(link, &Config{AccessCodeHash: "nuovo"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced by a file")
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "nuovo") {
		t.Error("the file behind the link did not change")
	}

	// a save that fails leaves nothing behind
	taken := filepath.Join(dir, "taken")
	if err := os.MkdirAll(filepath.Join(taken, "full"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveLocalConfig(taken, cfg); err == nil {
		t.Error("no error where a folder is in the way")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Errorf("left behind after a failed save: %v", left)
	}
	if err := SaveLocalConfig(filepath.Join(path, "x", "config.local"), cfg); err == nil {
		t.Error("no error")
	}
}
