package live

import (
	"context"
	"fmt"
	"os/exec"
)

// RalphexExecutor invokes ralphex for non-interactive execution of a
// previously generated plan. It uses the real ralphex execution path:
//
//	ralphex --config-dir <configDir> <planFile>
//
// This is ralphex's task/review/finalize phases. The PM never calls
// ralphex --plan or bare ralphex through this type.
//
// By construction this type contains no file-write calls — it reads the
// plan file from disk (via ralphex) but never opens it for writing.
// Callers are expected to have generated and validated the plan through
// the Planner before handing it to RalphexExecutor.
type RalphexExecutor struct {
	// Runner executes the ralphex binary and streams stdout/stderr.
	Runner *ProcessRunner
}

// Run invokes ralphex for non-interactive execution of the plan at
// planPath using the PM-owned execution profile at configDir. It returns
// the final RunResult (exit code, duration, captured lines, cancellation
// status) so callers can surface execution telemetry in the dashboard.
func (e *RalphexExecutor) Run(ctx context.Context, configDir, planPath string) (*RunResult, error) {
	if configDir == "" {
		return nil, fmt.Errorf("ralphex execution: config-dir is required")
	}
	if planPath == "" {
		return nil, fmt.Errorf("ralphex execution: plan file path is required")
	}

	runner := e.Runner
	if runner == nil {
		runner = &ProcessRunner{Command: exec.CommandContext}
	}

	result, err := runner.Run(ctx, nil, "ralphex", "--config-dir", configDir, planPath)
	if err != nil {
		return result, fmt.Errorf("ralphex execution: %w", err)
	}
	return result, nil
}
