package live

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestIssueWorkspaceSuccessfulCloneAndCleanup(t *testing.T) {
	base := t.TempDir()
	repo := dashboard.Repository{FullName: "mkoziy/hermestrator"}

	// Inject a fake gh that mimics a shallow clone by creating the
	// destination directory and a README stub.
	ws := IssueWorkspace{
		BaseDir: base,
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			// args: ["gh", "repo", "clone", "mkoziy/hermestrator", "<path>", "--", "--depth=1"]
			if len(args) < 5 || args[0] != "repo" || args[1] != "clone" {
				return exec.CommandContext(ctx, "false")
			}
			path := args[3]
			return exec.CommandContext(ctx, "sh", "-c", "mkdir -p "+path+" && echo '# hermestrator' > "+path+"/README.md")
		},
	}

	path, err := ws.Start(context.Background(), repo, 42)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasSuffix(path, filepath.Join(base, "42")) && !strings.Contains(path, "42") {
		t.Fatalf("expected workspace path under base/42, got %q", path)
	}

	// Verify the clone produced a real directory.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat workspace: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("workspace path is not a directory")
	}

	// Verify the README is present.
	readme, err := os.ReadFile(filepath.Join(path, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if string(readme) != "# hermestrator\n" {
		t.Fatalf("unexpected README content: %q", readme)
	}

	// Cleanup must remove the workspace directory.
	if err := ws.Cleanup(context.Background(), path); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("workspace still exists after cleanup")
	}
}

func TestIssueWorkspaceCleanupRefusesPathEscape(t *testing.T) {
	base := t.TempDir()
	ws := IssueWorkspace{BaseDir: base}

	// A path inside BaseDir should clean up fine.
	inside := filepath.Join(base, "42")
	if err := os.Mkdir(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ws.Cleanup(context.Background(), inside); err != nil {
		t.Fatalf("cleanup inside base: %v", err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatal("workspace still exists after cleanup")
	}

	// A path outside BaseDir must be rejected.
	outside := t.TempDir()
	if err := ws.Cleanup(context.Background(), outside); err == nil {
		t.Fatal("cleanup outside base unexpectedly succeeded")
	}
}

func TestIssueWorkspaceCloneFailureSurfacesStderr(t *testing.T) {
	base := t.TempDir()
	repo := dashboard.Repository{FullName: "mkoziy/hermestrator"}

	ws := IssueWorkspace{
		BaseDir: base,
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			// Simulate a clone failure: write an error to stderr and exit 1.
			return exec.CommandContext(ctx, "sh", "-c", "echo 'ERROR: repository not found' >&2; exit 1")
		},
	}

	_, err := ws.Start(context.Background(), repo, 73)
	if err == nil {
		t.Fatal("expected clone failure, got nil error")
	}

	// The error message must surface the gh stderr output.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "ERROR: repository not found") {
		t.Errorf("error message %q does not contain stderr output", errMsg)
	}

	// The failed clone directory must be cleaned up (not left orphaned).
	path := filepath.Join(base, "73")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("failed clone directory %q was not cleaned up", path)
	}
}

func TestIssueWorkspaceCloneFailurePreservesExistingWorkspace(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "73")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "keep.txt")
	if err := os.WriteFile(marker, []byte("existing work"), 0o640); err != nil {
		t.Fatal(err)
	}

	ws := IssueWorkspace{BaseDir: base, Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exit 1")
	}}
	_, err := ws.Start(context.Background(), dashboard.Repository{FullName: "mkoziy/hermestrator"}, 73)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Start error = %v, want existing workspace rejection", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "existing work" {
		t.Fatalf("existing workspace was changed: data=%q err=%v", data, err)
	}
}

func TestIssueWorkspaceRejectsInvalidRepositoryName(t *testing.T) {
	base := t.TempDir()
	ws := IssueWorkspace{BaseDir: base}

	// Path traversal attempt in repo name.
	_, err := ws.Start(context.Background(), dashboard.Repository{FullName: "foo/bar/../../../etc"}, 1)
	if err == nil {
		t.Fatal("expected rejection of path-traversal repo name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid GitHub repository name") {
		t.Errorf("unexpected error: %v", err)
	}

	// Missing slash.
	_, err = ws.Start(context.Background(), dashboard.Repository{FullName: "no-slash"}, 1)
	if err == nil {
		t.Fatal("expected rejection of repo name without slash, got nil")
	}
}

func TestIssueWorkspaceRejectsMissingBaseDir(t *testing.T) {
	ws := IssueWorkspace{}
	_, err := ws.Start(context.Background(), dashboard.Repository{FullName: "mkoziy/hermestrator"}, 1)
	if err == nil {
		t.Fatal("expected error for missing BaseDir")
	}
	if !strings.Contains(err.Error(), "base directory is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestIssueWorkspaceRejectsInvalidIssueNumber(t *testing.T) {
	base := t.TempDir()
	ws := IssueWorkspace{BaseDir: base}

	_, err := ws.Start(context.Background(), dashboard.Repository{FullName: "mkoziy/hermestrator"}, 0)
	if err == nil {
		t.Fatal("expected error for zero issue number")
	}
	if !strings.Contains(err.Error(), "issue number is required") {
		t.Errorf("unexpected error: %v", err)
	}

	_, err = ws.Start(context.Background(), dashboard.Repository{FullName: "mkoziy/hermestrator"}, -1)
	if err == nil {
		t.Fatal("expected error for negative issue number")
	}
	if !strings.Contains(err.Error(), "issue number is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestIssueWorkspaceUsesDefaultCommandWhenNil(t *testing.T) {
	base := t.TempDir()
	ws := IssueWorkspace{BaseDir: base}

	// With Command nil, the adapter uses exec.CommandContext. That will
	// try to run the real "gh" binary, which may not exist in CI. We
	// just verify the call reaches execution and fails at the OS level
	// rather than panicking on a nil function.
	_, err := ws.Start(context.Background(), dashboard.Repository{FullName: "mkoziy/hermestrator"}, 1)
	if err == nil {
		// gh was available and completed; that's fine too.
		return
	}
	// The error should come from exec, not a nil-dereference.
	if matched, _ := regexp.MatchString(`(exec|clone issue workspace):`, err.Error()); !matched {
		t.Errorf("unexpected error when Command is nil: %v", err)
	}
}
