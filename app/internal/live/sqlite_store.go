package live

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	aix "github.com/firebase/genkit/go/ai/exp"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// SQLiteSessionStore persists Genkit snapshots in the dashboard database.
type SQLiteSessionStore[State any] struct{ db *sql.DB }

func (s *SQLiteSessionStore[State]) Close() error { return s.db.Close() }

func NewSQLiteSessionStore[State any](path string) (*SQLiteSessionStore[State], error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open Genkit session store: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS genkit_snapshots (snapshot_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, body BLOB NOT NULL); CREATE INDEX IF NOT EXISTS genkit_snapshots_session_created ON genkit_snapshots(session_id, created_at DESC);`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize Genkit session store: %w", err)
	}
	return &SQLiteSessionStore[State]{db: db}, nil
}

func (s *SQLiteSessionStore[State]) GetSnapshot(ctx context.Context, id string) (*aix.SessionSnapshot[State], error) {
	return s.snapshot(ctx, `SELECT body FROM genkit_snapshots WHERE snapshot_id=?`, id)
}

func (s *SQLiteSessionStore[State]) GetLatestSnapshot(ctx context.Context, sessionID string) (*aix.SessionSnapshot[State], error) {
	if sessionID == "" {
		return nil, fmt.Errorf("genkit session ID is required")
	}
	return s.snapshot(ctx, `SELECT body FROM genkit_snapshots WHERE session_id=? ORDER BY created_at DESC, snapshot_id DESC LIMIT 1`, sessionID)
}

func (s *SQLiteSessionStore[State]) snapshot(ctx context.Context, query, value string) (*aix.SessionSnapshot[State], error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, query, value).Scan(&body)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load Genkit snapshot: %w", err)
	}
	var snapshot aix.SessionSnapshot[State]
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return nil, fmt.Errorf("decode Genkit snapshot: %w", err)
	}
	return &snapshot, nil
}

func (s *SQLiteSessionStore[State]) SaveSnapshot(ctx context.Context, id string, fn func(*aix.SessionSnapshot[State]) (*aix.SessionSnapshot[State], error)) (*aix.SessionSnapshot[State], error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin Genkit snapshot write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing *aix.SessionSnapshot[State]
	if id != "" {
		var body []byte
		err := tx.QueryRowContext(ctx, `SELECT body FROM genkit_snapshots WHERE snapshot_id=?`, id).Scan(&body)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("load Genkit snapshot for write: %w", err)
		}
		if err == nil {
			existing = &aix.SessionSnapshot[State]{}
			if err := json.Unmarshal(body, existing); err != nil {
				return nil, fmt.Errorf("decode Genkit snapshot for write: %w", err)
			}
		}
	}
	next, err := fn(existing)
	if err != nil || next == nil {
		return next, err
	}
	if id == "" {
		id = uuid.NewString()
	}
	next.SnapshotID = id
	if existing != nil {
		next.SessionID = existing.SessionID
	}
	if next.Status == "" {
		next.Status = aix.SnapshotStatusCompleted
	}
	if next.SessionID == "" {
		return nil, fmt.Errorf("genkit snapshot session ID is required")
	}
	body, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode Genkit snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO genkit_snapshots(snapshot_id,session_id,created_at,updated_at,body) VALUES(?,?,?,?,?) ON CONFLICT(snapshot_id) DO UPDATE SET updated_at=excluded.updated_at, body=excluded.body`, id, next.SessionID, next.CreatedAt.UTC().Format(time.RFC3339Nano), next.UpdatedAt.UTC().Format(time.RFC3339Nano), body); err != nil {
		return nil, fmt.Errorf("save Genkit snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit Genkit snapshot: %w", err)
	}
	return next, nil
}
