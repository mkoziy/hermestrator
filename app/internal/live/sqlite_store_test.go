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
