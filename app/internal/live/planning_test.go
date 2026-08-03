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

const validPlan = `# Plan: Test plan

### Task 1: Set up the project

This task initializes the repository.

### Task 2: Implement the core feature

This task delivers the primary feature.`

const invalidPlanNoTitle = `### Task 1: Do something

No title line here.`

const invalidPlanNoTask = `# Plan: My plan

Just a description, no task sections at all.`

// planCommandScript builds a shell snippet that writes the given content to
// stdout and exits 0. The content is escaped for use in a single-quoted
// printf argument.
func planCommandScript(content string) string {
	// Escape single quotes for shell: '\'' closes the quoted string, inserts a
	// literal single quote, and re-opens.
	escaped := strings.ReplaceAll(content, "'", `'\''`)
	return "printf '" + escaped + "'"
}

func TestPlannerGeneratePlanCodexPath(t *testing.T) {
	workspace := t.TempDir()

	planner := Planner{
		ProfilePath: "",
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				// Verify the caller invoked "codex exec".
				return exec.CommandContext(ctx, "sh", "-c", planCommandScript(validPlan))
			},
		},
	}

	content, err := planner.GeneratePlan(context.Background(), workspace, dashboard.Codex, "medium scope test")
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if content != validPlan {
		t.Errorf("plan content mismatch:\ngot:  %q\nwant: %q", content, validPlan)
	}

	// Verify the plan file was written to the workspace.
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan file: %v", err)
	}
	if string(data) != validPlan {
		t.Errorf("plan file content mismatch:\ngot:  %q\nwant: %q", string(data), validPlan)
	}
}

func TestPlannerGeneratePlanPiPath(t *testing.T) {
	workspace := t.TempDir()
	profilePath := filepath.Join(t.TempDir(), "planning-profile.json")
	if err := os.WriteFile(profilePath, []byte(`{"planner":"pi","model":"gpt-5","effort":"high","sandbox":"yes"}`), 0o640); err != nil {
		t.Fatal(err)
	}

	planner := Planner{
		ProfilePath: profilePath,
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				// Verify the caller invoked "pi -p" with the prompt.
				return exec.CommandContext(ctx, "sh", "-c", planCommandScript(validPlan))
			},
		},
	}

	content, err := planner.GeneratePlan(context.Background(), workspace, dashboard.Pi, "pi scope test")
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if content != validPlan {
		t.Errorf("plan content mismatch:\ngot:  %q\nwant: %q", content, validPlan)
	}

	// Verify the plan file was written.
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("plan file not found: %v", err)
	}
}

func TestPlannerGeneratePlanInvalidStructure(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr string
	}{
		{
			name:    "no title line",
			output:  invalidPlanNoTitle,
			wantErr: "structurally invalid",
		},
		{
			name:    "no task sections",
			output:  invalidPlanNoTask,
			wantErr: "structurally invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()

			planner := Planner{
				ProfilePath: "",
				Runner: &ProcessRunner{
					Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
						return exec.CommandContext(ctx, "sh", "-c", planCommandScript(tt.output))
					},
				},
			}

			_, err := planner.GeneratePlan(context.Background(), workspace, dashboard.Codex, "invalid test")
			if err == nil {
				t.Fatal("GeneratePlan = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tt.wantErr)
			}

			// Verify no plan file was written on validation failure.
			planPath := filepath.Join(workspace, dashboard.PlanFileName)
			if _, statErr := os.Stat(planPath); !os.IsNotExist(statErr) {
				t.Errorf("plan file was written despite validation failure: %s", planPath)
			}
		})
	}
}

func TestPlannerGeneratePlanNonZeroExit(t *testing.T) {
	workspace := t.TempDir()

	planner := Planner{
		ProfilePath: "",
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", "echo 'something went wrong' >&2; exit 1")
			},
		},
	}

	_, err := planner.GeneratePlan(context.Background(), workspace, dashboard.Codex, "failing test")
	if err == nil {
		t.Fatal("GeneratePlan = nil, want error")
	}
	if !strings.Contains(err.Error(), "exit code 1") {
		t.Errorf("error = %v, want exit code 1", err)
	}
}

func TestPlannerGeneratePlanEmptyOutput(t *testing.T) {
	workspace := t.TempDir()

	planner := Planner{
		ProfilePath: "",
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", "true") // exits 0 with no output
			},
		},
	}

	_, err := planner.GeneratePlan(context.Background(), workspace, dashboard.Codex, "empty test")
	if err == nil {
		t.Fatal("GeneratePlan = nil, want error for empty output")
	}
	if !strings.Contains(err.Error(), "no output") {
		t.Errorf("error = %v, want 'no output'", err)
	}
}

func TestPlannerDefaultProfile(t *testing.T) {
	workspace := t.TempDir()
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config"), []byte("executor=codex\ncodex_model=ralphex-model\ncodex_reasoning_effort=high\ncodex_sandbox=workspace-write\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	var invokedName string
	var invokedArgs []string

	planner := Planner{
		ProfilePath:              "",
		RalphexPlanningConfigDir: configDir,
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				invokedName = name
				invokedArgs = args
				return exec.CommandContext(ctx, "sh", "-c", planCommandScript(validPlan))
			},
		},
	}

	_, err := planner.GeneratePlan(context.Background(), workspace, dashboard.Ralphex, "default profile test")
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}

	// The PM-owned ralphex adapter invokes the profile's executor directly;
	// it never invokes the interactive ralphex plan UI.
	if invokedName != "codex" {
		t.Errorf("invoked binary = %q, want codex", invokedName)
	}
	if len(invokedArgs) < 2 || invokedArgs[0] != "exec" {
		t.Errorf("invoked args = %v, want [exec, ...]", invokedArgs)
	}

	if !strings.Contains(strings.Join(invokedArgs, " "), "ralphex-model") {
		t.Errorf("ralphex planning model was not passed: %v", invokedArgs)
	}
	if invokedName == "ralphex" {
		t.Error("interactive ralphex was invoked instead of the PM-owned headless adapter")
	}
}

