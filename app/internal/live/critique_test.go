package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// fakeCritiqueModel returns a CritiqueModelFunc that responds with the given
// text. Each call to next increments a counter so tests can verify how many
// model calls were made.
func fakeCritiqueModel(responses ...string) (CritiqueModelFunc, *int) {
	calls := new(int)
	idx := 0
	return func(_ context.Context, _ string, _ string) (string, error) {
		*calls++
		if idx >= len(responses) {
			// Default to "Approved" if we run out of programmed
			// responses.
			return "Approved", nil
		}
		resp := responses[idx]
		idx++
		return resp, nil
	}, calls
}

// testPlanner returns a Planner that writes a valid plan to the workspace
// using a fake ProcessRunner. Each call after the first regenerates with
// a slightly different plan so callers can detect regeneration.
func testPlanner(t *testing.T) *Planner {
	t.Helper()
	genCount := new(int)
	return &Planner{
		ProfilePath: "",
		Runner: &ProcessRunner{
			Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				*genCount++
				// When the Planner is called for regeneration
				// (genCount >= 1 after increment), produce a
				// visibly different plan. Tests that never
				// regenerate never call this Command.
				plan := validPlan
				if *genCount >= 1 {
					plan = regeneratedPlan(*genCount)
				}
				return exec.CommandContext(ctx, "sh", "-c", planCommandScript(plan))
			},
		},
	}
}

// regeneratedPlan returns a valid plan that is observably different from the
// original so tests can confirm regeneration happened.
func regeneratedPlan(gen int) string {
	return fmt.Sprintf(`# Plan: Regenerated plan v%d

### Task 1: Updated setup (gen %d)

This task was regenerated after critique round.

### Task 2: Revised implementation (gen %d)

This task incorporates prior critique findings.`, gen, gen, gen)
}

// writePlan writes content to dashboard.PlanFileName inside workspacePath.
func writePlan(t *testing.T, workspacePath, content string) {
	t.Helper()
	planPath := filepath.Join(workspacePath, dashboard.PlanFileName)
	if err := os.WriteFile(planPath, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestCritiquePlanApproved(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	modelFunc, calls := fakeCritiqueModel("Approved")

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	result, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}
	if !result.Approved {
		t.Error("result.Approved = false, want true")
	}
	if result.Blocked {
		t.Error("result.Blocked = true, want false")
	}
	if result.RegenerationRounds != 0 {
		t.Errorf("result.RegenerationRounds = %d, want 0", result.RegenerationRounds)
	}
	if *calls != 1 {
		t.Errorf("model calls = %d, want 1", *calls)
	}
}

func TestCritiquePlanApprovedWithNoMaterialFindings(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	modelFunc, calls := fakeCritiqueModel("No material findings. The plan looks good.")

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	result, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}
	if !result.Approved {
		t.Error("result.Approved = false, want true")
	}
	if *calls != 1 {
		t.Errorf("model calls = %d, want 1", *calls)
	}
}

func TestCritiquePlanApprovedWithPlanLooksGood(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	modelFunc, calls := fakeCritiqueModel("The plan looks good. All tasks are well-scoped.")

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	result, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}
	if !result.Approved {
		t.Error("result.Approved = false, want true")
	}
	if *calls != 1 {
		t.Errorf("model calls = %d, want 1", *calls)
	}
}

func TestCritiquePlanApprovedWithPlanIsApproved(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	modelFunc, calls := fakeCritiqueModel("Plan is approved. No further changes needed.")

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	result, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}
	if !result.Approved {
		t.Error("result.Approved = false, want true")
	}
	if *calls != 1 {
		t.Errorf("model calls = %d, want 1", *calls)
	}
}

func TestCritiquePlanMaterialFindingsTriggersRegeneration(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	// First call returns findings, second approves the regenerated plan.
	modelFunc, calls := fakeCritiqueModel(
		"Finding: Task 1 has a blind spot around authentication.",
		"Approved",
	)

	planner := testPlanner(t)
	critiquer := Critiquer{
		Planner:   planner,
		ModelFunc: modelFunc,
	}

	result, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}
	if !result.Approved {
		t.Error("result.Approved = false, want true (should be approved after regeneration)")
	}
	if result.RegenerationRounds != 1 {
		t.Errorf("result.RegenerationRounds = %d, want 1", result.RegenerationRounds)
	}
	if *calls != 2 {
		t.Errorf("model calls = %d, want 2 (1 critique + 1 re-critique)", *calls)
	}
	if result.Blocked {
		t.Error("result.Blocked = true, want false")
	}

	// Verify the plan file was regenerated (not the original validPlan).
	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read regenerated plan: %v", err)
	}
	if string(data) == validPlan {
		t.Error("plan file still contains original plan; regeneration did not occur")
	}
	// Verify the plan structure is still valid after regeneration.
	if err := dashboard.ValidatePlan(string(data)); err != nil {
		t.Errorf("regenerated plan is invalid: %v", err)
	}
	// Verify plan was not hand-edited — it should contain the
	// regenerated plan markers, not the findings text.
	if strings.Contains(string(data), "blind spot") {
		t.Error("plan file contains critique findings; it may have been hand-edited instead of regenerated")
	}
}

