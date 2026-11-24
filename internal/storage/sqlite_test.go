package storage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorage_SessionLifecycle(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "vedetta-test-storage-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "test.db")
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	defer store.Close()

	// 1. Create session
	sess, err := store.CreateSession("Test Speranza in Filosofia", "standard")
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	if sess.ID == "" || sess.Title != "Test Speranza in Filosofia" {
		t.Fatalf("invalid session returned: %+v", sess)
	}

	// 2. Add message turn
	msg := TurnMessage{
		SessionID: sess.ID,
		Role:      "assistant",
		Query:     "Cosa dice Bloch?",
		Content:   "Bloch parla del principio speranza...",
		Thinking:  "Sto pensando a Ernst Bloch...",
		Mode:      "standard",
		Focus:     []string{"academic", "web"},
	}
	if err := store.AddMessage(msg); err != nil {
		t.Fatalf("failed to add message: %v", err)
	}

	// 3. Get session with messages
	detail, err := store.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	if detail == nil || len(detail.Messages) != 1 {
		t.Fatalf("expected 1 message, got: %+v", detail)
	}
	if detail.Messages[0].Content != msg.Content {
		t.Errorf("content mismatch: want %q, got %q", msg.Content, detail.Messages[0].Content)
	}

	// 4. List sessions
	list, err := store.ListSessions()
	if err != nil {
		t.Fatalf("failed to list sessions: %v", err)
	}
	if len(list) != 1 || list[0].ID != sess.ID {
		t.Fatalf("expected list with session, got: %+v", list)
	}

	// 5. Delete session
	if err := store.DeleteSession(sess.ID); err != nil {
		t.Fatalf("failed to delete session: %v", err)
	}
	detailAfter, err := store.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get after delete error: %v", err)
	}
	if detailAfter != nil {
		t.Fatalf("expected nil after delete, got %+v", detailAfter)
	}
}

func TestTreePersistence(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "vedetta-test-tree-persist-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "tree_test.db")
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	defer store.Close()

	sess, err := store.CreateSession("Deep Tree Session", "deep")
	if err != nil {
		t.Fatalf("create session error: %v", err)
	}

	treeData := `{"root":{"id":"root","query":"Root Topic","depth":0},"nodes":[{"id":"n1","query":"Sub 1","depth":1}],"statuses":{"F1":"consensus"}}`
	msg := TurnMessage{
		SessionID: sess.ID,
		Role:      "assistant",
		Query:     "Root Topic",
		Content:   "Here is the deep report.",
		Mode:      "deep",
		Tree:      json.RawMessage(treeData),
	}

	if err := store.AddMessage(msg); err != nil {
		t.Fatalf("add message error: %v", err)
	}

	detail, err := store.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session error: %v", err)
	}
	if len(detail.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(detail.Messages))
	}

	savedMsg := detail.Messages[0]
	if len(savedMsg.Tree) == 0 {
		t.Fatalf("expected non-empty Tree in message")
	}

	var parsed struct {
		Root struct {
			ID string `json:"id"`
		} `json:"root"`
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
		Statuses map[string]string `json:"statuses"`
	}
	if err := json.Unmarshal(savedMsg.Tree, &parsed); err != nil {
		t.Fatalf("failed to unmarshal saved tree: %v", err)
	}
	if parsed.Root.ID != "root" || len(parsed.Nodes) != 1 || parsed.Nodes[0].ID != "n1" {
		t.Errorf("unexpected tree content: %+v", parsed)
	}
	if parsed.Statuses["F1"] != "consensus" {
		t.Errorf("unexpected status in tree: %v", parsed.Statuses)
	}

	// Message without tree should omit msg.Tree
	plainMsg := TurnMessage{
		SessionID: sess.ID,
		Role:      "assistant",
		Query:     "Standard query",
		Content:   "Standard answer",
		Mode:      "standard",
	}
	if err := store.AddMessage(plainMsg); err != nil {
		t.Fatalf("add standard message error: %v", err)
	}

	detail2, _ := store.GetSession(sess.ID)
	if len(detail2.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(detail2.Messages))
	}
	if len(detail2.Messages[1].Tree) != 0 {
		t.Errorf("expected empty Tree for standard turn, got: %s", string(detail2.Messages[1].Tree))
	}
}