func TestPlannerRalphexPlanningRequiresDedicatedConfig(t *testing.T) {
	_, err := (&Planner{}).GeneratePlan(context.Background(), t.TempDir(), dashboard.Ralphex, "scope")
	if err == nil || !strings.Contains(err.Error(), "ralphex planning config directory is required") {
		t.Fatalf("GeneratePlan error = %v, want missing planning config", err)
	}
}

func TestPlannerLoadProfileFromFile(t *testing.T) {
	workspace := t.TempDir()
	profilePath := filepath.Join(t.TempDir(), "planning-profile.json")
	if err := os.WriteFile(profilePath, []byte(`{"planner":"pi","model":"custom","effort":"low","sandbox":"yes"}`), 0o640); err != nil {
		t.Fatal(err)
	}

	var invokedName string
	planner := Planner{
		ProfilePath: profilePath,
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				invokedName = name
				return exec.CommandContext(ctx, "sh", "-c", planCommandScript(validPlan))
			},
		},
	}

	_, err := planner.GeneratePlan(context.Background(), workspace, dashboard.Codex, "file profile test")
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}

	if invokedName != "pi" {
		t.Errorf("invoked = %q, want pi (from profile file)", invokedName)
	}
}

func TestPlannerLoadProfileFileNotFoundFallsBackToDefault(t *testing.T) {
	workspace := t.TempDir()

	var invokedName string
	planner := Planner{
		ProfilePath: filepath.Join(t.TempDir(), "nonexistent-profile.json"),
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				invokedName = name
				return exec.CommandContext(ctx, "sh", "-c", planCommandScript(validPlan))
			},
		},
	}

	_, err := planner.GeneratePlan(context.Background(), workspace, dashboard.Codex, "missing profile test")
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}

	if invokedName != "codex" {
		t.Errorf("invoked = %q, want codex (default when profile file missing)", invokedName)
	}
}

func TestPlannerLoadProfileInvalidJSON(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "bad-profile.json")
	if err := os.WriteFile(profilePath, []byte(`{not valid json}`), 0o640); err != nil {
		t.Fatal(err)
	}

	planner := Planner{
		ProfilePath: profilePath,
	}

	_, err := planner.loadProfile()
	if err == nil {
		t.Fatal("loadProfile = nil, want error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decode planning profile") {
		t.Errorf("error = %v, want decode error", err)
	}
}

func TestPlannerLoadProfileMissingPlannerFieldDefaultsToCodex(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "partial-profile.json")
	if err := os.WriteFile(profilePath, []byte(`{"model":"fast","effort":"low"}`), 0o640); err != nil {
		t.Fatal(err)
	}

	planner := Planner{ProfilePath: profilePath}
	profile, err := planner.loadProfile()
	if err != nil {
		t.Fatalf("loadProfile: %v", err)
	}
	if profile.Planner != "codex" {
		t.Errorf("planner = %q, want codex (default when field missing)", profile.Planner)
	}
}

func TestPlanCommandCodexArgs(t *testing.T) {
	planner := Planner{}
	profile := PlanningProfile{Planner: "codex", Model: "gpt-5", Effort: "high", Sandbox: "yes"}
	name, args := planner.planCommand(profile, "test scope")

	if name != "codex" {
		t.Errorf("name = %q, want codex", name)
	}
	if len(args) < 2 || args[0] != "exec" {
		t.Errorf("args = %v, want [exec, ...]", args)
	}
	prompt := args[len(args)-1]
	if !strings.Contains(strings.Join(args, " "), "--model gpt-5") {
		t.Errorf("args = %v, want configured model flag", args)
	}
	if !strings.Contains(prompt, "test scope") {
		t.Errorf("prompt does not contain scope: %q", prompt)
	}
	if !strings.Contains(prompt, "effort=high") {
		t.Errorf("prompt does not contain effort: %q", prompt)
	}
	if !strings.Contains(prompt, "# Plan:") {
		t.Errorf("prompt does not specify plan format requirements: %q", prompt)
	}
}

func TestPlanCommandPiArgs(t *testing.T) {
	planner := Planner{}
	profile := PlanningProfile{Planner: "pi", Model: "claude", Effort: "low", Sandbox: "no"}
	name, args := planner.planCommand(profile, "pi test")

	if name != "pi" {
		t.Errorf("name = %q, want pi", name)
	}
	if len(args) < 4 || args[0] != "--model" || args[1] != "claude" || args[2] != "-p" {
		t.Errorf("args = %v, want [--model claude -p ...]", args)
	}
	prompt := args[len(args)-1]
	if !strings.Contains(prompt, "pi test") {
		t.Errorf("prompt does not contain scope: %q", prompt)
	}
}
