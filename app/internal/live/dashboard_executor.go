package live

import (
	"context"
	"fmt"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// DashboardExecutorRunner adapts ProcessRunner to the dashboard's executor
// boundary. Supplying the workspace explicitly ensures every executor starts
// inside its isolated issue clone rather than inheriting the PM process cwd.
type DashboardExecutorRunner struct {
	Runner *ProcessRunner
}

func (r DashboardExecutorRunner) Run(ctx context.Context, workspacePath string, onLine func(string) error, name string, args ...string) (dashboard.ExecutorRunResult, error) {
	runner := r.Runner
	if runner == nil {
		runner = &ProcessRunner{}
	}
	result, err := runner.Run(ctx, workspacePath, func(event LineEvent) error {
		if onLine == nil {
			return nil
		}
		return onLine(event.Text)
	}, name, args...)
	if result == nil {
		return dashboard.ExecutorRunResult{}, err
	}
	run := dashboard.ExecutorRunResult{ExitCode: result.ExitCode, Duration: result.Duration, Cancelled: result.Cancelled}
	if err != nil {
		return run, fmt.Errorf("run executor: %w", err)
	}
	return run, nil
}

var _ dashboard.ExecutorRunner = DashboardExecutorRunner{}