func TestCritiquePlanDoesNotHandEditPlanFile(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	// Return findings that must NOT appear in the plan file.
	modelFunc, _ := fakeCritiqueModel(
		"Finding: missing error handling in Task 2",
		"Approved",
	)

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	_, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}

	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan file: %v", err)
	}
	content := string(data)

	// The plan file must not contain the critique findings verbatim.
	if strings.Contains(content, "missing error handling in Task 2") {
		t.Error("plan file contains verbatim findings from critique; plan was hand-edited instead of regenerated through Planner")
	}
	// It should still be a structurally valid plan.
	if err := dashboard.ValidatePlan(content); err != nil {
		t.Errorf("plan file is structurally invalid after critique: %v", err)
	}
}

func TestCritiquePlanRoundCapBlocksAfterMaxRounds(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	// Return findings for every round — never approve.
	responses := make([]string, MaxCritiqueRounds)
	for i := range responses {
		responses[i] = "Finding: something is still wrong with the plan."
	}
	modelFunc, calls := fakeCritiqueModel(responses...)

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	result, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}
	if result.Approved {
		t.Error("result.Approved = true, want false (should be blocked)")
	}
	if !result.Blocked {
		t.Error("result.Blocked = false, want true (max rounds exceeded)")
	}
	if result.RegenerationRounds != MaxCritiqueRounds {
		t.Errorf("result.RegenerationRounds = %d, want %d", result.RegenerationRounds, MaxCritiqueRounds)
	}
	if *calls != MaxCritiqueRounds {
		t.Errorf("model calls = %d, want %d", *calls, MaxCritiqueRounds)
	}
	// Findings should accumulate across rounds.
	if result.Findings == "" {
		t.Error("result.Findings is empty; want accumulated findings from all rounds")
	}
	// Verify we have multiple rounds of findings (3 separators for
	// MaxCritiqueRounds = 3 rounds).
	if strings.Count(result.Findings, "previous round findings above") != MaxCritiqueRounds-1 {
		t.Errorf("accumulated findings had %d separators, want %d",
			strings.Count(result.Findings, "previous round findings above"), MaxCritiqueRounds-1)
	}
}

func TestCritiquePlanBlockedWithoutPlanner(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	modelFunc, calls := fakeCritiqueModel("Finding: something is wrong.")

	// No Planner configured — regeneration is impossible.
	critiquer := Critiquer{
		Planner:   nil,
		ModelFunc: modelFunc,
	}

	result, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}
	if result.Approved {
		t.Error("result.Approved = true, want false")
	}
	if !result.Blocked {
		t.Error("result.Blocked = false, want true (no Planner to regenerate)")
	}
	if *calls != 1 {
		t.Errorf("model calls = %d, want 1", *calls)
	}
}

func TestCritiquePlanMissingPlanFile(t *testing.T) {
	workspace := t.TempDir()
	// Do not write a plan file.

	modelFunc, _ := fakeCritiqueModel("Approved")

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	_, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err == nil {
		t.Fatal("CritiquePlan = nil, want error for missing plan file")
	}
	if !strings.Contains(err.Error(), "read plan for critique") {
		t.Errorf("error = %v, want 'read plan for critique'", err)
	}
}

func TestCritiquePlanNilModelFunc(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: nil,
	}

	_, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err == nil {
		t.Fatal("CritiquePlan = nil, want error for nil ModelFunc")
	}
	if !strings.Contains(err.Error(), "critique model function is not configured") {
		t.Errorf("error = %v, want 'critique model function is not configured'", err)
	}
}

func TestCritiquePlanModelError(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	modelFunc := func(_ context.Context, _ string, _ string) (string, error) {
		return "", fmt.Errorf("simulated model failure")
	}

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	_, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err == nil {
		t.Fatal("CritiquePlan = nil, want error from model call")
	}
	if !strings.Contains(err.Error(), "critique model call") {
		t.Errorf("error = %v, want 'critique model call'", err)
	}
}

