package live

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestVerificationRunnerAllPassing(t *testing.T) {
	runner := &VerificationRunner{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				// All checks pass: exit 0 with typical output.
				return exec.CommandContext(ctx, "printf", "ok\n")
			},
		},
	}

	checks := []CheckSpec{
		{Name: "go vet", Command: "go", Args: []string{"vet", "./..."}},
		{Name: "go test", Command: "go", Args: []string{"test", "./..."}},
	}

	workspace := t.TempDir()
	result, err := runner.Run(context.Background(), workspace, checks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !result.ReadyForPR {
		t.Error("ReadyForPR = false, want true (all checks passed)")
	}
	if len(result.Checks) != 2 {
		t.Fatalf("expected 2 check results, got %d", len(result.Checks))
	}
	for i, cr := range result.Checks {
		if cr.ExitCode != 0 {
			t.Errorf("check %d (%s): exit code = %d, want 0", i, cr.Name, cr.ExitCode)
		}
		if !cr.Passed {
			t.Errorf("check %d (%s): Passed = false, want true", i, cr.Name)
		}
		if cr.Output == "" {
			t.Errorf("check %d (%s): Output is empty", i, cr.Name)
		}
	}
}

func TestVerificationRunnerFailingCheckBlocksReadyForPR(t *testing.T) {
	callIdx := 0
	runner := &VerificationRunner{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				callIdx++
				// The second invocation is "go test" — make it fail.
				// Use callIdx to distinguish since "go test -race" also has "test" as args[0].
				if callIdx == 2 {
					return exec.CommandContext(ctx, "sh", "-c", "echo 'FAIL: TestFoo' >&2; exit 1")
				}
				return exec.CommandContext(ctx, "printf", "ok\n")
			},
		},
	}

	checks := []CheckSpec{
		{Name: "go vet", Command: "go", Args: []string{"vet", "./..."}},
		{Name: "go test", Command: "go", Args: []string{"test", "./..."}},
		{Name: "go test -race", Command: "go", Args: []string{"test", "-race", "./..."}},
	}

	workspace := t.TempDir()
	result, err := runner.Run(context.Background(), workspace, checks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.ReadyForPR {
		t.Error("ReadyForPR = true, want false (go test failed)")
	}
	if len(result.Checks) != 3 {
		t.Fatalf("expected 3 check results, got %d", len(result.Checks))
	}

	// First check (go vet) should have passed.
	if !result.Checks[0].Passed {
		t.Errorf("check 0 (%s): Passed = false, want true", result.Checks[0].Name)
	}

	// Second check (go test) should have failed.
	if result.Checks[1].Passed {
		t.Errorf("check 1 (%s): Passed = true, want false", result.Checks[1].Name)
	}
	if result.Checks[1].ExitCode != 1 {
		t.Errorf("check 1 (%s): exit code = %d, want 1", result.Checks[1].Name, result.Checks[1].ExitCode)
	}
	if !strings.Contains(result.Checks[1].Output, "FAIL: TestFoo") {
		t.Errorf("check 1 (%s): output does not contain expected failure: %q", result.Checks[1].Name, result.Checks[1].Output)
	}

	// Third check (go test -race) should also be run and pass (we don't
	// short-circuit on first failure — the operator wants the full picture).
	if !result.Checks[2].Passed {
		t.Errorf("check 2 (%s): Passed = false, want true", result.Checks[2].Name)
	}
}

func TestVerificationRunnerCancelledCheckBlocksReadyForPR(t *testing.T) {
	runner := &VerificationRunner{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sleep", "10")
			},
		},
	}

	checks := []CheckSpec{
		{Name: "hanging test", Command: "sleep", Args: []string{"10"}},
	}

	// Use a context with a very short timeout so the process starts but
	// gets killed before it completes.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	workspace := t.TempDir()
	result, err := runner.Run(ctx, workspace, checks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.ReadyForPR {
		t.Error("ReadyForPR = true, want false (check was cancelled)")
	}
	if len(result.Checks) > 0 && result.Checks[0].Passed {
		t.Error("cancelled check should not be marked as Passed")
	}
}

func TestVerificationOnlySkipsPlanningAndExecution(t *testing.T) {
	// Prove that VerificationOnly flows directly to verification without
	// invoking any planning or execution binaries (codex, pi, ralphex).
	// We track which binaries were invoked through the ProcessRunner.
	invoked := make(map[string]bool)

	runner := &VerificationRunner{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				invoked[name] = true
				return exec.CommandContext(ctx, "printf", "ok\n")
			},
		},
	}

	workspace := t.TempDir()
	result, err := VerifyWorkspaceForPR(context.Background(), workspace, runner.Runner)
	if err != nil {
		t.Fatalf("VerifyWorkspaceForPR: %v", err)
	}
	if !result.ReadyForPR {
		t.Error("ReadyForPR = false, want true")
	}

	// The only binaries invoked should be "go", never "codex", "pi", or "ralphex".
	for _, forbidden := range []string{"codex", "pi", "ralphex"} {
		if invoked[forbidden] {
			t.Errorf("forbidden binary %q was invoked during VerificationOnly path", forbidden)
		}
	}

	// "go" should have been invoked (for go vet, go test, go test -race).
	if !invoked["go"] {
		t.Error("go was not invoked — verification checks should run go vet/go test")
	}
}

