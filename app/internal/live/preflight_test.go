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

func writePlanFile(t *testing.T, workspace, content string) {
	t.Helper()
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	if err := os.WriteFile(planPath, []byte(content), 0o640); err != nil {
		t.Fatalf("write plan file: %v", err)
	}
}

func TestPreflightAllClear(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, validPlan)

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			// Simulate git remote returning a matching URL.
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			// Simulate all tools found.
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	if !result.Passed {
		t.Fatalf("expected all-clear, got failures: %+v", result.Failures)
	}
	if len(result.Failures) != 0 {
		t.Errorf("expected 0 failures, got %d", len(result.Failures))
	}
}

func TestPreflightMissingWorkspaceDirectory(t *testing.T) {
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), nonexistent, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for missing workspace directory, got Passed=true")
	}
	found := false
	for _, f := range result.Failures {
		if f.Check == "workspace-exists" {
			found = true
			if !strings.Contains(f.Detail, nonexistent) {
				t.Errorf("failure detail %q should mention the path", f.Detail)
			}
		}
	}
	if !found {
		t.Errorf("expected workspace-exists failure, got %+v", result.Failures)
	}
}

func TestPreflightWorkspacePathIsNotDirectory(t *testing.T) {
	// Create a file where the workspace directory should be.
	base := t.TempDir()
	filePath := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), filePath, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for workspace path that is a file, got Passed=true")
	}
	found := false
	for _, f := range result.Failures {
		if f.Check == "workspace-exists" && strings.Contains(f.Detail, "not a directory") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected workspace-exists 'not a directory' failure, got %+v", result.Failures)
	}
}

func TestPreflightMissingPlanFile(t *testing.T) {
	workspace := t.TempDir()
	// No plan file written.

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for missing plan file, got Passed=true")
	}
	found := false
	for _, f := range result.Failures {
		if f.Check == "plan-file-exists" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected plan-file-exists failure, got %+v", result.Failures)
	}
}

func TestPreflightMalformedPlan(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, invalidPlanNoTitle)

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for malformed plan, got Passed=true")
	}
	found := false
	for _, f := range result.Failures {
		if f.Check == "plan-structure" {
			found = true
			if !strings.Contains(f.Detail, "does not satisfy the required structure") {
				t.Errorf("plan-structure detail should mention structural invalidity: %q", f.Detail)
			}
		}
	}
	if !found {
		t.Errorf("expected plan-structure failure, got %+v", result.Failures)
	}
}

func TestPreflightMissingTools(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, validPlan)

	missingTools := map[string]bool{
		"ralphex": true,
		"codex":   true,
	}

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			if missingTools[tool] {
				return "", exec.ErrNotFound
			}
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for missing tools, got Passed=true")
	}

	toolFailures := 0
	for _, f := range result.Failures {
		if f.Check == "tool-available" {
			toolFailures++
		}
	}
	if toolFailures != len(missingTools) {
		t.Errorf("expected %d tool-available failures, got %d: %+v", len(missingTools), toolFailures, result.Failures)
	}
}

func TestPreflightRemoteMismatch(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, validPlan)

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			// Return a URL for a different repository.
			return exec.CommandContext(ctx, "printf", "https://github.com/other/wrong-repo.git")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for remote mismatch, got Passed=true")
	}
	found := false
	for _, f := range result.Failures {
		if f.Check == "git-remote" {
			found = true
			if !strings.Contains(f.Detail, "wrong-repo") {
				t.Errorf("git-remote detail should mention the actual remote: %q", f.Detail)
			}
		}
	}
	if !found {
		t.Errorf("expected git-remote failure, got %+v", result.Failures)
	}
}

func TestPreflightGitRemoteCommandFails(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, validPlan)

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			// Simulate git failure (no origin configured).
			return exec.CommandContext(ctx, "sh", "-c", "echo 'fatal: No such remote' >&2; exit 2")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for git remote error, got Passed=true")
	}
	found := false
	for _, f := range result.Failures {
		if f.Check == "git-remote" && strings.Contains(f.Detail, "cannot determine origin remote") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected git-remote failure for command error, got %+v", result.Failures)
	}
}

func TestPreflightEmptyExpectedRemoteSkipsGitCheck(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, validPlan)

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			// Should not be called.
			t.Error("git command was called but expectedRemote is empty")
			return exec.CommandContext(ctx, "false")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "")
	if !result.Passed {
		t.Fatalf("expected all-clear when expectedRemote is empty, got failures: %+v", result.Failures)
	}
}

