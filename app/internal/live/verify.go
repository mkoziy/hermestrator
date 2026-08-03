package live

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// CheckSpec describes a single verification check to run inside the
// workspace. Name is a short human-readable label; Command and Args are
// passed to ProcessRunner.Run.
type CheckSpec struct {
	Name    string
	Command string
	Args    []string
}

// CheckResult captures the outcome of a single verification check.
type CheckResult struct {
	Name     string
	ExitCode int
	Passed   bool // true when ExitCode == 0
	Output   string
}

// VerificationResult aggregates the results of all verification checks.
// ReadyForPR is true only when every check passed.
type VerificationResult struct {
	ReadyForPR bool
	Checks     []CheckResult
}

// VerificationRunner executes canonical build/test/lint/race checks inside a
// workspace via ProcessRunner and gates a ReadyForPR boolean. PR creation
// itself is out of scope per issue #4 — this only produces the pass/fail
// gate and blocks on failure.
type VerificationRunner struct {
	// Runner executes each check command. When nil, a ProcessRunner with
	// exec.CommandContext is used.
	Runner *ProcessRunner
}

// DefaultVerificationChecks returns the standard canonical checks for a Go
// project. Callers that need different checks can supply their own slice.
func DefaultVerificationChecks() []CheckSpec {
	return []CheckSpec{
		{Name: "go build", Command: "go", Args: []string{"build", "./..."}},
		{Name: "go vet", Command: "go", Args: []string{"vet", "./..."}},
		{Name: "golangci-lint", Command: "go", Args: []string{"tool", "golangci-lint", "run"}},
		{Name: "go test", Command: "go", Args: []string{"test", "./..."}},
		{Name: "go test -race", Command: "go", Args: []string{"test", "-race", "./..."}},
	}
}

// Run executes checks inside workspacePath and returns an aggregated result.
// ReadyForPR is true only when every check in checks passes (exit code 0).
//
// The caller is responsible for deciding which checks to run. For a Go
// project the caller typically passes DefaultVerificationChecks().
//
// When VerificationOnly is the selected executor kind, the caller must
// route directly to this method, skipping planning, critique, and any
// executor binary invocation entirely. The DirectExecutor and
// RalphexExecutor types already reject VerificationOnly; callers should
// branch on that kind before reaching them.
func (v *VerificationRunner) Run(ctx context.Context, workspacePath string, checks []CheckSpec) (VerificationResult, error) {
	runner := v.Runner
	if runner == nil {
		runner = &ProcessRunner{Command: exec.CommandContext}
	}

	var result VerificationResult
	result.Checks = make([]CheckResult, len(checks))
	allPassed := true

	for i, check := range checks {
		cr := CheckResult{Name: check.Name}
		runResult, err := runner.Run(ctx, workspacePath, nil, check.Command, check.Args...)
		if err != nil {
			// A process-start failure (e.g. missing binary) is terminal.
			return result, fmt.Errorf("verification check %q: %w", check.Name, err)
		}
		cr.ExitCode = runResult.ExitCode
		cr.Passed = runResult.ExitCode == 0 && !runResult.Cancelled
		cr.Output = linesToString(runResult.Lines)
		if !cr.Passed {
			allPassed = false
		}
		result.Checks[i] = cr
	}

	result.ReadyForPR = allPassed
	return result, nil
}

// VerifyWorkspaceForPR is a convenience entry point for the VerificationOnly
// path. It runs the default checks against the workspace and returns the
// aggregated result. The caller (inside app.go's executorRun handler)
// checks ExecutorKind == VerificationOnly and calls this instead of
// invoking codex/pi/ralphex.
//
// This function exists so the handler stays small and the decision of
// which checks to run is owned here, not spread across packages.
func VerifyWorkspaceForPR(ctx context.Context, workspacePath string, runner *ProcessRunner) (VerificationResult, error) {
	v := &VerificationRunner{Runner: runner}
	return v.Run(ctx, workspacePath, DefaultVerificationChecks())
}

// linesToString joins captured LineEvent lines preserving stream labels
// for diagnostics.
func linesToString(lines []LineEvent) string {
	var sb strings.Builder
	for _, l := range lines {
		// Include stream label so the operator can distinguish
		// stdout from stderr when diagnosing failures.
		fmt.Fprintf(&sb, "[%s] %s\n", l.Stream, l.Text)
	}
	return sb.String()
}

// IsVerificationOnly reports whether the executor kind means the workflow
// should skip directly to the verification gate, bypassing planning,
// critique, and executor invocation entirely.
func IsVerificationOnly(kind dashboard.ExecutorKind) bool {
	return kind == dashboard.VerificationOnly
}
