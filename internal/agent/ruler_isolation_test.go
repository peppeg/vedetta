package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// V25-QB: the ruler's hand-written expectations (scripts/expected.json) are for
// measuring only; the engine must find its own map from the question.
func TestEngineNeverReadsTheRuler(t *testing.T) {
	root := filepath.Join("..", "..")
	var files []string
	_ = filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	files = append(files, filepath.Join(root, "main.go"))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "expected.json") {
			t.Errorf("%s names expected.json: the engine must not read the ruler", path)
		}
	}
	if len(files) < 10 {
		t.Fatalf("only %d files checked: wrong root?", len(files))
	}
}