func TestPreflightMultipleFailuresCollected(t *testing.T) {
	// No workspace, no plan — expect both workspace and plan failures
	// plus tool failures collected together.
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			return "", exec.ErrNotFound // every tool missing
		},
	}

	result := preflight.Verify(context.Background(), nonexistent, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected multiple failures, got Passed=true")
	}

	checks := map[string]bool{}
	for _, f := range result.Failures {
		checks[f.Check] = true
	}
	if !checks["workspace-exists"] {
		t.Error("missing workspace-exists failure")
	}
	if !checks["plan-file-exists"] {
		t.Error("missing plan-file-exists failure")
	}
	if !checks["tool-available"] {
		t.Error("missing tool-available failure")
	}
	if checks["plan-structure"] {
		t.Error("plan-structure failure should not appear when plan file does not exist")
	}

	if len(result.Failures) < 3 {
		t.Errorf("expected at least 3 failures, got %d: %+v", len(result.Failures), result.Failures)
	}
}

func TestPreflightRemoteMatches(t *testing.T) {
	tests := []struct {
		url      string
		fullName string
		want     bool
	}{
		{"https://github.com/mkoziy/hermestrator.git", "mkoziy/hermestrator", true},
		{"https://github.com/mkoziy/hermestrator", "mkoziy/hermestrator", true},
		{"git@github.com:mkoziy/hermestrator.git", "mkoziy/hermestrator", true},
		{"git@github.com:mkoziy/hermestrator", "mkoziy/hermestrator", true},
		{"https://github.com/other/repo.git", "mkoziy/hermestrator", false},
		{"git@github.com:other/repo.git", "mkoziy/hermestrator", false},
		{"https://gitlab.com/mkoziy/hermestrator.git", "mkoziy/hermestrator", true}, // path still matches
		{"file:///local/path", "mkoziy/hermestrator", false},
	}

	for _, tt := range tests {
		got := remoteMatches(tt.url, tt.fullName)
		if got != tt.want {
			t.Errorf("remoteMatches(%q, %q) = %v, want %v", tt.url, tt.fullName, got, tt.want)
		}
	}
}

func TestPreflightUsesDefaultLookPathWhenNil(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, validPlan)

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		// LookPath is nil — real exec.LookPath will be used.
	}

	result := preflight.Verify(context.Background(), workspace, "")
	// With real LookPath, tools may or may not be found. The test just
	// asserts we didn't panic and that workspace + plan checks still work.
	// The Passed field may be false if tools are not installed.
	if len(result.Failures) > 0 {
		// Verify no unexpected failure types.
		for _, f := range result.Failures {
			if f.Check != "tool-available" {
				t.Errorf("unexpected failure with nil LookPath: %+v", f)
			}
		}
	}
}

func TestPreflightUsesDefaultCommandWhenNil(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, validPlan)

	preflight := Preflight{
		// Command is nil — real exec.CommandContext will be used.
		// It will try to run real "git", which should work if git is
		// installed.
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	// The git remote check may fail if the workspace is not a real git
	// repo, but the test should not panic.
	if result.Passed {
		return // git happened to succeed — that's fine too
	}
	// Make sure the failure is a git-remote failure (expected since our
	// temp dir isn't a git repo), not a panic.
	foundGitFailure := false
	for _, f := range result.Failures {
		if f.Check == "git-remote" {
			foundGitFailure = true
		}
	}
	if !foundGitFailure {
		t.Errorf("expected git-remote failure when Command is nil (temp dir is not a git repo), got: %+v", result.Failures)
	}
}

func TestPreflightPlanFileWithInvalidStructureNoTitle(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, invalidPlanNoTitle)

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for plan without title, got Passed=true")
	}
	found := false
	for _, f := range result.Failures {
		if f.Check == "plan-structure" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected plan-structure failure, got %+v", result.Failures)
	}
}

func TestPreflightPlanFileWithInvalidStructureNoTask(t *testing.T) {
	workspace := t.TempDir()
	writePlanFile(t, workspace, invalidPlanNoTask)

	preflight := Preflight{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "https://github.com/mkoziy/hermestrator.git")
		},
		LookPath: func(tool string) (string, error) {
			return "/usr/local/bin/" + tool, nil
		},
	}

	result := preflight.Verify(context.Background(), workspace, "mkoziy/hermestrator")
	if result.Passed {
		t.Fatal("expected failure for plan without tasks, got Passed=true")
	}
	found := false
	for _, f := range result.Failures {
		if f.Check == "plan-structure" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected plan-structure failure, got %+v", result.Failures)
	}
}
