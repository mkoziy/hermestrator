package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// PreflightFailure describes a single pre-critique check failure.
type PreflightFailure struct {
	Check  string // short name of the check that failed
	Detail string // human-readable failure detail
}

// PreflightResult captures the outcome of pre-critique verification.
// Passed is true only when every check succeeded. When passed is false,
// Failures contains every check that did not succeed (the caller can
// aggregate and surface them all rather than stopping at the first
// failure).
type PreflightResult struct {
	Passed   bool
	Failures []PreflightFailure
}

// AddFailure is a convenience for collecting failures during verification.
func (r *PreflightResult) AddFailure(check, detail string) {
	r.Failures = append(r.Failures, PreflightFailure{Check: check, Detail: detail})
}

// Preflight runs pre-critique checks on an issue workspace before the
// critique gate is entered. It uses the same injectable-Command convention
// as ProcessRunner and IssueWorkspace so tests can supply coreutils.
//
// LookPath resolves executables in the test's PATH; when nil,
// exec.LookPath is used.
type Preflight struct {
	// Command returns a prepared *exec.Cmd for git queries.
	Command func(context.Context, string, ...string) *exec.Cmd
	// LookPath resolves a tool name to an absolute path.
	LookPath func(string) (string, error)
}

// requiredTools is the list of executor and infrastructure binaries that
// must be resolvable in PATH before the critique gate.
var requiredTools = []string{"ralphex", "codex", "pi", "gh"}

// Verify runs all pre-flight checks against the given workspace. It
// collects every failure so the caller can surface them all at once.
//
// Parameters:
//   - workspacePath: absolute path to the issue clone (must exist).
//   - expectedRemote: the GitHub full name (owner/repo) the clone's origin
//     remote must point to.
func (p *Preflight) Verify(ctx context.Context, workspacePath string, expectedRemote string) PreflightResult {
	var result PreflightResult

	// 1. Workspace directory must exist.
	if info, err := os.Stat(workspacePath); err != nil {
		result.AddFailure("workspace-exists", fmt.Sprintf("workspace directory %q: %v", workspacePath, err))
	} else if !info.IsDir() {
		result.AddFailure("workspace-exists", fmt.Sprintf("workspace path %q is not a directory", workspacePath))
	}

	// 2. Plan file must exist and have valid structure.
	planPath := filepath.Join(workspacePath, dashboard.PlanFileName)
	planContent, err := os.ReadFile(planPath)
	if err != nil {
		result.AddFailure("plan-file-exists", fmt.Sprintf("plan file %q: %v", planPath, err))
	} else {
		if err := dashboard.ValidatePlan(string(planContent)); err != nil {
			result.AddFailure("plan-structure", err.Error())
		}
	}

	// 3. Required tools must be resolvable in PATH.
	lookPath := p.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	for _, tool := range requiredTools {
		if _, err := lookPath(tool); err != nil {
			result.AddFailure("tool-available", fmt.Sprintf("required tool %q not found in PATH: %v", tool, err))
		}
	}

	// 4. Remote git state must match expectations. We query 'git remote
	//    get-url origin' inside the workspace and verify the URL references
	//    the expected GitHub repository.
	if expectedRemote != "" {
		actualRemote, err := p.gitRemoteURL(ctx, workspacePath)
		if err != nil {
			result.AddFailure("git-remote", fmt.Sprintf("cannot determine origin remote: %v", err))
		} else if !remoteMatches(actualRemote, expectedRemote) {
			result.AddFailure("git-remote", fmt.Sprintf("origin remote %q does not match expected repository %s", actualRemote, expectedRemote))
		}
	}

	result.Passed = len(result.Failures) == 0
	return result
}

// gitRemoteURL runs "git remote get-url origin" inside workspacePath and
// returns the trimmed output.
func (p *Preflight) gitRemoteURL(ctx context.Context, workspacePath string) (string, error) {
	command := p.Command
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = workspacePath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// remoteMatches reports whether urlStr references the GitHub repository
// identified by fullName (owner/repo).
func remoteMatches(urlStr, fullName string) bool {
	// Accept https://github.com/owner/repo, git@github.com:owner/repo,
	// or any URL whose path ends with /owner/repo or :owner/repo.
	return strings.HasSuffix(urlStr, "/"+fullName) ||
		strings.HasSuffix(urlStr, ":"+fullName) ||
		strings.HasSuffix(urlStr, "/"+fullName+".git") ||
		strings.HasSuffix(urlStr, ":"+fullName+".git")
}