func TestTreeMigration(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "vedetta-test-tree-migration-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "legacy.db")

	// 1. Create DB with legacy schema without tree_json
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open legacy db: %v", err)
	}
	legacySchema := `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT 'fast',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);
	CREATE TABLE messages (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		role TEXT NOT NULL,
		query TEXT NOT NULL,
		content TEXT NOT NULL,
		thinking TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT 'fast',
		focus TEXT NOT NULL DEFAULT '[]',
		sources_json TEXT NOT NULL DEFAULT '[]',
		facts_json TEXT NOT NULL DEFAULT '[]',
		subqueries_json TEXT NOT NULL DEFAULT '[]',
		created_at DATETIME NOT NULL
	);
	`
	if _, err := legacyDB.Exec(legacySchema); err != nil {
		t.Fatalf("failed to create legacy schema: %v", err)
	}
	// Insert dummy session and legacy message
	_, err = legacyDB.Exec(`INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES ('s1', 'Legacy Session', 'deep', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("insert legacy session error: %v", err)
	}
	_, err = legacyDB.Exec(`INSERT INTO messages (id, session_id, role, query, content, thinking, mode, focus, sources_json, facts_json, subqueries_json, created_at)
		VALUES ('m1', 's1', 'assistant', 'old query', 'old content', '', 'deep', '[]', '[]', '[]', '[]', CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("insert legacy message error: %v", err)
	}
	legacyDB.Close()

	// 2. Open legacy DB with NewStorage (triggers migration in initSchema)
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatalf("failed to open storage on legacy db: %v", err)
	}
	defer store.Close()

	// 3. Verify column tree_json was added
	var colCount int
	rows, err := store.db.Query("PRAGMA table_info(messages)")
	if err != nil {
		t.Fatalf("pragma table_info error: %v", err)
	}
	var hasTreeCol bool
	for rows.Next() {
		var cid int
		var name, colType string
		var notnull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notnull, &dfltValue, &pk); err == nil {
			colCount++
			if name == "tree_json" {
				hasTreeCol = true
			}
		}
	}
	rows.Close()

	if !hasTreeCol {
		t.Fatalf("migration failed: tree_json column missing from messages table")
	}

	// 4. Verify existing message can be read cleanly
	detail, err := store.GetSession("s1")
	if err != nil {
		t.Fatalf("failed to get migrated session: %v", err)
	}
	if len(detail.Messages) != 1 || detail.Messages[0].ID != "m1" {
		t.Fatalf("expected legacy message m1, got %+v", detail)
	}

	// 5. Verify new message with Tree can be saved
	treeJSON := `{"root":{"id":"root","query":"q"},"nodes":[]}`
	err = store.AddMessage(TurnMessage{
		SessionID: "s1",
		Role:      "assistant",
		Query:     "new query",
		Content:   "new content",
		Mode:      "deep",
		Tree:      json.RawMessage(treeJSON),
	})
	if err != nil {
		t.Fatalf("add message to migrated db error: %v", err)
	}

	// 6. Verify idempotence: calling initSchema again does not fail
	if err := store.initSchema(); err != nil {
		t.Fatalf("idempotent initSchema failed: %v", err)
	}
}

