package dashboard

import "fmt"

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
