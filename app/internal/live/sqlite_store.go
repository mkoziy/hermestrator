package live

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	aix "github.com/firebase/genkit/go/ai/exp"
	"github.com/google/uuid"
	"github.com/mkoziy/hermestrator/internal/dashboard"
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

// ImplementationRunStore enforces at most one active implementation run per
// repository via a partial unique index, and records terminal run history for
// executor selection (priorFailures).
type ImplementationRunStore struct{ db *sql.DB }

// Close releases the underlying database connection.
func (s *ImplementationRunStore) Close() error { return s.db.Close() }

// NewImplementationRunStore opens the SQLite database at path, creates the
// implementation_runs table and partial unique index if they do not exist,
// and returns a store ready for use.
func NewImplementationRunStore(path string) (*ImplementationRunStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open implementation run store: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS implementation_runs (
			run_id          TEXT PRIMARY KEY,
			repository_id   TEXT NOT NULL,
			executor_kind   TEXT NOT NULL,
			state           TEXT NOT NULL DEFAULT 'active',
			failure_reason  TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS one_active_implementation_per_repository
			ON implementation_runs(repository_id) WHERE state = 'active';
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize implementation run store: %w", err)
	}
	return &ImplementationRunStore{db: db}, nil
}

// runState constants mirror the intent of the one_active_implementation_per_repository index.
const (
	runStateActive    = "active"
	runStateCompleted = "completed"
	runStateFailed    = "failed"
)

// Acquire inserts an active run row for repositoryID. It returns a unique
// run ID and an error. If another active run already exists for the same
// repository, Acquire returns an error because the partial unique index
// prevents a second active row.
func (s *ImplementationRunStore) Acquire(ctx context.Context, repositoryID string, kind dashboard.ExecutorKind) (string, error) {
	if strings.TrimSpace(repositoryID) == "" {
		return "", fmt.Errorf("repository ID is required")
	}
	if kind == "" {
		return "", fmt.Errorf("executor kind is required")
	}
	runID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO implementation_runs(run_id, repository_id, executor_kind, state, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
		runID, repositoryID, string(kind), runStateActive, now, now)
	if err != nil {
		if isUniqueConstraintError(err) {
			return "", fmt.Errorf("repository %q already has an active implementation run", repositoryID)
		}
		return "", fmt.Errorf("acquire implementation lock: %w", err)
	}
	return runID, nil
}

// Release transitions a run to a terminal state. If terminalState is
// runStateFailed the failureReason is recorded; otherwise it is ignored.
func (s *ImplementationRunStore) Release(ctx context.Context, runID string, terminalState string, failureReason string) error {
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("run ID is required")
	}
	if terminalState != runStateCompleted && terminalState != runStateFailed {
		return fmt.Errorf("terminal state must be %q or %q, got %q", runStateCompleted, runStateFailed, terminalState)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx,
		`UPDATE implementation_runs SET state=?, failure_reason=?, updated_at=? WHERE run_id=? AND state=?`,
		terminalState, failureReason, now, runID, runStateActive)
	if err != nil {
		return fmt.Errorf("release implementation lock: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check release result: %w", err)
	}
	if changed == 0 {
		return fmt.Errorf("run %q is not in active state; cannot release", runID)
	}
	return nil
}

// RecentFailures returns up to limit most recent terminal runs
// (completed or failed) for repositoryID, ordered by most recent first.
func (s *ImplementationRunStore) RecentFailures(ctx context.Context, repositoryID string, limit int) ([]dashboard.FailureRecord, error) {
	if strings.TrimSpace(repositoryID) == "" {
		return nil, fmt.Errorf("repository ID is required")
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT executor_kind, failure_reason FROM implementation_runs WHERE repository_id=? AND state IN (?,?) ORDER BY created_at DESC LIMIT ?`,
		repositoryID, runStateCompleted, runStateFailed, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent failures: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []dashboard.FailureRecord
	for rows.Next() {
		var kind, reason string
		if err := rows.Scan(&kind, &reason); err != nil {
			return nil, fmt.Errorf("scan recent failure: %w", err)
		}
		records = append(records, dashboard.FailureRecord{
			Kind:   dashboard.ExecutorKind(kind),
			Reason: reason,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent failures: %w", err)
	}
	return records, nil
}

// isUniqueConstraintError returns true when err is an SQLite
// UNIQUE constraint violation.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
