package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

var pullRequestURL = regexp.MustCompile(`/pull/([0-9]+)$`)

// GHPRCreator pushes the issue branch and creates (or reuses) its pull request.
type GHPRCreator struct {
	Command func(context.Context, string, ...string) *exec.Cmd
}

var _ dashboard.PRCreator = GHPRCreator{}

func (p GHPRCreator) CreateOrReuse(ctx context.Context, repo dashboard.Repository, status dashboard.IntakeStatus) (dashboard.PullRequest, error) {
	if repo.FullName == "" || status.ExecutorWorkspacePath == "" {
		return dashboard.PullRequest{}, fmt.Errorf("repository and executor workspace are required")
	}
	command := p.Command
	if command == nil {
		command = exec.CommandContext
	}
	branchOutput, err := command(ctx, "git", "-C", status.ExecutorWorkspacePath, "branch", "--show-current").Output()
	if err != nil {
		return dashboard.PullRequest{}, fmt.Errorf("inspect workspace branch: %w", err)
	}
	branch := strings.TrimSpace(string(branchOutput))
	if branch == "" {
		return dashboard.PullRequest{}, fmt.Errorf("workspace branch is empty")
	}
	view := command(ctx, "gh", "pr", "list", "--repo", repo.FullName, "--head", branch, "--json", "number,url,state", "--limit", "1")
	output, err := view.Output()
	if err != nil {
		return dashboard.PullRequest{}, fmt.Errorf("find pull request: %w", err)
	}
	var prs []dashboard.PullRequest
	if err := json.Unmarshal(output, &prs); err != nil {
		return dashboard.PullRequest{}, fmt.Errorf("decode pull request list: %w", err)
	}
	if len(prs) > 0 {
		return prs[0], nil
	}
	if output, err := command(ctx, "git", "-C", status.ExecutorWorkspacePath, "push", "-u", "origin", branch).CombinedOutput(); err != nil {
		return dashboard.PullRequest{}, fmt.Errorf("push workspace branch: %w: %s", err, strings.TrimSpace(string(output)))
	}
	body := fmt.Sprintf("Implement GitHub issue #%d.\n\n<!-- hermestrator-pr:%d -->", status.PublishedIssue.Number, status.PublishedIssue.Number)
	created, err := command(ctx, "gh", "pr", "create", "--repo", repo.FullName, "--head", branch, "--title", fmt.Sprintf("Implement issue #%d", status.PublishedIssue.Number), "--body", body).Output()
	if err != nil {
		return dashboard.PullRequest{}, fmt.Errorf("create pull request: %w", err)
	}
	url := strings.TrimSpace(string(created))
	match := pullRequestURL.FindStringSubmatch(url)
	if len(match) != 2 {
		return dashboard.PullRequest{}, fmt.Errorf("parse created pull request URL %q", url)
	}
	var number int
	if _, err := fmt.Sscan(match[1], &number); err != nil {
		return dashboard.PullRequest{}, fmt.Errorf("parse pull request number: %w", err)
	}
	return dashboard.PullRequest{Number: number, URL: url, State: "open"}, nil
}
