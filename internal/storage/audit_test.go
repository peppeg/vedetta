package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// External audit 24/11/2026: a new database is for its user only, and a title is cut
// between letters.
func TestNewStorage_PrivateToItsUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "vedetta.db")
	s, err := NewStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateSession("domanda", "deep"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", filepath.Base(p), fi, err)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("folder: %v", fi.Mode())
	}
}

func TestNewStorage_KeepsTheModeOfAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vedetta.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func TestCreateSession_TitleCutBetweenLetters(t *testing.T) {
	s, err := NewStorage(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sess, err := s.CreateSession(strings.Repeat("è", 119)+"àù", "deep")
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(sess.Title) || !strings.HasSuffix(sess.Title, "à...") {
		t.Errorf("title %q", sess.Title)
	}
}
