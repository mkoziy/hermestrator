package live

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestGHReviewerBlocksOversizedDiffBeforeModels(t *testing.T) {
	called := false
	command := func(_ context.Context, name string, args ...string) *exec.Cmd {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "diff" {
			return exec.Command("sh", "-c", "printf oversized")
		}
		return exec.Command("sh", "-c", "printf '{}'")
	}
	r := GHReviewer{Command: command, MaxDiffBytes: 3, StandardsModel: func(context.Context, string) (string, error) { called = true; return "", nil }, SpecModel: func(context.Context, string) (string, error) { called = true; return "", nil }}
	result, err := r.Review(context.Background(), dashboard.Repository{FullName: "acme/repo"}, dashboard.PullRequest{Number: 4}, dashboard.IntakeStatus{PublishedIssue: dashboard.PublishedIssue{Number: 9}})
	if err != nil || !result.Blocked || called || !strings.Contains(result.Findings, "above") {
		t.Fatalf("result=%+v err=%v called=%v", result, err, called)
	}
}

func TestGHReviewerCombinesAxisFindings(t *testing.T) {
	command := func(_ context.Context, name string, args ...string) *exec.Cmd {
		if len(args) >= 2 && args[0] == "pr" {
			return exec.Command("sh", "-c", "printf 'small diff'")
		}
		return exec.Command("sh", "-c", "printf '{\"title\":\"issue\",\"body\":\"criteria\"}'")
	}
	r := GHReviewer{Command: command, StandardsModel: func(context.Context, string) (string, error) { return "approved", nil }, SpecModel: func(context.Context, string) (string, error) { return "missing acceptance criterion", nil }}
	result, err := r.Review(context.Background(), dashboard.Repository{FullName: "acme/repo"}, dashboard.PullRequest{Number: 4}, dashboard.IntakeStatus{PublishedIssue: dashboard.PublishedIssue{Number: 9}})
	if err != nil || result.Approved || !strings.Contains(result.Findings, "Spec findings") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
