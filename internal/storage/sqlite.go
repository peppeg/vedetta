package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type SessionSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Mode      string    `json:"mode"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TurnMessage struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"session_id"`
	Role       string          `json:"role"` // "user", "assistant"
	Query      string          `json:"query"`
	Content    string          `json:"content"`
	Thinking   string          `json:"thinking,omitempty"`
	Mode       string          `json:"mode,omitempty"`
	Focus      []string        `json:"focus,omitempty"`
	Sources    json.RawMessage `json:"sources,omitempty"`
	Facts      json.RawMessage `json:"facts,omitempty"`
	Subqueries json.RawMessage `json:"subqueries,omitempty"`
	Tree       json.RawMessage `json:"tree,omitempty"`
	// Meta holds structured answer metadata computed by the agent (provenance,
	// citation map, counters). It is opaque to storage.
	Meta json.RawMessage `json:"meta,omitempty"`
	// QuestionMap holds the question map and the state of its cells (V25-QD1.7); empty without a map.
	QuestionMap json.RawMessage `json:"question_map,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

type SessionDetail struct {
	SessionSummary
	Messages []TurnMessage `json:"messages"`
}

type Storage struct {
	db   *sql.DB
	mu   sync.RWMutex
	path string
}

func NewStorage(dbPath string) (*Storage, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create data dir %s: %w", dir, err)
	}
	// The searches are for this user only; a database that is already there keeps its mode.
	if f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		_ = f.Close()
	}

	// foreign_keys: the messages of a session go with it (ON DELETE CASCADE).
	// secure_delete: what is deleted is overwritten, not left readable in free
	// space (Piano dopo audit 1.2 / F1).
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)")
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	s := &Storage{db: db, path: dbPath}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to init schema: %w", err)
	}
	if err := s.removeOrphanMessages(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to remove orphan messages: %w", err)
	}

	return s, nil
}

// removeOrphanMessages deletes the messages whose session no longer exists: the
// old DeleteSession relied on a cascade that was off and left them behind. When
// it finds some, VACUUM rewrites the file, so the free space left by those
// deletions, made without secure_delete, no longer holds their text.
func (s *Storage) removeOrphanMessages() error {
	res, err := s.db.Exec("DELETE FROM messages WHERE session_id NOT IN (SELECT id FROM sessions)")
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	log.Printf("[storage] removed %d messages of searches deleted earlier; compacting the database", n)
	if _, err := s.db.Exec("VACUUM"); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

func (s *Storage) Close() error {
	return s.db.Close()
}

func (s *Storage) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT 'standard',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS messages (
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
		meta_json TEXT NOT NULL DEFAULT '{}',
		question_map_json TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, created_at ASC);
	CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at DESC);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Migrations: add columns introduced after the first schema to existing databases.
	if err := s.ensureColumn("messages", "tree_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn("messages", "meta_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn("messages", "question_map_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}

	return nil
}

// ensureColumn adds column to table when it is missing. The PRAGMA result set is
// fully consumed and closed before the ALTER TABLE runs.
func (s *Storage) ensureColumn(table, column, definition string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notnull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notnull, &dfltValue, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan %s columns: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate %s columns: %w", table, err)
	}
	rows.Close()

	if found {
		return nil
	}
	if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Storage) CreateSession(title, mode string) (*SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	id := uuid.NewString()

	title = strings.TrimSpace(title)
	if title == "" {
		title = "Nuova Ricerca"
	}
	// Truncate title if too long
	if r := []rune(title); len(r) > 120 {
		title = string(r[:120]) + "..."
	}

	if mode == "" {
		mode = "standard"
	}

	_, err := s.db.Exec(
		"INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		id, title, mode, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create session error: %w", err)
	}

	return &SessionSummary{
		ID:        id,
		Title:     title,
		Mode:      mode,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// normalizeStoredMode maps decommissioned mode names from historical rows to the
// current vocabulary. The old "fast" and "balanced" profiles are both the
// "standard" profile now (F6.4). Unknown modes are returned unchanged.
func normalizeStoredMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "fast", "balanced":
		return "standard"
	default:
		return m
	}
}

func (s *Storage) ListSessions() ([]SessionSummary, error) {
	return s.FindSessions(SessionQuery{Limit: 100})
}

// SessionQuery chooses the sessions FindSessions returns (map R4: the sidebar shows
// the last days, the Library all of them, with a search field).
type SessionQuery struct {
	Limit int       // at most this many, the most recent first; 0 = all
	Since time.Time // only sessions updated at or after it; zero = no limit
	Match string    // only sessions whose title or one of whose questions has every word of it
}

// FindSessions lists sessions, the most recently updated first. Match ignores case
// and accents, and looks at the title and at every question of the session.
func (s *Storage) FindSessions(q SessionQuery) ([]SessionSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	words := strings.Fields(foldText(q.Match))
	query := "SELECT id, title, mode, created_at, updated_at, '' FROM sessions ORDER BY updated_at DESC"
	if len(words) > 0 {
		query = `SELECT s.id, s.title, s.mode, s.created_at, s.updated_at, COALESCE(GROUP_CONCAT(m.query, ' '), '')
			FROM sessions s LEFT JOIN messages m ON m.session_id = s.id
			GROUP BY s.id ORDER BY s.updated_at DESC`
	}
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := []SessionSummary{}
	for rows.Next() {
		var sm SessionSummary
		var questions string
		if err := rows.Scan(&sm.ID, &sm.Title, &sm.Mode, &sm.CreatedAt, &sm.UpdatedAt, &questions); err != nil {
			return nil, err
		}
		if !q.Since.IsZero() && sm.UpdatedAt.Before(q.Since) {
			break // ordered by updated_at: the rest is older
		}
		if len(words) > 0 {
			text := foldText(sm.Title + " " + questions)
			ok := true
			for _, w := range words {
				if !strings.Contains(text, w) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		sm.Mode = normalizeStoredMode(sm.Mode)
		list = append(list, sm)
		if q.Limit > 0 && len(list) >= q.Limit {
			break
		}
	}
	return list, rows.Err()
}

// foldText lowers a text and takes the accents off its letters, so that a search
// for "citta" finds "Città".
func foldText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if f, ok := accentFold[r]; ok {
			b.WriteRune(f)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

var accentFold = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'ç': 'c', 'ñ': 'n',
}

func (s *Storage) GetSession(id string) (*SessionDetail, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sm SessionSummary
	err := s.db.QueryRow(
		"SELECT id, title, mode, created_at, updated_at FROM sessions WHERE id = ?",
		id,
	).Scan(&sm.ID, &sm.Title, &sm.Mode, &sm.CreatedAt, &sm.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	sm.Mode = normalizeStoredMode(sm.Mode)

	rows, err := s.db.Query(
		"SELECT id, session_id, role, query, content, thinking, mode, focus, sources_json, facts_json, subqueries_json, tree_json, meta_json, question_map_json, created_at FROM messages WHERE session_id = ? AND content != '' ORDER BY created_at ASC",
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []TurnMessage
	for rows.Next() {
		var msg TurnMessage
		var focusJSON, sourcesRaw, factsRaw, subqueriesRaw, treeRaw, metaRaw, mapRaw string

		if err := rows.Scan(
			&msg.ID,
			&msg.SessionID,
			&msg.Role,
			&msg.Query,
			&msg.Content,
			&msg.Thinking,
			&msg.Mode,
			&focusJSON,
			&sourcesRaw,
			&factsRaw,
			&subqueriesRaw,
			&treeRaw,
			&metaRaw,
			&mapRaw,
			&msg.CreatedAt,
		); err != nil {
			return nil, err
		}
		msg.Mode = normalizeStoredMode(msg.Mode)

		_ = json.Unmarshal([]byte(focusJSON), &msg.Focus)
		msg.Sources = json.RawMessage(sourcesRaw)
		msg.Facts = json.RawMessage(factsRaw)
		msg.Subqueries = json.RawMessage(subqueriesRaw)
		if treeRaw != "" && treeRaw != "{}" && treeRaw != "[]" && treeRaw != "null" {
			msg.Tree = json.RawMessage(treeRaw)
		}
		if metaRaw != "" && metaRaw != "{}" && metaRaw != "null" {
			msg.Meta = json.RawMessage(metaRaw)
		}
		if mapRaw != "" && mapRaw != "{}" && mapRaw != "null" {
			msg.QuestionMap = json.RawMessage(mapRaw)
		}

		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if messages == nil {
		messages = []TurnMessage{}
	}

	return &SessionDetail{
		SessionSummary: sm,
		Messages:       messages,
	}, nil
}

// DeleteSession deletes a search and its messages. The messages go first and
// explicitly, in the same transaction: the cascade is a second guard.
func (s *Storage) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM messages WHERE session_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The service stays open for days, and the WAL beside the file still holds
	// the old pages: they go into the file, already overwritten, and the WAL is
	// emptied. A reader still open may keep part of it: best effort, logged.
	var busy, frames, done int
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &done); err != nil || busy != 0 {
		log.Printf("[storage] after deleting session %s the WAL could not be emptied (busy=%d, err=%v)", id, busy, err)
	}
	return nil
}

// DeleteAllSessions removes every session and its messages (map R7, "Cancella
// tutte le ricerche"): overwritten on disk (secure_delete) and out of the WAL, as
// for one session. It returns how many sessions went.
func (s *Storage) DeleteAllSessions() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM messages"); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM sessions"); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// the freed pages are already overwritten: VACUUM gives the space back, so the
	// file is as small as an empty database; in WAL mode the new file goes through
	// the WAL, so the checkpoint comes after it
	if _, err := s.db.Exec("VACUUM"); err != nil {
		log.Printf("[storage] VACUUM after deleting every session failed: %v", err)
	}
	var busy, frames, done int
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &done); err != nil || busy != 0 {
		log.Printf("[storage] after deleting every session the WAL could not be emptied (busy=%d, err=%v)", busy, err)
	}
	return n, nil
}

// Usage is how many sessions there are and the bytes of the database on disk
// (the file and its WAL and shared-memory files).
func (s *Storage) Usage() (sessions int, bytes int64) {
	s.mu.RLock()
	_ = s.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions)
	s.mu.RUnlock()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			bytes += fi.Size()
		}
	}
	return sessions, bytes
}

func (s *Storage) AddMessage(msg TurnMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(msg.Content) == "" && strings.TrimSpace(msg.Thinking) == "" {
		return nil // Do not save empty aborted turns
	}

	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	focusBytes, _ := json.Marshal(msg.Focus)
	sourcesStr := string(msg.Sources)
	if sourcesStr == "" {
		sourcesStr = "[]"
	}
	factsStr := string(msg.Facts)
	if factsStr == "" {
		factsStr = "[]"
	}
	subqueriesStr := string(msg.Subqueries)
	if subqueriesStr == "" {
		subqueriesStr = "[]"
	}
	treeStr := string(msg.Tree)
	if treeStr == "" {
		treeStr = "{}"
	}
	metaStr := string(msg.Meta)
	if metaStr == "" {
		metaStr = "{}"
	}
	mapStr := string(msg.QuestionMap)
	if mapStr == "" {
		mapStr = "{}"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO messages (id, session_id, role, query, content, thinking, mode, focus, sources_json, facts_json, subqueries_json, tree_json, meta_json, question_map_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, msg.ID, msg.SessionID, msg.Role, msg.Query, msg.Content, msg.Thinking, msg.Mode, string(focusBytes), sourcesStr, factsStr, subqueriesStr, treeStr, metaStr, mapStr, msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert message error: %w", err)
	}

	// Update session updated_at timestamp
	_, err = tx.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?", msg.CreatedAt, msg.SessionID)
	if err != nil {
		return fmt.Errorf("update session timestamp error: %w", err)
	}

	return tx.Commit()
}

func (s *Storage) UpdateSessionTitle(id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	title = strings.TrimSpace(title)
	if len(title) > 120 {
		title = title[:120] + "..."
	}
	_, err := s.db.Exec("UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?", title, time.Now().UTC(), id)
	return err
}
