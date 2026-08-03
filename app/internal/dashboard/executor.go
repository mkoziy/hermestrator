package dashboard

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ExecutorKind identifies which executor drives implementation for a ticket.
type ExecutorKind string

const (
	Ralphex          ExecutorKind = "ralphex"
	Codex            ExecutorKind = "codex"
	Pi               ExecutorKind = "pi"
	VerificationOnly ExecutorKind = "verification-only"
)

// ExecutorSelection records the chosen executor and the reason it was picked.
type ExecutorSelection struct {
	Kind      ExecutorKind
	Rationale string
}

// FailureRecord captures a prior failed executor run so SelectExecutor can
// avoid re-selecting an executor that has already failed for this ticket.
type FailureRecord struct {
	Kind   ExecutorKind
	Reason string
}

// SelectExecutor picks an executor deterministically from ticket scope,
// repository policy, and prior failure history. It never calls an LLM.
func SelectExecutor(scope string, repoPolicy ExecutorKind, priorFailures []FailureRecord) ExecutorSelection {
	// Build a set of executors that have already failed.
	failed := make(map[ExecutorKind]bool, len(priorFailures))
	for _, f := range priorFailures {
		failed[f.Kind] = true
	}

	// The fallback chain in priority order from most ambitious to safest.
	// VerificationOnly sits outside the chain as the terminal fallback.
	chain := []ExecutorKind{Ralphex, Codex, Pi}

	// Determine the starting candidate.
	candidate := executorForScope(scope)
	if repoPolicy != "" {
		candidate = repoPolicy
	}

	// If the starting candidate is unrecognised, fall back to VerificationOnly.
	if !validExecutor(candidate) {
		return ExecutorSelection{
			Kind:      VerificationOnly,
			Rationale: fmt.Sprintf("unrecognised executor %q; falling back to verification-only", candidate),
		}
	}

	// VerificationOnly as an explicit policy is accepted directly.
	if candidate == VerificationOnly {
		return ExecutorSelection{
			Kind:      VerificationOnly,
			Rationale: rationaleFor(candidate, scope, repoPolicy),
		}
	}

	// Walk the chain starting from the candidate; skip past any that have
	// already failed. If we exhaust the chain, fall back to
	// VerificationOnly.
	startIdx := indexInChain(candidate, chain)
	for i := startIdx; i < len(chain); i++ {
		if !failed[chain[i]] {
			if chain[i] == candidate {
				return ExecutorSelection{
					Kind:      candidate,
					Rationale: rationaleFor(candidate, scope, repoPolicy),
				}
			}
			return ExecutorSelection{
				Kind:      chain[i],
				Rationale: fmt.Sprintf("%s has prior failures; falling back to %s", candidate, chain[i]),
			}
		}
	}

	// Every real executor in the chain has failed.
	return ExecutorSelection{
		Kind:      VerificationOnly,
		Rationale: "all executors have prior failures; falling back to verification-only",
	}
}

func executorForScope(scope string) ExecutorKind {
	switch scope {
	case "simple":
		return Pi
	case "medium":
		return Codex
	case "complex":
		return Ralphex
	default:
		return VerificationOnly
	}
}

func validExecutor(k ExecutorKind) bool {
	switch k {
	case Ralphex, Codex, Pi, VerificationOnly:
		return true
	}
	return false
}

func indexInChain(k ExecutorKind, chain []ExecutorKind) int {
	for i, c := range chain {
		if c == k {
			return i
		}
	}
	// Unknown kinds start at the end of the chain (VerificationOnly).
	return len(chain) - 1
}

// PlanFileName is the standard plan file path relative to the issue
// workspace root. Tasks 6, 7, and 8 reference this instead of
// redefining it.
const PlanFileName = "plan.md"

// ErrPlanInvalidStructure is returned when a generated plan does not
// satisfy the required structural markers.
var ErrPlanInvalidStructure = errors.New("generated plan does not satisfy the required structure")