func TestParseCritiqueResponse(t *testing.T) {
	tests := []struct {
		name     string
		response string
		approved bool
	}{
		{name: "explicit approved", response: "Approved", approved: true},
		{name: "approved with whitespace", response: "  Approved  ", approved: true},
		{name: "approved with details", response: "Approved. The plan is well-structured.", approved: true},
		{name: "no material findings", response: "No material findings to report.", approved: true},
		{name: "plan is approved", response: "Plan is approved. Proceed.", approved: true},
		{name: "plan looks good", response: "Plan looks good. No issues found.", approved: true},
		{name: "findings only", response: "Finding: Task 1 needs more detail.", approved: false},
		{name: "concern raised", response: "I have a concern about the authentication flow.", approved: false},
		{name: "not approved", response: "The plan is not approved because of missing error handling.", approved: false},
		{name: "empty response", response: "", approved: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, approved := parseCritiqueResponse(tt.response)
			if approved != tt.approved {
				t.Errorf("parseCritiqueResponse(%q) approved = %v, want %v", tt.response, approved, tt.approved)
			}
		})
	}
}

func TestJoinFindings(t *testing.T) {
	tests := []struct {
		name       string
		existing   string
		additional string
		want       string
	}{
		{
			name:       "empty existing",
			existing:   "",
			additional: "finding 1",
			want:       "finding 1",
		},
		{
			name:       "append to existing",
			existing:   "finding 1",
			additional: "finding 2",
			want:       "finding 1\n\n--- previous round findings above ---\n\nfinding 2",
		},
		{
			name:       "empty additional",
			existing:   "finding 1",
			additional: "",
			want:       "finding 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := joinFindings(tt.existing, tt.additional)
			if got != tt.want {
				t.Errorf("joinFindings = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildCritiquePrompt(t *testing.T) {
	plan := "# Plan: test plan\n\n### Task 1: do stuff"

	prompt := buildCritiquePrompt(plan, "")
	if !strings.Contains(prompt, plan) {
		t.Errorf("prompt does not contain plan content: %q", prompt)
	}
	if strings.Contains(prompt, "Previous findings") {
		t.Error("prompt with no previous findings should not mention previous findings")
	}

	previous := "finding: missing auth"
	prompt = buildCritiquePrompt(plan, previous)
	if !strings.Contains(prompt, plan) {
		t.Errorf("prompt does not contain plan content: %q", prompt)
	}
	if !strings.Contains(prompt, previous) {
		t.Errorf("prompt does not contain previous findings: %q", prompt)
	}
}

// TestCritiquePlanRegenerationPreservesStructure verifies that after
// regeneration the plan is structurally valid and not just the original
// with findings appended.
func TestCritiquePlanRegenerationPreservesStructure(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	modelFunc, _ := fakeCritiqueModel(
		"Finding: blind spot around error handling",
		"Approved",
	)

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	result, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}
	if !result.Approved {
		t.Fatal("plan should be approved after regeneration")
	}

	planPath := filepath.Join(workspace, dashboard.PlanFileName)
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}

	if err := dashboard.ValidatePlan(string(data)); err != nil {
		t.Errorf("regenerated plan is invalid: %v", err)
	}
}

// TestCritiquePlanAccumulatedFindings verifies that findings from the first
// round are passed to the model on the second round.
func TestCritiquePlanAccumulatedFindings(t *testing.T) {
	workspace := t.TempDir()
	writePlan(t, workspace, validPlan)

	var receivedFindings []string
	modelFunc := func(_ context.Context, planContent string, previousFindings string) (string, error) {
		receivedFindings = append(receivedFindings, previousFindings)
		if len(receivedFindings) == 1 {
			return "Finding: first round issue", nil
		}
		return "Approved", nil
	}

	critiquer := Critiquer{
		Planner:   testPlanner(t),
		ModelFunc: modelFunc,
	}

	_, err := critiquer.CritiquePlan(context.Background(), workspace, dashboard.Codex, "medium")
	if err != nil {
		t.Fatalf("CritiquePlan: %v", err)
	}

	// First call should have empty previousFindings.
	if receivedFindings[0] != "" {
		t.Errorf("first call previousFindings = %q, want empty", receivedFindings[0])
	}
	// Second call should contain findings from the first round.
	if !strings.Contains(receivedFindings[1], "first round issue") {
		t.Errorf("second call previousFindings = %q, want to contain 'first round issue'", receivedFindings[1])
	}
}
