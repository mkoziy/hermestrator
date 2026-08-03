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

func TestDirectExecutorCodexInvocationShape(t *testing.T) {
	workspace := t.TempDir()
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	planContent := `# Plan: Test plan

### Task 1: Set up the project

### Task 2: Implement the core feature
`
	if err := os.WriteFile(planPath, []byte(planContent), 0o640); err != nil {
		t.Fatal(err)
	}

	var invokedName string
	var invokedArgs []string

	executor := DirectExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				invokedName = name
				invokedArgs = args
				return exec.CommandContext(ctx, "printf", "executing...\ndone")
			},
		},
	}

	_, err := executor.Run(context.Background(), dashboard.Codex, workspace)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if invokedName != "codex" {
		t.Errorf("invoked binary = %q, want codex", invokedName)
	}
	if len(invokedArgs) != 2 {
		t.Fatalf("invoked args = %v, want 2 elements [exec, <task>]", invokedArgs)
	}
	if invokedArgs[0] != "exec" {
		t.Errorf("args[0] = %q, want exec", invokedArgs[0])
	}
	task := invokedArgs[1]
	if !strings.Contains(task, planContent) {
		t.Errorf("task does not contain plan content: %q", task)
	}
	if !strings.Contains(task, "Execute the following plan") {
		t.Errorf("task does not contain execution framing: %q", task)
	}
}

func TestDirectExecutorPiInvocationShape(t *testing.T) {
	workspace := t.TempDir()
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	planContent := `# Plan: Test plan

### Task 1: Set up the project

### Task 2: Implement the core feature
`
	if err := os.WriteFile(planPath, []byte(planContent), 0o640); err != nil {
		t.Fatal(err)
	}

	var invokedName string
	var invokedArgs []string

	executor := DirectExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				invokedName = name
				invokedArgs = args
				return exec.CommandContext(ctx, "printf", "executing...\ndone")
			},
		},
	}

	_, err := executor.Run(context.Background(), dashboard.Pi, workspace)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if invokedName != "pi" {
		t.Errorf("invoked binary = %q, want pi", invokedName)
	}
	if len(invokedArgs) != 2 {
		t.Fatalf("invoked args = %v, want 2 elements [-p, <task>]", invokedArgs)
	}
	if invokedArgs[0] != "-p" {
		t.Errorf("args[0] = %q, want -p", invokedArgs[0])
	}
	task := invokedArgs[1]
	if !strings.Contains(task, planContent) {
		t.Errorf("task does not contain plan content: %q", task)
	}
}

func TestDirectExecutorNeverInvokesRalphex(t *testing.T) {
	workspace := t.TempDir()
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	if err := os.WriteFile(planPath, []byte("# Plan: test\n\n### Task 1: Do something\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	// Both codex and pi paths must never invoke ralphex.
	kinds := []dashboard.ExecutorKind{dashboard.Codex, dashboard.Pi}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			var invokedName string
			executor := DirectExecutor{
				Runner: &ProcessRunner{
					Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
						invokedName = name
						return exec.CommandContext(ctx, "printf", "ok")
					},
				},
			}

			_, err := executor.Run(context.Background(), kind, workspace)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if invokedName == "ralphex" {
				t.Errorf("ralphex was invoked for executor kind %q; direct execution must never call ralphex", kind)
			}
		})
	}
}

func TestDirectExecutorUnsupportedKind(t *testing.T) {
	workspace := t.TempDir()
	// No plan file needed — the kind check happens before file access.

	executor := DirectExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "should not run")
			},
		},
	}

	_, err := executor.Run(context.Background(), dashboard.Ralphex, workspace)
	if err == nil {
		t.Fatal("Run = nil, want error for unsupported executor kind Ralphex")
	}
	if !strings.Contains(err.Error(), "unsupported executor kind") {
		t.Errorf("error = %v, want 'unsupported executor kind'", err)
	}
}

func TestDirectExecutorMissingPlanFile(t *testing.T) {
	workspace := t.TempDir()
	// No plan file written.

	executor := DirectExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "should not run")
			},
		},
	}

	_, err := executor.Run(context.Background(), dashboard.Codex, workspace)
	if err == nil {
		t.Fatal("Run = nil, want error for missing plan file")
	}
	if !strings.Contains(err.Error(), "read plan file") {
		t.Errorf("error = %v, want 'read plan file'", err)
	}
}

func TestDirectExecutorStreamingOutput(t *testing.T) {
	workspace := t.TempDir()
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	if err := os.WriteFile(planPath, []byte("# Plan: test\n\n### Task 1: Do something\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	executor := DirectExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "task 1 done\ntask 2 done\ntask 3 done")
			},
		},
	}

	result, err := executor.Run(context.Background(), dashboard.Codex, workspace)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if result.Cancelled {
		t.Error("Cancelled = true, want false")
	}
	if len(result.Lines) != 3 {
		t.Fatalf("got %d lines, want 3: %v", len(result.Lines), result.Lines)
	}
}

func TestDirectExecutorNonZeroExit(t *testing.T) {
	workspace := t.TempDir()
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	if err := os.WriteFile(planPath, []byte("# Plan: test\n\n### Task 1: Do something\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	executor := DirectExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", "echo 'execution failed' >&2; exit 3")
			},
		},
	}

	result, err := executor.Run(context.Background(), dashboard.Pi, workspace)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", result.ExitCode)
	}
}

func TestDirectExecutorDefaultRunner(t *testing.T) {
	// When no Runner is provided, the executor uses exec.CommandContext
	// by default (via ProcessRunner defaults). The struct accepts nil
	// Runner and falls through to defaults. Actual invocation of the
	// real codex/pi binary is not done here; the injected-Runner tests
	// above cover the invocation shape.
	_ = DirectExecutor{} // nil Runner compiles and is usable
}

func TestDirectExecutorVerificationOnlyRejected(t *testing.T) {
	workspace := t.TempDir()
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	if err := os.WriteFile(planPath, []byte("# Plan: test\n\n### Task 1: Do something\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	executor := DirectExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "should not run")
			},
		},
	}

	_, err := executor.Run(context.Background(), dashboard.VerificationOnly, workspace)
	if err == nil {
		t.Fatal("Run = nil, want error for VerificationOnly kind")
	}
	if !strings.Contains(err.Error(), "unsupported executor kind") {
		t.Errorf("error = %v, want 'unsupported executor kind'", err)
	}
}