var planTitleRE = regexp.MustCompile(`^# Plan:`)
var planTaskRE = regexp.MustCompile(`^### Task \d+:`)

// ValidatePlan checks that content satisfies the minimal structural markers
// a valid generated plan must have: a "# Plan:" title line and one or more
// "### Task N:" sections.
func ValidatePlan(content string) error {
	lines := strings.Split(content, "\n")
	hasTitle := false
	hasTask := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if planTitleRE.MatchString(trimmed) {
			hasTitle = true
		}
		if planTaskRE.MatchString(trimmed) {
			hasTask = true
		}
		if hasTitle && hasTask {
			return nil
		}
	}
	return fmt.Errorf("%w: hasTitle=%v hasTask=%v", ErrPlanInvalidStructure, hasTitle, hasTask)
}

// IssueClone creates an isolated clone for implementation work, keyed by
// issue number. Implementations must never share the intake clone
// directory.
type IssueClone interface {
	Start(context.Context, Repository, int) (string, error)
	Cleanup(context.Context, string) error
}

// RecoveryState classifies an in-flight issue workspace after a restart.
type RecoveryState string

const (
	RecoveryNoChanges          RecoveryState = "no-changes"
	RecoveryUncommittedChanges RecoveryState = "uncommitted-changes"
	RecoveryLocalCommits       RecoveryState = "local-commits"
	RecoveryRemoteBranchExists RecoveryState = "remote-branch-exists"
	RecoveryExecutorFailed     RecoveryState = "executor-failed"
)

// WorkspaceClassifier inspects a workspace directory and returns its
// recovery state. The processFailed flag, when true, indicates the last
// known executor process terminated abnormally.
type WorkspaceClassifier interface {
	Classify(ctx context.Context, workspacePath string, processFailed bool) (RecoveryState, error)
}

func rationaleFor(k ExecutorKind, scope string, repoPolicy ExecutorKind) string {
	if repoPolicy != "" {
		return fmt.Sprintf("repository policy prefers %s", k)
	}
	switch scope {
	case "simple", "medium", "complex":
		return fmt.Sprintf("selected %s for %s scope", k, scope)
	default:
		return fmt.Sprintf("falling back to %s (ambiguous scope %q)", k, scope)
	}
}

// Planner generates a plan file by invoking codex or pi directly.
type Planner interface {
	GeneratePlan(ctx context.Context, workspacePath string, executorKind ExecutorKind, scope string) (string, error)
}

// Critiquer evaluates a generated plan and triggers regeneration on material findings.
type Critiquer interface {
	CritiquePlan(ctx context.Context, workspacePath string, executorKind ExecutorKind, scope string) (*CritiqueResult, error)
}

// Preflight runs pre-critique checks on an issue workspace.
type Preflight interface {
	Verify(ctx context.Context, workspacePath string, expectedRemote string) PreflightResult
}

// PreflightResult captures the outcome of pre-critique verification.
type PreflightResult struct {
	Passed   bool
	Failures []PreflightFailure
}

// PreflightFailure describes a single pre-flight check failure.
type PreflightFailure struct {
	Check  string
	Detail string
}

// VerificationRunner executes canonical checks before PR creation.
type VerificationRunner interface {
	Run(ctx context.Context, workspacePath string, checks []CheckSpec) (VerificationResult, error)
}

// CheckSpec describes a single verification check.
type CheckSpec struct {
	Name    string
	Command string
	Args    []string
}

// VerificationResult aggregates verification check results.
type VerificationResult struct {
	ReadyForPR bool
	Checks     []CheckResult
}

// CheckResult captures the outcome of a single verification check.
type CheckResult struct {
	Name     string
	ExitCode int
	Passed   bool
	Output   string
}

// CritiqueResult captures the outcome of plan critique.
type CritiqueResult struct {
	Approved           bool
	Findings           string
	RegenerationRounds int
	Blocked            bool
}
