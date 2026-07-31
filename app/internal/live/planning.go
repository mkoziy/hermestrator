package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// PlanningProfile contains the PM-owned settings for plan generation. It is
// stored in a simple JSON file; it deliberately does not use ralphex's
// read_cfg schema or live under the existing ./ralphex/ tree.
type PlanningProfile struct {
	// Planner selects which binary generates the plan: "codex" or "pi".
	Planner string `json:"planner"`
	// Model is the model name passed to the chosen binary.
	Model string `json:"model"`
	// Effort controls how exhaustively the planner explores solutions
	// ("low", "medium", "high").
	Effort string `json:"effort"`
	// Sandbox controls whether the plan encourages sandboxed execution.
	Sandbox string `json:"sandbox"`
}

// DefaultPlanningProfile returns a profile with safe defaults suitable for
// most repositories.
func DefaultPlanningProfile() PlanningProfile {
	return PlanningProfile{
		Planner: "codex",
		Model:   "default",
		Effort:  "medium",
		Sandbox: "no",
	}
}

// Planner generates a plan file by invoking codex or pi directly via the
// ProcessRunner. It never calls ralphex --plan. The produced plan is
// written to PlanFileName inside the workspace and validated against the
// plan contract before returning.
type Planner struct {
	// Runner executes the codex / pi binary and streams output.
	Runner *ProcessRunner
	// ProfilePath is the path to the PM-owned planning profile JSON file.
	// When empty, DefaultPlanningProfile is used.
	ProfilePath string
}

// GeneratePlan runs the planning binary (codex exec or pi -p) inside the
// given workspace and writes the result to dashboard.PlanFileName. It
// returns the generated plan content.
//
// The executorKind parameter does not control planning — planning always
// uses codex or pi as directed by the planning profile, never ralphex
// --plan. executorKind is accepted so callers can surface it in logs/
// artifacts but the planner ignores it for binary selection.
func (p *Planner) GeneratePlan(ctx context.Context, workspacePath string, executorKind dashboard.ExecutorKind, scope string) (string, error) {
	profile, err := p.loadProfile()
	if err != nil {
		return "", fmt.Errorf("load planning profile: %w", err)
	}

	name, args := p.planCommand(profile, scope)

	runner := p.Runner
	if runner == nil {
		runner = &ProcessRunner{Command: exec.CommandContext}
	}
	runner.Dir = workspacePath

	result, err := runner.Run(ctx, nil, name, args...)
	if err != nil {
		return "", fmt.Errorf("plan generation: %w", err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("plan generation failed with exit code %d", result.ExitCode)
	}

	content := collectStdout(result)
	if content == "" {
		return "", fmt.Errorf("plan generation produced no output")
	}

	if err := dashboard.ValidatePlan(content); err != nil {
		return "", fmt.Errorf("generated plan is structurally invalid: %w", err)
	}

	planPath := filepath.Join(workspacePath, dashboard.PlanFileName)
	if err := os.WriteFile(planPath, []byte(content), 0o640); err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}

	return content, nil
}

// loadProfile reads the planning profile from disk or returns defaults.
func (p *Planner) loadProfile() (PlanningProfile, error) {
	if p.ProfilePath == "" {
		return DefaultPlanningProfile(), nil
	}
	data, err := os.ReadFile(p.ProfilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultPlanningProfile(), nil
		}
		return PlanningProfile{}, fmt.Errorf("read planning profile: %w", err)
	}
	var profile PlanningProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return PlanningProfile{}, fmt.Errorf("decode planning profile: %w", err)
	}
	if profile.Planner == "" {
		profile.Planner = "codex"
	}
	return profile, nil
}

// planCommand returns the binary name and arguments for the planning step.
// The profile's Planner field selects codex or pi; the default is codex.
func (p *Planner) planCommand(profile PlanningProfile, scope string) (string, []string) {
	prompt := buildPlanPrompt(profile, scope)
	switch profile.Planner {
	case "pi":
		return "pi", []string{"-p", prompt}
	default:
		return "codex", []string{"exec", prompt}
	}
}

// buildPlanPrompt constructs the bounded planning prompt sent to codex or pi.
func buildPlanPrompt(profile PlanningProfile, scope string) string {
	effort := profile.Effort
	if effort == "" {
		effort = "medium"
	}
	return fmt.Sprintf(
		"Generate a plan for the following scope with effort=%s, sandbox=%s. "+
			"Output only the plan in a file called %s. "+
			"The plan must start with a '# Plan:' title line and contain one or more '### Task N:' sections.\n\n"+
			"Scope: %s",
		effort, profile.Sandbox, dashboard.PlanFileName, scope,
	)
}

// collectStdout concatenates all stdout lines from a run result.
func collectStdout(result *RunResult) string {
	if result == nil || len(result.Lines) == 0 {
		return ""
	}
	var out string
	for _, line := range result.Lines {
		if line.Stream == "stdout" {
			if out != "" {
				out += "\n"
			}
			out += line.Text
		}
	}
	return out
}
