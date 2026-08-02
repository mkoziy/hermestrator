package live

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	// RalphexPlanningConfigDir is a PM-owned ralphex planning profile. The
	// upstream ralphex plan UI is interactive, so this adapter reads its
	// executor settings and invokes that executor directly, headlessly.
	RalphexPlanningConfigDir string
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

	name, args, err := p.planCommandForExecutor(profile, executorKind, scope)
	if err != nil {
		return "", err
	}

	runner := p.Runner
	if runner == nil {
		runner = &ProcessRunner{Command: exec.CommandContext}
	}
	result, err := runner.Run(ctx, workspacePath, nil, name, args...)
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

func (p *Planner) planCommandForExecutor(profile PlanningProfile, executorKind dashboard.ExecutorKind, scope string) (string, []string, error) {
	if executorKind != dashboard.Ralphex {
		name, args := p.planCommand(profile, scope)
		return name, args, nil
	}
	if p.RalphexPlanningConfigDir == "" {
		return "", nil, fmt.Errorf("ralphex planning config directory is required")
	}
	settings, err := loadRalphexPlanningSettings(filepath.Join(p.RalphexPlanningConfigDir, "config"))
	if err != nil {
		return "", nil, err
	}
	if settings.Executor != "codex" {
		return "", nil, fmt.Errorf("ralphex planning profile executor %q is unsupported", settings.Executor)
	}
	args := []string{"exec", "--model", settings.Model, "-c", fmt.Sprintf("model_reasoning_effort=%q", settings.Effort)}
	if settings.Sandbox != "" {
		args = append(args, "--sandbox", settings.Sandbox)
	}
	args = append(args, buildRalphexPlanPrompt(scope))
	return "codex", args, nil
}

type ralphexPlanningSettings struct{ Executor, Model, Effort, Sandbox string }

func loadRalphexPlanningSettings(path string) (ralphexPlanningSettings, error) {
	f, err := os.Open(path)
	if err != nil {
		return ralphexPlanningSettings{}, fmt.Errorf("read ralphex planning profile: %w", err)
	}
	defer func() { _ = f.Close() }()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	if err := scanner.Err(); err != nil {
		return ralphexPlanningSettings{}, fmt.Errorf("read ralphex planning profile: %w", err)
	}
	return ralphexPlanningSettings{Executor: values["executor"], Model: defaultString(values["codex_model"], "gpt-5.6-terra"), Effort: defaultString(values["codex_reasoning_effort"], "medium"), Sandbox: values["codex_sandbox"]}, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
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
		args := []string{"-p", prompt}
		if profile.Model != "" {
			args = append([]string{"--model", profile.Model}, args...)
		}
		return "pi", args
	default:
		args := []string{"exec"}
		if profile.Model != "" {
			args = append(args, "--model", profile.Model)
		}
		return "codex", append(args, prompt)
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

func buildRalphexPlanPrompt(scope string) string {
	return fmt.Sprintf("Generate a ralphex-compatible implementation plan for this scope. Inspect the repository first. Output only markdown beginning with '# Plan:', include '## Validation Commands', and one or more '### Task N:' sections with unchecked checklist items.\n\nScope: %s", scope)
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
