package live

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

func TestProcessRunnerSuccess(t *testing.T) {
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}

	result, err := runner.Run(context.Background(), "", nil, "printf", "%s\n%s", "hello", "world")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if result.Cancelled {
		t.Error("Cancelled = true, want false")
	}
	if len(result.Lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(result.Lines), result.Lines)
	}
	if result.Lines[0].Stream != "stdout" || result.Lines[0].Text != "hello" {
		t.Errorf("line 0 = %v, want stdout hello", result.Lines[0])
	}
	if result.Lines[1].Stream != "stdout" || result.Lines[1].Text != "world" {
		t.Errorf("line 1 = %v, want stdout world", result.Lines[1])
	}
	if result.Duration <= 0 {
		t.Errorf("duration = %v, want positive", result.Duration)
	}
}

func TestProcessRunnerNonZeroExit(t *testing.T) {
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}

	result, err := runner.Run(context.Background(), "", nil, "sh", "-c", "echo before; exit 1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", result.ExitCode)
	}
	if result.Cancelled {
		t.Error("Cancelled = true, want false")
	}
	if len(result.Lines) == 0 || result.Lines[0].Text != "before" {
		t.Errorf("lines = %v, want [before]", result.Lines)
	}
}

func TestProcessRunnerHangAndCancel(t *testing.T) {
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result, err := runner.Run(ctx, "", nil, "sleep", "10")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Cancelled {
		t.Error("Cancelled = false, want true after context timeout")
	}
	// sleep produces no output, so lines should be empty.
	if len(result.Lines) != 0 {
		t.Errorf("lines = %v, want empty", result.Lines)
	}
}

func TestProcessRunnerPartialOutputBeforeCancel(t *testing.T) {
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	var lines []string
	onLine := func(e LineEvent) error {
		lines = append(lines, e.Text)
		if e.Text == "first" {
			// Cancel after receiving "first" but before "second".
			cancel()
		}
		return nil
	}

	result, err := runner.Run(ctx, "", onLine, "sh", "-c", "echo first; sleep 10; echo second")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Cancelled {
		t.Error("Cancelled = false, want true")
	}
	// We must have captured "first" before the cancellation took effect.
	if len(lines) < 1 || lines[0] != "first" {
		t.Errorf("captured lines = %v, want at least [first]", lines)
	}
	// "second" must not appear because we cancelled before sleep finished.
	for _, l := range lines {
		if l == "second" {
			t.Error("second line was captured but should not have been produced")
		}
	}
}

func TestProcessRunnerStderrCapture(t *testing.T) {
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}

	result, err := runner.Run(context.Background(), "", nil, "sh", "-c", "echo stdout-line; echo stderr-line >&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	foundStdout := false
	foundStderr := false
	for _, line := range result.Lines {
		if line.Stream == "stdout" && line.Text == "stdout-line" {
			foundStdout = true
		}
		if line.Stream == "stderr" && line.Text == "stderr-line" {
			foundStderr = true
		}
	}
	if !foundStdout {
		t.Error("stdout line not found in captured output")
	}
	if !foundStderr {
		t.Error("stderr line not found in captured output")
	}
}

func TestProcessRunnerOnLineErrorKillsProcess(t *testing.T) {
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}

	var count int
	onLine := func(e LineEvent) error {
		count++
		return fmt.Errorf("stop")
	}

	result, err := runner.Run(context.Background(), "", onLine, "sh", "-c", "echo line1; echo line2")
	if err == nil {
		t.Fatal("expected error from onLine, got nil")
	}
	// The onLine error must be returned to the caller (the process may have
	// already buffered additional lines before the kill signal takes effect).
	if count < 1 {
		t.Error("onLine was never called")
	}
	_ = result
}

func TestProcessRunnerExitCodeNegativeOneOnSignal(t *testing.T) {
	// exitCode should return -1 for signal-killed processes.
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, err := runner.Run(ctx, "", nil, "sleep", "10")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The exit code should be -1 when killed by a signal.
	if result.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1 for signal-killed process", result.ExitCode)
	}
}

func TestProcessRunnerProcessGroupKillsDescendants(t *testing.T) {
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start a shell that spawns two background sleep processes and waits
	// for them. When we cancel the context, the process-group signal must
	// kill the shell and all its descendants.
	done := make(chan *RunResult, 1)
	go func() {
		result, err := runner.Run(ctx, "", nil, "sh", "-c", "sleep 100 & sleep 100 & wait")
		if err != nil {
			t.Logf("Run error: %v", err)
		}
		done <- result
	}()

	// Give the shell time to spawn its children.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// The run must complete quickly after cancellation (within 2 seconds).
	select {
	case result := <-done:
		if !result.Cancelled {
			t.Error("Cancelled = false, want true after context cancellation")
		}
		if result.ExitCode != -1 {
			t.Errorf("exit code = %d, want -1 for signal-killed process group", result.ExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process group not terminated within 2s of cancellation — descendants may have survived")
	}
}

func TestProcessRunnerSetsProcessGroup(t *testing.T) {
	// Verify that the Command field receives context and that the runner
	// sets Setpgid on the returned *exec.Cmd before starting the child.
	var capturedCmd *exec.Cmd
	runner := ProcessRunner{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, name, args...)
			capturedCmd = cmd
			return cmd
		},
	}

	result, err := runner.Run(context.Background(), "", nil, "printf", "ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if capturedCmd == nil {
		t.Fatal("Command was never called")
	}
	if capturedCmd.SysProcAttr == nil || !capturedCmd.SysProcAttr.Setpgid {
		t.Error("SysProcAttr.Setpgid was not set to true on the command")
	}
}