func TestVerificationRunnerDefaultChecksAreValid(t *testing.T) {
	checks := DefaultVerificationChecks()
	if len(checks) == 0 {
		t.Fatal("DefaultVerificationChecks returned empty slice")
	}
	for _, c := range checks {
		if c.Name == "" {
			t.Error("check has empty Name")
		}
		if c.Command == "" {
			t.Errorf("check %q has empty Command", c.Name)
		}
	}
}

func TestVerificationRunnerMissingBinary(t *testing.T) {
	runner := &VerificationRunner{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				// Simulate a missing binary by using a non-existent path.
				return exec.CommandContext(ctx, "/nonexistent/binary-that-does-not-exist")
			},
		},
	}

	checks := []CheckSpec{
		{Name: "broken check", Command: "/nonexistent/binary-that-does-not-exist", Args: nil},
	}

	workspace := t.TempDir()
	_, err := runner.Run(context.Background(), workspace, checks)
	if err == nil {
		t.Fatal("Run = nil, want error for missing binary")
	}
}

func TestVerificationRunnerEmptyChecks(t *testing.T) {
	runner := &VerificationRunner{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "should not run")
			},
		},
	}

	workspace := t.TempDir()
	result, err := runner.Run(context.Background(), workspace, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// An empty check list means nothing failed, so ReadyForPR is true.
	if !result.ReadyForPR {
		t.Error("ReadyForPR = false, want true (no checks = nothing failed)")
	}
	if len(result.Checks) != 0 {
		t.Errorf("expected 0 check results, got %d", len(result.Checks))
	}
}

func TestVerificationRunnerAllChecksRunEvenOnFailure(t *testing.T) {
	// The verification runner must not short-circuit on the first failure.
	// The operator needs to see all failures at once.
	callCount := 0
	runner := &VerificationRunner{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				callCount++
				return exec.CommandContext(ctx, "sh", "-c", "echo failing; exit 1")
			},
		},
	}

	checks := []CheckSpec{
		{Name: "check 1", Command: "sh", Args: []string{"-c", "exit 1"}},
		{Name: "check 2", Command: "sh", Args: []string{"-c", "exit 1"}},
		{Name: "check 3", Command: "sh", Args: []string{"-c", "exit 1"}},
	}

	workspace := t.TempDir()
	result, err := runner.Run(context.Background(), workspace, checks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ReadyForPR {
		t.Error("ReadyForPR = true, want false")
	}
	if callCount != 3 {
		t.Errorf("called Run %d times, want 3 (all checks must run)", callCount)
	}
	if len(result.Checks) != 3 {
		t.Fatalf("expected 3 check results, got %d", len(result.Checks))
	}
	for i, cr := range result.Checks {
		if cr.Passed {
			t.Errorf("check %d (%s): Passed = true, want false", i, cr.Name)
		}
	}
}

func TestLinesToString(t *testing.T) {
	lines := []LineEvent{
		{Stream: "stdout", Text: "ok"},
		{Stream: "stderr", Text: "warning: something"},
	}
	output := linesToString(lines)
	if !strings.Contains(output, "[stdout] ok") {
		t.Errorf("output missing stdout line: %q", output)
	}
	if !strings.Contains(output, "[stderr] warning: something") {
		t.Errorf("output missing stderr line: %q", output)
	}
}

func TestIsVerificationOnly(t *testing.T) {
	if !IsVerificationOnly(dashboard.VerificationOnly) {
		t.Error("IsVerificationOnly(VerificationOnly) = false, want true")
	}
	if IsVerificationOnly(dashboard.Codex) {
		t.Error("IsVerificationOnly(Codex) = true, want false")
	}
	if IsVerificationOnly(dashboard.Pi) {
		t.Error("IsVerificationOnly(Pi) = true, want false")
	}
	if IsVerificationOnly(dashboard.Ralphex) {
		t.Error("IsVerificationOnly(Ralphex) = true, want false")
	}
	if IsVerificationOnly("") {
		t.Error("IsVerificationOnly(\"\") = true, want false")
	}
}

func TestVerifyWorkspaceForPRConvenience(t *testing.T) {
	invoked := make(map[string]bool)
	runner := &ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			invoked[name] = true
			return exec.CommandContext(ctx, "printf", "ok\n")
		},
	}

	wsDir := t.TempDir()
	result, err := VerifyWorkspaceForPR(context.Background(), wsDir, runner)
	if err != nil {
		t.Fatalf("VerifyWorkspaceForPR: %v", err)
	}
	if !result.ReadyForPR {
		t.Error("ReadyForPR = false, want true")
	}
	if len(result.Checks) != 3 {
		t.Fatalf("expected 3 default checks, got %d", len(result.Checks))
	}
	if !invoked["go"] {
		t.Error("go binary not invoked via default checks")
	}
}
