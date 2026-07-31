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

func TestRalphexExecutorInvocationShape(t *testing.T) {
	// The PM must invoke ralphex with --config-dir <dir> <plan-file>.
	// Capture the actual arguments passed to the injected command.

	var invokedName string
	var invokedArgs []string

	executor := RalphexExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				invokedName = name
				invokedArgs = args
				// Return a successful dummy process.
				return exec.CommandContext(ctx, "printf", "task 1 done\ntask 2 done")
			},
		},
	}

	_, err := executor.Run(context.Background(), "/pm/execution-profiles/default", "/workspace/42/plan.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if invokedName != "ralphex" {
		t.Errorf("invoked binary = %q, want ralphex", invokedName)
	}
	if len(invokedArgs) != 3 {
		t.Fatalf("invoked args = %v, want 3 elements [--config-dir, <dir>, <plan-file>]", invokedArgs)
	}
	if invokedArgs[0] != "--config-dir" {
		t.Errorf("args[0] = %q, want --config-dir", invokedArgs[0])
	}
	if invokedArgs[1] != "/pm/execution-profiles/default" {
		t.Errorf("args[1] = %q, want /pm/execution-profiles/default", invokedArgs[1])
	}
	if invokedArgs[2] != "/workspace/42/plan.md" {
		t.Errorf("args[2] = %q, want /workspace/42/plan.md", invokedArgs[2])
	}
}

func TestRalphexExecutorNoPlanFileWrite(t *testing.T) {
	// Create a real plan file, run the executor, and assert the file's
	// content and modification time are unchanged — proving the PM side
	// never writes to the plan during execution.

	workspace := t.TempDir()
	planPath := filepath.Join(workspace, dashboard.PlanFileName)

	originalContent := `# Plan: Test plan

### Task 1: Set up

### Task 2: Implement
`
	if err := os.WriteFile(planPath, []byte(originalContent), 0o640); err != nil {
		t.Fatal(err)
	}

	originalStat, err := os.Stat(planPath)
	if err != nil {
		t.Fatal(err)
	}

	executor := RalphexExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "executing plan...\nall tasks complete")
			},
		},
	}

	_, err = executor.Run(context.Background(), "/pm/config", planPath)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify content unchanged.
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan file after execution: %v", err)
	}
	if string(data) != originalContent {
		t.Errorf("plan file was modified during execution:\ngot:  %q\nwant: %q", string(data), originalContent)
	}

	// Verify mtime unchanged.
	afterStat, err := os.Stat(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(originalStat.ModTime()) {
		t.Errorf("plan file mtime changed: was %v, now %v", originalStat.ModTime(), afterStat.ModTime())
	}
}

func TestRalphexExecutorMissingConfigDir(t *testing.T) {
	executor := RalphexExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "should not run")
			},
		},
	}

	_, err := executor.Run(context.Background(), "", "/workspace/plan.md")
	if err == nil {
		t.Fatal("Run = nil, want error for missing config-dir")
	}
	if !strings.Contains(err.Error(), "config-dir is required") {
		t.Errorf("error = %v, want 'config-dir is required'", err)
	}
}

func TestRalphexExecutorMissingPlanPath(t *testing.T) {
	executor := RalphexExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "should not run")
			},
		},
	}

	_, err := executor.Run(context.Background(), "/pm/config", "")
	if err == nil {
		t.Fatal("Run = nil, want error for missing plan path")
	}
	if !strings.Contains(err.Error(), "plan file path is required") {
		t.Errorf("error = %v, want 'plan file path is required'", err)
	}
}

func TestRalphexExecutorStreamingOutput(t *testing.T) {
	// Verify that ralphex stdout is streamed and captured in the result.

	executor := RalphexExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "line1\nline2\nline3")
			},
		},
	}

	result, err := executor.Run(context.Background(), "/pm/config", "/workspace/plan.md")
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
	if result.Lines[0].Text != "line1" {
		t.Errorf("line 0 = %q, want line1", result.Lines[0].Text)
	}
}

func TestRalphexExecutorNonZeroExit(t *testing.T) {
	executor := RalphexExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", "echo 'ralphex failed' >&2; exit 2")
			},
		},
	}

	result, err := executor.Run(context.Background(), "/pm/config", "/workspace/plan.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 2 {
		t.Errorf("exit code = %d, want 2", result.ExitCode)
	}
}

func TestRalphexExecutorNeverCallsRalphexPlan(t *testing.T) {
	// RalphexExecutor must always use --config-dir and a positional plan
	// file argument. It must never call ralphex --plan or bare ralphex.
	// The --plan flag must not appear in the args.

	executor := RalphexExecutor{
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				for _, a := range args {
					if a == "--plan" {
						t.Error("ralphex --plan was invoked; execution must use --config-dir + plan file, never --plan")
					}
				}
				return exec.CommandContext(ctx, "printf", "ok")
			},
		},
	}

	_, err := executor.Run(context.Background(), "/pm/config", "/workspace/plan.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRalphexExecutorDefaultRunner(t *testing.T) {
	// When no Runner is provided, the executor should use exec.CommandContext
	// by default (via ProcessRunner defaults). The struct accepts nil Runner
	// and falls through to defaults. Actual invocation of the real ralphex
	// binary is not done here; the injected-Runner tests above cover the
	// invocation shape.
	_ = RalphexExecutor{} // nil Runner compiles and is usable
}