// F6.4: historical sessions/messages persisted with the decommissioned "fast"
// and "balanced" modes must be surfaced as "standard" on read, without any
// destructive migration.
func TestStoredModeNormalization(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "vedetta-test-mode-normalize-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewStorage(filepath.Join(tempDir, "modes.db"))
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	defer store.Close()

	// Legacy rows written with the old vocabulary.
	_, err = store.db.Exec(`INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES ('s-fast', 'Fast Legacy', 'fast', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP), ('s-balanced', 'Balanced Legacy', 'balanced', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP), ('s-deep', 'Deep Session', 'deep', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("insert legacy sessions error: %v", err)
	}
	_, err = store.db.Exec(`INSERT INTO messages (id, session_id, role, query, content, thinking, mode, focus, sources_json, facts_json, subqueries_json, tree_json, created_at) VALUES ('m1', 's-fast', 'assistant', 'q', 'c', '', 'fast', '[]', '[]', '[]', '[]', '{}', CURRENT_TIMESTAMP), ('m2', 's-balanced', 'assistant', 'q', 'c', '', 'balanced', '[]', '[]', '[]', '[]', '{}', CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("insert legacy messages error: %v", err)
	}

	// ListSessions normalizes the summary mode.
	list, err := store.ListSessions()
	if err != nil {
		t.Fatalf("list sessions error: %v", err)
	}
	modes := map[string]string{}
	for _, s := range list {
		modes[s.ID] = s.Mode
	}
	if modes["s-fast"] != "standard" || modes["s-balanced"] != "standard" {
		t.Errorf("list sessions modes = %+v, want legacy modes normalized to standard", modes)
	}
	if modes["s-deep"] != "deep" {
		t.Errorf("deep mode must be preserved, got %q", modes["s-deep"])
	}

	// GetSession normalizes both the session summary and the message modes.
	detail, err := store.GetSession("s-fast")
	if err != nil {
		t.Fatalf("get session error: %v", err)
	}
	if detail == nil || detail.Mode != "standard" {
		t.Fatalf("session mode = %+v, want standard", detail)
	}
	if len(detail.Messages) != 1 || detail.Messages[0].Mode != "standard" {
		t.Fatalf("message modes = %+v, want standard", detail.Messages)
	}
}

// V25-T1.1: databases created before meta_json existed gain the column on open,
// legacy rows read back without Meta, and new rows round-trip their Meta.
func TestMetaMigrationAndRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-meta.db")

	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	legacySchema := `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT 'standard',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);
	CREATE TABLE messages (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		role TEXT NOT NULL,
		query TEXT NOT NULL,
		content TEXT NOT NULL,
		thinking TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT 'standard',
		focus TEXT NOT NULL DEFAULT '[]',
		sources_json TEXT NOT NULL DEFAULT '[]',
		facts_json TEXT NOT NULL DEFAULT '[]',
		subqueries_json TEXT NOT NULL DEFAULT '[]',
		tree_json TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL
	);`
	if _, err := legacyDB.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := legacyDB.Exec(`INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES ('s1', 'Legacy', 'standard', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert legacy session: %v", err)
	}
	if _, err := legacyDB.Exec(`INSERT INTO messages (id, session_id, role, query, content, created_at) VALUES ('m1', 's1', 'assistant', 'q', 'old answer', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert legacy message: %v", err)
	}
	legacyDB.Close()

	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatalf("open storage on legacy db: %v", err)
	}
	defer store.Close()

	detail, err := store.GetSession("s1")
	if err != nil {
		t.Fatalf("get legacy session: %v", err)
	}
	if len(detail.Messages) != 1 || detail.Messages[0].Meta != nil {
		t.Fatalf("legacy message should read back without meta, got %+v", detail.Messages)
	}

	meta := `{"v":1,"provenance":"grounded","partial":false,"citations":[{"n":1,"fact_id":"F2","source_url":"https://example.com/a"}]}`
	if err := store.AddMessage(TurnMessage{
		SessionID: "s1",
		Role:      "assistant",
		Query:     "q2",
		Content:   "new answer",
		Meta:      json.RawMessage(meta),
	}); err != nil {
		t.Fatalf("add message with meta: %v", err)
	}

	detail, err = store.GetSession("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(detail.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(detail.Messages))
	}
	if got := string(detail.Messages[1].Meta); got != meta {
		t.Fatalf("meta round-trip mismatch:\n got  %s\n want %s", got, meta)
	}

	// Re-running the migrations is a no-op.
	if err := store.initSchema(); err != nil {
		t.Fatalf("idempotent initSchema failed: %v", err)
	}

	// V25-QD1.7: the question map column is migrated too; rows without a map read back without it.
	if detail.Messages[0].QuestionMap != nil || detail.Messages[1].QuestionMap != nil {
		t.Fatalf("messages without a map must read back without it")
	}
	qmap := `{"map":{"ambito":"NAS"},"cells":[{"id":"c1","status":"covered"}]}`
	if err := store.AddMessage(TurnMessage{SessionID: "s1", Role: "assistant", Query: "q3", Content: "mapped answer",
		QuestionMap: json.RawMessage(qmap)}); err != nil {
		t.Fatalf("add message with map: %v", err)
	}
	detail, _ = store.GetSession("s1")
	if got := string(detail.Messages[2].QuestionMap); got != qmap {
		t.Fatalf("question map round-trip mismatch:\n got  %s\n want %s", got, qmap)
	}
}

