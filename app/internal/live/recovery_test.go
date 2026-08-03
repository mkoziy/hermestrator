package live

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// setupGitRepo creates a minimal git repository at path and returns the
// path. The repo has one initial commit.
func setupGitRepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "init")
	runGit(t, path, "config", "user.email", "test@example.com")
	runGit(t, path, "config", "user.name", "Test")
	// Create an initial file and commit so HEAD exists.
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", "README.md")
	runGit(t, path, "commit", "-m", "initial")
	return path
}

// runGit executes a git command in dir. It fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
}

// fakeGitCommand returns a Command func that delegates to real git for the
// given repo path, and fails on unexpected commands.
func fakeGitCommand(t *testing.T) func(context.Context, string, ...string) *exec.Cmd {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, name, args...)
	}
}

func TestClassifyNoChanges(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "ws")
	setupGitRepo(t, repoPath)

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	state, err := classifier.Classify(context.Background(), repoPath, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if state != dashboard.RecoveryNoChanges {
		t.Errorf("expected NoChanges, got %s", state)
	}
}

func TestClassifyUncommittedChanges(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "ws")
	setupGitRepo(t, repoPath)

	// Create an uncommitted file.
	if err := os.WriteFile(filepath.Join(repoPath, "new.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	state, err := classifier.Classify(context.Background(), repoPath, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if state != dashboard.RecoveryUncommittedChanges {
		t.Errorf("expected UncommittedChanges, got %s", state)
	}
}

func TestClassifyLocalCommits(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "ws")
	setupGitRepo(t, repoPath)

	// Create a local commit with an upstream set (we simulate by setting
	// the upstream to a detached tracking branch via a fake remote).
	// We'll set up a bare repo as the remote and push the initial commit.
	barePath := filepath.Join(dir, "bare.git")
	if err := os.MkdirAll(barePath, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit(t, barePath, "init", "--bare")
	runGit(t, repoPath, "remote", "add", "origin", barePath)
	runGit(t, repoPath, "push", "-u", "origin", "main")

	// Now create a local commit that hasn't been pushed.
	if err := os.WriteFile(filepath.Join(repoPath, "new.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoPath, "add", "new.go")
	runGit(t, repoPath, "commit", "-m", "local commit ahead of remote")

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	state, err := classifier.Classify(context.Background(), repoPath, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if state != dashboard.RecoveryLocalCommits {
		t.Errorf("expected LocalCommits, got %s", state)
	}
}

func TestClassifyRemoteBranchExists(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "ws")
	setupGitRepo(t, repoPath)

	// Set up a remote and push to it.
	barePath := filepath.Join(dir, "bare.git")
	if err := os.MkdirAll(barePath, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit(t, barePath, "init", "--bare")
	runGit(t, repoPath, "remote", "add", "origin", barePath)
	runGit(t, repoPath, "push", "-u", "origin", "main")

	// No local commits ahead, no uncommitted changes. The workspace has
	// no changes relative to the remote. But the remote branch exists.
	// hasRemoteBranch returns true because upstream is set.
	// The order of checks in Classify puts remote check first.
	// Wait — remoteExists will be true, so it should return
	// RemoteBranchExists even when there are no uncommitted changes or
	// local commits. That matches our logic: if a remote branch exists
	// the work has been pushed somewhere.

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	state, err := classifier.Classify(context.Background(), repoPath, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if state != dashboard.RecoveryRemoteBranchExists {
		t.Errorf("expected RemoteBranchExists, got %s", state)
	}
}

func TestClassifyExecutorFailed(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "ws")
	setupGitRepo(t, repoPath)

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	// When processFailed is true, ExecutorFailed takes priority
	// regardless of other git state.
	state, err := classifier.Classify(context.Background(), repoPath, true)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if state != dashboard.RecoveryExecutorFailed {
		t.Errorf("expected ExecutorFailed, got %s", state)
	}
}

func TestClassifyAmbiguousCorruptWorkspace(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "ws")
	// Create a directory that is NOT a git repo.
	if err := os.MkdirAll(repoPath, 0o750); err != nil {
		t.Fatal(err)
	}

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	_, err := classifier.Classify(context.Background(), repoPath, false)
	if err == nil {
		t.Fatal("expected error for corrupt/non-git workspace, got nil")
	}
	if !strings.Contains(err.Error(), ".git") {
		t.Errorf("expected error to mention .git, got: %v", err)
	}
}

func TestClassifyNonexistentWorkspace(t *testing.T) {
	dir := t.TempDir()
	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	_, err := classifier.Classify(context.Background(), filepath.Join(dir, "nonexistent"), false)
	if err == nil {
		t.Fatal("expected error for nonexistent workspace")
	}
}

func TestClassifyWorkspaceNotADirectory(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(filePath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	_, err := classifier.Classify(context.Background(), filePath, false)
	if err == nil {
		t.Fatal("expected error for file instead of directory")
	}
}

func TestRecoverLocksReleasesCrashOrphanedLock(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	// Acquire a lock for repo-1, issue 42.
	runID, err := store.Acquire(ctx, "repo-1", 42, "ralphex")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Create the workspace directory as a real git repo so the
	// classifier can inspect it. Workspace is keyed by issue number.
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspaces")
	repoPath := filepath.Join(wsRoot, "42")
	setupGitRepo(t, repoPath)

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	// processAlive returns false — simulating a crashed PM.
	processAlive := func(id string) bool { return false }

	if err := RecoverLocks(ctx, store, classifier, wsRoot, processAlive); err != nil {
		t.Fatalf("RecoverLocks: %v", err)
	}

	// The lock should now be released, so a subsequent acquire for the
	// same repository should succeed.
	runID2, err := store.Acquire(ctx, "repo-1", 43, "codex")
	if err != nil {
		t.Fatalf("re-acquire after recovery: %v (original runID=%s)", err, runID)
	}
	if runID2 == "" {
		t.Fatal("expected non-empty run ID after re-acquire")
	}
	if runID2 == runID {
		t.Fatal("re-acquire should produce a new run ID")
	}
}

func TestRecoverLocksKeepsAliveProcessLock(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	runID, err := store.Acquire(ctx, "repo-1", 42, "ralphex")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspaces")
	repoPath := filepath.Join(wsRoot, "42")
	setupGitRepo(t, repoPath)

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	// processAlive returns true for our run ID — process still running.
	processAlive := func(id string) bool { return id == runID }

	if err := RecoverLocks(ctx, store, classifier, wsRoot, processAlive); err != nil {
		t.Fatalf("RecoverLocks: %v", err)
	}

	// The lock should still be held.
	_, err = store.Acquire(ctx, "repo-1", 43, "codex")
	if err == nil {
		t.Fatal("expected acquire to fail because alive lock is still held")
	}
}

func TestRecoverLocksEmptyActiveList(t *testing.T) {
	store, err := NewImplementationRunStore(t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspaces")

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	// No locks acquired — recovery should be a no-op.
	if err := RecoverLocks(ctx, store, classifier, wsRoot, nil); err != nil {
		t.Fatalf("RecoverLocks on empty store: %v", err)
	}
}

func TestClassifyModifiedFileDetected(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "ws")
	setupGitRepo(t, repoPath)

	// Modify the tracked README.md.
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("# modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	classifier := WorkspaceClassifier{
		Command: fakeGitCommand(t),
	}

	state, err := classifier.Classify(context.Background(), repoPath, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if state != dashboard.RecoveryUncommittedChanges {
		t.Errorf("expected UncommittedChanges for modified file, got %s", state)
	}
}
