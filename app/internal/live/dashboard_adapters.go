package live

import (
	"context"
	"fmt"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// DashboardCritiquer keeps live result types behind the dashboard boundary.
type DashboardCritiquer struct{ Critiquer *Critiquer }

func (c DashboardCritiquer) CritiquePlan(ctx context.Context, workspacePath string, kind dashboard.ExecutorKind, scope string) (*dashboard.CritiqueResult, error) {
	if c.Critiquer == nil {
		return nil, fmt.Errorf("dashboard critiquer is not configured")
	}
	result, err := c.Critiquer.CritiquePlan(ctx, workspacePath, kind, scope)
	if result == nil {
		return nil, err
	}
	return &dashboard.CritiqueResult{Approved: result.Approved, Findings: result.Findings, RegenerationRounds: result.RegenerationRounds, Blocked: result.Blocked}, err
}

type DashboardPreflight struct{ Preflight *Preflight }

func (p DashboardPreflight) Verify(ctx context.Context, workspacePath, expectedRemote string) dashboard.PreflightResult {
	preflight := p.Preflight
	if preflight == nil {
		preflight = &Preflight{}
	}
	result := preflight.Verify(ctx, workspacePath, expectedRemote)
	failures := make([]dashboard.PreflightFailure, len(result.Failures))
	for i, failure := range result.Failures {
		failures[i] = dashboard.PreflightFailure{Check: failure.Check, Detail: failure.Detail}
	}
	return dashboard.PreflightResult{Passed: result.Passed, Failures: failures}
}

type DashboardVerificationRunner struct{ Runner *VerificationRunner }

func (v DashboardVerificationRunner) Run(ctx context.Context, workspacePath string, checks []dashboard.CheckSpec) (dashboard.VerificationResult, error) {
	runner := v.Runner
	if runner == nil {
		runner = &VerificationRunner{}
	}
	var liveChecks []CheckSpec
	if checks == nil {
		liveChecks = DefaultVerificationChecks()
	} else {
		liveChecks = make([]CheckSpec, len(checks))
		for i, check := range checks {
			liveChecks[i] = CheckSpec{Name: check.Name, Command: check.Command, Args: check.Args}
		}
	}
	result, err := runner.Run(ctx, workspacePath, liveChecks)
	converted := dashboard.VerificationResult{ReadyForPR: result.ReadyForPR, Checks: make([]dashboard.CheckResult, len(result.Checks))}
	for i, check := range result.Checks {
		converted.Checks[i] = dashboard.CheckResult{Name: check.Name, ExitCode: check.ExitCode, Passed: check.Passed, Output: check.Output}
	}
	return converted, err
}

var (
	_ dashboard.Critiquer          = DashboardCritiquer{}
	_ dashboard.Preflight          = DashboardPreflight{}
	_ dashboard.VerificationRunner = DashboardVerificationRunner{}
)