// countMessages counts the stored messages of a session, any content.
func countMessages(t *testing.T, store *Storage, sessionID string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id = ?", sessionID).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// fileHolds reports whether the database file, or its WAL, still holds text.
func fileHolds(t *testing.T, dbPath, text string) bool {
	t.Helper()
	for _, p := range []string{dbPath, dbPath + "-wal"} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte(text)) {
			return true
		}
	}
	return false
}

// Deleting a search deletes its question, answer, reasoning and sources
// (Piano dopo audit 1.2 / F1).
func TestDeleteSession_RemovesMessages(t *testing.T) {
	store, err := NewStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	defer store.Close()
	sess, err := store.CreateSession("una ricerca", "deep")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.AddMessage(TurnMessage{SessionID: sess.ID, Role: "assistant", Query: "domanda", Content: "risposta"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.DeleteSession(sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if n := countMessages(t, store, sess.ID); n != 0 {
		t.Errorf("after DeleteSession %d messages are left, want 0", n)
	}
	for _, pragma := range []string{"foreign_keys", "secure_delete"} {
		var on int
		if err := store.db.QueryRow("PRAGMA " + pragma).Scan(&on); err != nil || on != 1 {
			t.Errorf("PRAGMA %s = %d (err %v), want 1", pragma, on, err)
		}
	}
}

// A deleted search leaves no readable text in the file: SQLite keeps deleted
// rows in free space unless it overwrites them (secure_delete).
func TestDeleteSession_OverwritesText(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	sess, err := store.CreateSession("titolo ZEBRA-TITLE-31", "deep")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.AddMessage(TurnMessage{SessionID: sess.ID, Role: "assistant",
		Query: "domanda XYZZY-QUERY-42", Content: "risposta PLUGH-ANSWER-77", Thinking: "ragionamento FROB-THINK-13"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.DeleteSession(sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	texts := []string{"ZEBRA-TITLE-31", "XYZZY-QUERY-42", "PLUGH-ANSWER-77", "FROB-THINK-13"}
	// The service stays open for days: the WAL beside the file must not keep the
	// old pages either.
	for _, text := range texts {
		if fileHolds(t, dbPath, text) {
			t.Errorf("with the database open, its files still hold %q after the search was deleted", text)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, text := range texts {
		if fileHolds(t, dbPath, text) {
			t.Errorf("the database file still holds %q after the search was deleted", text)
		}
	}
}

// Messages left behind by deletions made before the fix go at the next start,
// and their text with them.
func TestNewStorage_RemovesOrphanMessages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	keep, err := store.CreateSession("resta", "deep")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.AddMessage(TurnMessage{SessionID: keep.ID, Role: "assistant", Query: "q", Content: "KEEP-ANSWER-55"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	store.Close()

	// An orphan as the old DeleteSession left them: the session row gone, the
	// message still there (a connection without foreign keys, as before).
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO messages (id, session_id, role, query, content, created_at)
		VALUES ('orphan-1', 'gone-session', 'assistant', 'domanda ORPHAN-QUERY-21', 'risposta ORPHAN-ANSWER-22', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert orphan: %v", err)
	}
	raw.Close()

	store, err = NewStorage(dbPath)
	if err != nil {
		t.Fatalf("NewStorage again: %v", err)
	}
	if n := countMessages(t, store, "gone-session"); n != 0 {
		t.Errorf("orphan messages after start: %d, want 0", n)
	}
	if n := countMessages(t, store, keep.ID); n != 1 {
		t.Errorf("messages of a live session after start: %d, want 1", n)
	}
	store.Close()
	for _, text := range []string{"ORPHAN-QUERY-21", "ORPHAN-ANSWER-22"} {
		if fileHolds(t, dbPath, text) {
			t.Errorf("the database file still holds %q after the start cleaned the orphans", text)
		}
	}
	if !fileHolds(t, dbPath, "KEEP-ANSWER-55") {
		t.Errorf("the answer of a live session is gone from the file")
	}
}

// Map R4: the Library lists every session and finds them by the words of their
// title or of any of their questions; the sidebar asks only for the last days.
func TestStorage_FindSessions(t *testing.T) {
	store, err := NewStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	mk := func(title string, questions ...string) string {
		sess, err := store.CreateSession(title, "deep")
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range questions {
			if err := store.AddMessage(TurnMessage{SessionID: sess.ID, Role: "assistant", Query: q, Content: "a", Mode: "deep"}); err != nil {
				t.Fatal(err)
			}
		}
		return sess.ID
	}
	old := mk("Mac mini M1 usato", "conviene un Mac mini M1 a 200 euro?", "e un minipc N100?")
	city := mk("Qualità dell'aria in Città", "com'è l'aria a Milano?")
	for i := 0; i < 120; i++ {
		mk("altra ricerca")
	}
	// the first session was last updated ten days ago
	if _, err := store.db.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?", time.Now().AddDate(0, 0, -10), old); err != nil {
		t.Fatal(err)
	}

	ids := func(list []SessionSummary) map[string]bool {
		m := map[string]bool{}
		for _, s := range list {
			m[s.ID] = true
		}
		return m
	}
	if all, _ := store.FindSessions(SessionQuery{}); len(all) != 122 {
		t.Errorf("all = %d, want 122", len(all))
	}
	if latest, _ := store.ListSessions(); len(latest) != 100 {
		t.Errorf("ListSessions = %d, want the latest 100", len(latest))
	}
	recent, _ := store.FindSessions(SessionQuery{Since: time.Now().AddDate(0, 0, -7)})
	if len(recent) != 121 || ids(recent)[old] {
		t.Errorf("last 7 days = %d sessions (old one in: %t), want 121 without it", len(recent), ids(recent)[old])
	}
	// a word of a follow-up question finds the session
	if got, _ := store.FindSessions(SessionQuery{Match: "N100"}); len(got) != 1 || got[0].ID != old {
		t.Errorf("follow-up word: %+v", got)
	}
	// case and accents do not matter; every word must be there
	if got, _ := store.FindSessions(SessionQuery{Match: "qualita CITTA"}); len(got) != 1 || got[0].ID != city {
		t.Errorf("accents and case: %+v", got)
	}
	if got, _ := store.FindSessions(SessionQuery{Match: "qualita Roma"}); len(got) != 0 {
		t.Errorf("a missing word must exclude the session: %+v", got)
	}
}

// Map R7: "Cancella tutte le ricerche" leaves no session, no message and no text of
// a question in the database file.
func TestStorage_DeleteAllSessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "all.db")
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"domanda segreta numero uno", "domanda segreta numero due"} {
		sess, err := store.CreateSession(q, "deep")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AddMessage(TurnMessage{SessionID: sess.ID, Role: "assistant", Query: q, Content: "risposta riservata", Mode: "deep"}); err != nil {
			t.Fatal(err)
		}
	}
	if n, b := store.Usage(); n != 2 || b == 0 {
		t.Fatalf("usage before = %d sessions, %d bytes", n, b)
	}
	n, err := store.DeleteAllSessions()
	if err != nil || n != 2 {
		t.Fatalf("DeleteAllSessions = %d, %v", n, err)
	}
	var msgs int
	store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&msgs)
	if left, _ := store.Usage(); left != 0 || msgs != 0 {
		t.Fatalf("left: %d sessions, %d messages", left, msgs)
	}
	// the space comes back: the file is as small as an empty database
	if fi, err := os.Stat(dbPath); err != nil || fi.Size() > 64*1024 {
		t.Errorf("database after deleting every session: %v bytes (err %v)", fi.Size(), err)
	}
	store.Close()
	for _, suffix := range []string{"", "-wal"} {
		raw, _ := os.ReadFile(dbPath + suffix)
		if bytes.Contains(raw, []byte("domanda segreta")) || bytes.Contains(raw, []byte("risposta riservata")) {
			t.Errorf("%s still holds the deleted text", filepath.Base(dbPath+suffix))
		}
	}
}
