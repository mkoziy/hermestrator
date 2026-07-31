package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// WorkspaceClassifier inspects a workspace directory using injected git
// commands and determines its recovery state.
type WorkspaceClassifier struct {
	Command func(context.Context, string, ...string) *exec.Cmd
}

var _ dashboard.WorkspaceClassifier = WorkspaceClassifier{}

// Classify inspects workspacePath and returns its RecoveryState. When
// processFailed is true, ExecutorFailed takes priority — the executor's
// abnormal exit is the most specific signal.
func (c WorkspaceClassifier) Classify(ctx context.Context, workspacePath string, processFailed bool) (dashboard.RecoveryState, error) {
	command := c.Command
	if command == nil {
		command = exec.CommandContext
	}

	// If the executor process failed, that is the strongest signal.
	if processFailed {
		return dashboard.RecoveryExecutorFailed, nil
	}

	// Verify the workspace path exists and looks like a git repo.
	gitDir := filepath.Join(workspacePath, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return "", fmt.Errorf("workspace %q does not contain a .git directory; cannot classify", workspacePath)
	}
	if err := c.checkPath(workspacePath); err != nil {
		return "", err
	}

	// Check states from most specific to least specific:
	// 1. Local commits ahead of remote (work done, not pushed).
	// 2. Remote branch exists (work pushed, nothing new locally).
	// 3. Uncommitted changes (dirty working tree).
	// 4. Clean (no changes at all).

	localCommits, err := c.hasLocalCommits(ctx, command, workspacePath)
	if err != nil {
		return "", fmt.Errorf("check local commits: %w", err)
	}
	if localCommits {
		return dashboard.RecoveryLocalCommits, nil
	}

	remoteExists, err := c.hasRemoteBranch(ctx, command, workspacePath)
	if err != nil {
		return "", fmt.Errorf("check remote branch: %w", err)
	}
	if remoteExists {
		return dashboard.RecoveryRemoteBranchExists, nil
	}

	uncommitted, err := c.hasUncommittedChanges(ctx, command, workspacePath)
	if err != nil {
		return "", fmt.Errorf("check uncommitted changes: %w", err)
	}
	if uncommitted {
		return dashboard.RecoveryUncommittedChanges, nil
	}

	return dashboard.RecoveryNoChanges, nil
}

// checkPath verifies the workspace path is accessible and not obviously
// corrupt.
func (c WorkspaceClassifier) checkPath(workspacePath string) error {
	info, err := os.Stat(workspacePath)
	if err != nil {
		return fmt.Errorf("stat workspace %q: %w", workspacePath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", workspacePath)
	}
	return nil
}

// hasRemoteBranch returns true when the current branch has a configured
// upstream and that remote branch exists.
func (c WorkspaceClassifier) hasRemoteBranch(ctx context.Context, command func(context.Context, string, ...string) *exec.Cmd, workspacePath string) (bool, error) {
	// git rev-parse --abbrev-ref HEAD@{u} succeeds if upstream is set.
	cmd := command(ctx, "git", "-C", workspacePath, "rev-parse", "--abbrev-ref", "HEAD@{u}")
	out, err := cmd.Output()
	if err != nil {
		// No upstream configured — no remote branch exists.
		return false, nil
	}
	ref := strings.TrimSpace(string(out))
	return ref != "", nil
}

// hasLocalCommits returns true when there are local commits that haven't
// been pushed to the upstream tracking branch.
func (c WorkspaceClassifier) hasLocalCommits(ctx context.Context, command func(context.Context, string, ...string) *exec.Cmd, workspacePath string) (bool, error) {
	// First verify upstream is configured. Without upstream, local commits
	// can't be classified as "ahead of remote" — they're just local state.
	cmd := command(ctx, "git", "-C", workspacePath, "rev-parse", "--abbrev-ref", "HEAD@{u}")
	_, err := cmd.Output()
	if err != nil {
		return false, nil
	}

	// git rev-list --count HEAD...@{u} returns the number of commits
	// on either side. We extract the two numbers from git rev-list
	// --left-right --count HEAD...@{u}.
	cmd = command(ctx, "git", "-C", workspacePath, "rev-list", "--left-right", "--count", "HEAD...@{u}")
	out, err := cmd.Output()
	if err != nil {
		return false, nil
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return false, fmt.Errorf("unexpected rev-list output: %q", string(out))
	}
	// parts[0] is commits ahead (local), parts[1] is commits behind (remote).
	return parts[0] != "0", nil
}

// hasUncommittedChanges returns true when git status --porcelain produces
// any output (modified, added, deleted, or untracked files).
func (c WorkspaceClassifier) hasUncommittedChanges(ctx context.Context, command func(context.Context, string, ...string) *exec.Cmd, workspacePath string) (bool, error) {
	cmd := command(ctx, "git", "-C", workspacePath, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// RecoverLocks inspects every active run in the store, classifying its
// workspace. Any lock whose owning run has no confirmed-alive process gets
// released (transitioned to a terminal state derived from the git
// classification). This prevents a crashed PM from permanently locking a
// repository.
//
// processAlive is a caller-supplied function that returns true when the
// process identified by runID is still running. In production this would
// check a process table; in tests it is injected.
func RecoverLocks(ctx context.Context, store *ImplementationRunStore, classifier dashboard.WorkspaceClassifier, workspaceRoot string, processAlive func(runID string) bool) error {
	active, err := store.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("recover locks: list active: %w", err)
	}

	for _, run := range active {
		if processAlive != nil && processAlive(run.RunID) {
			// Process is still alive — keep the lock.
			continue
		}

		// Process is not confirmed alive. Classify the workspace and
		// release the lock. Use the issue number to construct the workspace path.
		workspacePath := filepath.Join(workspaceRoot, strconv.Itoa(run.IssueNumber))
		state, classErr := classifier.Classify(ctx, workspacePath, false)
		if classErr != nil {
			// Workspace doesn't exist or is corrupt — release with
			// a failure reason that captures the classification error.
			_ = store.Release(ctx, run.RunID, runStateFailed,
				fmt.Sprintf("recovery: %v", classErr))
			continue
		}

		terminalState := runStateCompleted
		failureReason := ""
		if state == dashboard.RecoveryExecutorFailed {
			terminalState = runStateFailed
			failureReason = "executor failed (recovered)"
		}
		if err := store.Release(ctx, run.RunID, terminalState, failureReason); err != nil {
			return fmt.Errorf("recover locks: release %q: %w", run.RunID, err)
		}
	}

	return nil
}
