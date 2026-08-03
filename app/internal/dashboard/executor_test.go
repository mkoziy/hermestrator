package dashboard

import (
	"errors"
	"strings"
	"testing"
)

func TestSelectExecutor(t *testing.T) {
	tests := []struct {
		name                string
		scope               string
		repoPolicy          ExecutorKind
		priorFailures       []FailureRecord
		wantKind            ExecutorKind
		wantRationaleSubstr string
	}{
		// Success cases: scope-based selection
		{
			name:                "simple scope selects Pi",
			scope:               "simple",
			wantKind:            Pi,
			wantRationaleSubstr: "simple scope",
		},
		{
			name:                "medium scope selects Codex",
			scope:               "medium",
			wantKind:            Codex,
			wantRationaleSubstr: "medium scope",
		},
		{
			name:                "complex scope selects Ralphex",
			scope:               "complex",
			wantKind:            Ralphex,
			wantRationaleSubstr: "complex scope",
		},

		// Success cases: repo policy overrides scope
		{
			name:                "repo policy Pi overrides complex scope",
			scope:               "complex",
			repoPolicy:          Pi,
			wantKind:            Pi,
			wantRationaleSubstr: "repository policy prefers",
		},
		{
			name:                "repo policy Codex overrides simple scope",
			scope:               "simple",
			repoPolicy:          Codex,
			wantKind:            Codex,
			wantRationaleSubstr: "repository policy prefers",
		},

		// Success cases: prior failure fallback
		{
			name:  "Ralphex failure falls back to Codex",
			scope: "complex",
			priorFailures: []FailureRecord{
				{Kind: Ralphex, Reason: "timeout"},
			},
			wantKind:            Codex,
			wantRationaleSubstr: "prior failures; falling back",
		},
		{
			name:  "Ralphex + Codex failure falls back to Pi",
			scope: "complex",
			priorFailures: []FailureRecord{
				{Kind: Ralphex, Reason: "timeout"},
				{Kind: Codex, Reason: "crash"},
			},
			wantKind:            Pi,
			wantRationaleSubstr: "prior failures; falling back",
		},

		// Edge cases: ambiguous/conflicting inputs
		{
			name:                "empty scope with no policy falls back to VerificationOnly",
			scope:               "",
			wantKind:            VerificationOnly,
			wantRationaleSubstr: "ambiguous scope",
		},
		{
			name:                "unknown scope with no policy falls back to VerificationOnly",
			scope:               "gargantuan",
			wantKind:            VerificationOnly,
			wantRationaleSubstr: "ambiguous scope",
		},
		{
			name:                "unrecognised repo policy falls back to VerificationOnly",
			scope:               "simple",
			repoPolicy:          ExecutorKind("unknown-executor"),
			wantKind:            VerificationOnly,
			wantRationaleSubstr: "unrecognised executor",
		},

		// Edge case: all executors have failed
		{
			name:  "all executors failed falls back to VerificationOnly",
			scope: "complex",
			priorFailures: []FailureRecord{
				{Kind: Ralphex, Reason: "timeout"},
				{Kind: Codex, Reason: "crash"},
				{Kind: Pi, Reason: "out of memory"},
			},
			wantKind:            VerificationOnly,
			wantRationaleSubstr: "all executors have prior failures",
		},

		// Edge case: repo policy set to VerificationOnly explicitly
		{
			name:                "explicit VerificationOnly policy",
			scope:               "complex",
			repoPolicy:          VerificationOnly,
			wantKind:            VerificationOnly,
			wantRationaleSubstr: "repository policy prefers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SelectExecutor(tt.scope, tt.repoPolicy, tt.priorFailures)
			if result.Kind != tt.wantKind {
				t.Errorf("SelectExecutor() kind = %q, want %q", result.Kind, tt.wantKind)
			}
			if tt.wantRationaleSubstr != "" && !strings.Contains(result.Rationale, tt.wantRationaleSubstr) {
				t.Errorf("SelectExecutor() rationale = %q, want substring %q", result.Rationale, tt.wantRationaleSubstr)
			}
		})
	}
}

func TestSelectExecutor_PolicyWithPriorFailures(t *testing.T) {
	// When repo policy is Codex but Codex has failed, fall back to Pi
	// (the next in the chain).
	result := SelectExecutor("simple", Codex, []FailureRecord{
		{Kind: Codex, Reason: "crash"},
	})
	if result.Kind != Pi {
		t.Errorf("expected Pi after Codex failure, got %q", result.Kind)
	}
}

func TestSelectExecutor_PolicyVerificationOnlyWithNoFailures(t *testing.T) {
	result := SelectExecutor("complex", VerificationOnly, nil)
	if result.Kind != VerificationOnly {
		t.Errorf("expected VerificationOnly, got %q", result.Kind)
	}
}

func TestSelectExecutor_NilPriorFailures(t *testing.T) {
	result := SelectExecutor("medium", "", nil)
	if result.Kind != Codex {
		t.Errorf("expected Codex for medium scope, got %q", result.Kind)
	}
}

func TestValidatePlan_ValidPlan(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "minimal valid plan",
			content: `# Plan: Example plan

### Task 1: Do the thing

Some description.
`,
		},
		{
			name: "plan with multiple tasks",
			content: `# Plan: Multi-task plan

### Task 1: First

### Task 2: Second

### Task 3: Third
`,
		},
		{
			name: "title line with extra whitespace",
			content: `  # Plan: Something

### Task 1: Do it
`,
		},
		{
			name: "task section indented",
			content: `# Plan: Example

  ### Task 5: Indented task
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidatePlan(tt.content); err != nil {
				t.Errorf("ValidatePlan = %v, want nil", err)
			}
		})
	}
}

func TestValidatePlan_InvalidPlan(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "empty content",
			content: "",
		},
		{
			name:    "no title line",
			content: "### Task 1: Do something\n\nDescription.\n",
		},
		{
			name:    "no task sections",
			content: "# Plan: My plan\n\nJust a description, no tasks.\n",
		},
		{
			name:    "wrong title format",
			content: "## Plan: Wrong level\n\n### Task 1: Do it\n",
		},
		{
			name:    "wrong task format",
			content: "# Plan: My plan\n\n### Task: Missing number\n",
		},
		{
			name:    "bold instead of heading",
			content: "**Plan:** Not a heading\n\n**Task 1:** Not a heading either\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePlan(tt.content)
			if err == nil {
				t.Fatal("ValidatePlan = nil, want error")
			}
			if !errors.Is(err, ErrPlanInvalidStructure) {
				t.Errorf("error = %v, want ErrPlanInvalidStructure", err)
			}
		})
	}
}
