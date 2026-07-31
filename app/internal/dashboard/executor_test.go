package dashboard

import (
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
