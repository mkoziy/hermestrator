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

// IssueWorkspace creates and manages an isolated clone for per-issue
// implementation work. It mirrors CloneIntake's shape — injectable Command,
// path validation, Cleanup that refuses to escape BaseDir — but roots
// clones under a distinct workspace directory keyed by issue number.
//
// Implementation and intake workspaces are intentionally kept separate on
// disk and in code.
type IssueWorkspace struct {
	BaseDir string
	Command func(context.Context, string, ...string) *exec.Cmd
}

var _ dashboard.IssueClone = IssueWorkspace{}

// Start clones a repository into <BaseDir>/<issueNumber> and returns the
// absolute workspace path. The directory must not already exist — restart
// recovery is handled at a higher layer (task 15).
func (w IssueWorkspace) Start(ctx context.Context, repo dashboard.Repository, issueNumber int) (string, error) {
	if w.BaseDir == "" {
		return "", fmt.Errorf("issue workspace base directory is required")
	}
	if strings.Count(repo.FullName, "/") != 1 || strings.Contains(repo.FullName, "..") {
		return "", fmt.Errorf("invalid GitHub repository name")
	}
	if issueNumber < 1 {
		return "", fmt.Errorf("issue number is required")
	}
	if err := os.MkdirAll(w.BaseDir, 0o750); err != nil {
		return "", fmt.Errorf("create issue workspace base directory: %w", err)
	}
	path := filepath.Join(w.BaseDir, strconv.Itoa(issueNumber))
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("issue workspace already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect issue workspace destination: %w", err)
	}
	command := w.Command
	if command == nil {
		command = exec.CommandContext
	}
	output, err := command(ctx, "gh", "repo", "clone", repo.FullName, path, "--", "--depth=1").CombinedOutput()
	if err != nil {
		// The destination was absent immediately before this invocation, so any
		// path now present is clone-owned partial state and may be removed.
		// This must not delete a workspace that predated a failed clone.
		if _, statErr := os.Lstat(path); statErr == nil {
			_ = w.Cleanup(ctx, path)
		}
		return "", fmt.Errorf("clone issue workspace: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return path, nil
}

// Cleanup removes path after confirming it is a validated child of BaseDir.
// If path does not exist the call is a no-op and returns nil.
func (w IssueWorkspace) Cleanup(_ context.Context, path string) error {
	if err := w.validateChild(path); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove issue workspace: %w", err)
	}
	return nil
}

// validateChild refuses operations on paths that escape BaseDir.
// If path does not exist, the check proceeds against the raw path after
// verifying it does not contain path-traversal sequences.
func (w IssueWorkspace) validateChild(path string) error {
	base, err := filepath.EvalSymlinks(w.BaseDir)
	if err != nil {
		return fmt.Errorf("resolve issue workspace base directory: %w", err)
	}
	candidate, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("resolve issue workspace path: %w", err)
		}
		// Path does not exist; use the raw path after sanitisation.
		candidate = filepath.Clean(path)
	}
	relative, err := filepath.Rel(base, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refuse operation outside isolated issue workspace directory")
	}
	return nil
}
