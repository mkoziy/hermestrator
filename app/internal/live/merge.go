package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

type GHMergeExecutor struct {
	Command func(context.Context, string, ...string) *exec.Cmd
}
type mergeState struct {
	State    string `json:"state"`
	MergedAt string `json:"mergedAt"`
}
type issueState struct {
	State string `json:"state"`
}

func (e GHMergeExecutor) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if e.Command != nil {
		return e.Command(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

func (e GHMergeExecutor) Merge(ctx context.Context, repo dashboard.Repository, n int) error {
	if repo.FullName == "" || n < 1 {
		return fmt.Errorf("repository and pull request are required")
	}
	out, err := e.command(ctx, "gh", "pr", "view", fmt.Sprint(n), "--repo", repo.FullName, "--json", "state,mergedAt").Output()
	if err != nil {
		return fmt.Errorf("query pull request before merge: %w", err)
	}
	var state mergeState
	if err := json.Unmarshal(out, &state); err != nil {
		return fmt.Errorf("decode pull request state: %w", err)
	}
	if strings.EqualFold(state.State, "MERGED") || state.MergedAt != "" {
		return nil
	}
	if output, err := e.command(ctx, "gh", "pr", "merge", fmt.Sprint(n), "--repo", repo.FullName, "--merge").CombinedOutput(); err != nil {
		return fmt.Errorf("merge pull request: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (e GHMergeExecutor) ConfirmMerged(ctx context.Context, repo dashboard.Repository, n int) error {
	if repo.FullName == "" || n < 1 {
		return fmt.Errorf("repository and pull request are required")
	}
	out, err := e.command(ctx, "gh", "pr", "view", fmt.Sprint(n), "--repo", repo.FullName, "--json", "state,mergedAt").Output()
	if err != nil {
		return fmt.Errorf("confirm pull request merge: %w", err)
	}
	var state mergeState
	if err := json.Unmarshal(out, &state); err != nil {
		return fmt.Errorf("decode pull request state: %w", err)
	}
	if !strings.EqualFold(state.State, "MERGED") && state.MergedAt == "" {
		return fmt.Errorf("pull request is not merged")
	}
	return nil
}

func (e GHMergeExecutor) CloseIssue(ctx context.Context, repo dashboard.Repository, n int) error {
	if repo.FullName == "" || n < 1 {
		return fmt.Errorf("repository and issue are required")
	}
	out, err := e.command(ctx, "gh", "issue", "view", fmt.Sprint(n), "--repo", repo.FullName, "--json", "state").Output()
	if err != nil {
		return fmt.Errorf("query issue before close: %w", err)
	}
	var state issueState
	if err := json.Unmarshal(out, &state); err != nil {
		return fmt.Errorf("decode issue state: %w", err)
	}
	if strings.EqualFold(state.State, "CLOSED") {
		return nil
	}
	if output, err := e.command(ctx, "gh", "issue", "close", fmt.Sprint(n), "--repo", repo.FullName).CombinedOutput(); err != nil {
		return fmt.Errorf("close issue: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

var _ dashboard.MergeExecutor = GHMergeExecutor{}
