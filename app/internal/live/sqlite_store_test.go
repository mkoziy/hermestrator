package live

import (
	"context"
	"testing"
	"time"

	aix "github.com/firebase/genkit/go/ai/exp"
)

func TestSQLiteSessionStorePersistsLatestSnapshot(t *testing.T) {
	store, err := NewSQLiteSessionStore[string](t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	saved, err := store.SaveSnapshot(context.Background(), "", func(*aix.SessionSnapshot[string]) (*aix.SessionSnapshot[string], error) {
		return &aix.SessionSnapshot[string]{SessionID: "pm-42", CreatedAt: now, UpdatedAt: now, State: &aix.SessionState[string]{Custom: "discovery"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.GetLatestSnapshot(context.Background(), "pm-42")
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.SnapshotID != saved.SnapshotID || latest.State.Custom != "discovery" {
		t.Fatalf("latest snapshot = %#v", latest)
	}
}

func TestSQLiteSessionStoreRejectsSnapshotWithoutSessionID(t *testing.T) {
	store, err := NewSQLiteSessionStore[string](t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveSnapshot(context.Background(), "", func(*aix.SessionSnapshot[string]) (*aix.SessionSnapshot[string], error) {
		return &aix.SessionSnapshot[string]{}, nil
	})
	if err == nil {
		t.Fatal("SaveSnapshot accepted an empty session ID")
	}
}

func TestImplementationRunStoreAcquireFailsOnSecondAcquireForSameRepo(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	runID, err := store.Acquire(ctx, "repo-1", "ralphex")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if runID == "" {
		t.Fatal("expected a non-empty run ID")
	}

	_, err = store.Acquire(ctx, "repo-1", "codex")
	if err == nil {
		t.Fatal("second acquire for same repo should fail on unique constraint")
	}
}

func TestImplementationRunStoreAcquireSucceedsForDifferentRepos(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	runID1, err := store.Acquire(ctx, "repo-1", "ralphex")
	if err != nil {
		t.Fatalf("acquire repo-1: %v", err)
	}
	if runID1 == "" {
		t.Fatal("expected a non-empty run ID for repo-1")
	}

	runID2, err := store.Acquire(ctx, "repo-2", "codex")
	if err != nil {
		t.Fatalf("acquire repo-2: %v", err)
	}
	if runID2 == "" {
		t.Fatal("expected a non-empty run ID for repo-2")
	}

	if runID1 == runID2 {
		t.Fatal("run IDs for different repositories should differ")
	}
}

func TestImplementationRunStoreReleaseAndReacquire(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	runID1, err := store.Acquire(ctx, "repo-1", "ralphex")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if err := store.Release(ctx, runID1, runStateCompleted, ""); err != nil {
		t.Fatalf("release: %v", err)
	}

	runID2, err := store.Acquire(ctx, "repo-1", "codex")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if runID2 == "" {
		t.Fatal("expected a non-empty run ID after re-acquire")
	}
	if runID1 == runID2 {
		t.Fatal("re-acquire should produce a new run ID")
	}
}

func TestImplementationRunStoreRecentFailures(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	// Acquire and release a failed run.
	runID1, err := store.Acquire(ctx, "repo-1", "ralphex")
	if err != nil {
		t.Fatalf("acquire run 1: %v", err)
	}
	if err := store.Release(ctx, runID1, runStateFailed, "connection timeout"); err != nil {
		t.Fatalf("release run 1: %v", err)
	}

	// Acquire and release another failed run with a different executor.
	runID2, err := store.Acquire(ctx, "repo-1", "codex")
	if err != nil {
		t.Fatalf("acquire run 2: %v", err)
	}
	if err := store.Release(ctx, runID2, runStateFailed, "out of memory"); err != nil {
		t.Fatalf("release run 2: %v", err)
	}

	// Acquire and release a completed run (should NOT appear in results).
	runID3, err := store.Acquire(ctx, "repo-1", "pi")
	if err != nil {
		t.Fatalf("acquire run 3: %v", err)
	}
	if err := store.Release(ctx, runID3, runStateCompleted, ""); err != nil {
		t.Fatalf("release run 3: %v", err)
	}

	records, err := store.RecentFailures(ctx, "repo-1", 10)
	if err != nil {
		t.Fatalf("recent failures: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records (failed runs only), got %d", len(records))
	}

	// Most recent first (only failed runs; completed "pi" run excluded).
	if records[0].Kind != "codex" || records[0].Reason != "out of memory" {
		t.Fatalf("most recent should be codex, got kind=%s reason=%s", records[0].Kind, records[0].Reason)
	}
	if records[1].Kind != "ralphex" || records[1].Reason != "connection timeout" {
		t.Fatalf("second record mismatch: kind=%s reason=%s", records[1].Kind, records[1].Reason)
	}

	// Different repository returns no records.
	records, err = store.RecentFailures(ctx, "repo-2", 10)
	if err != nil {
		t.Fatalf("recent failures for empty repo: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 records for repo-2, got %d", len(records))
	}
}

func TestImplementationRunStoreAcquireRejectsEmptyInputs(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	_, err = store.Acquire(ctx, "", "ralphex")
	if err == nil {
		t.Fatal("Acquire with empty repository ID should fail")
	}

	_, err = store.Acquire(ctx, "repo-1", "")
	if err == nil {
		t.Fatal("Acquire with empty executor kind should fail")
	}
}

func TestImplementationRunStoreReleaseRejectsInvalidStates(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	runID, err := store.Acquire(ctx, "repo-1", "ralphex")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	err = store.Release(ctx, runID, "invalid", "")
	if err == nil {
		t.Fatal("Release with invalid state should fail")
	}

	err = store.Release(ctx, "nonexistent", runStateCompleted, "")
	if err == nil {
		t.Fatal("Release of nonexistent run should fail")
	}
}
