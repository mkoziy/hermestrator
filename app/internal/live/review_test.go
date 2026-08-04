package live

import (
	"context"
	"fmt"
	"os"
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

func TestGHReviewerPostFindingsIsIdempotentPerRound(t *testing.T) {
	commentFile := t.TempDir() + "/comment"
	posted := 0
	command := func(_ context.Context, name string, args ...string) *exec.Cmd {
		if args[0] == "pr" && args[1] == "view" {
			body := "{}"
			if data, err := os.ReadFile(commentFile); err == nil {
				body = fmt.Sprintf(`{"comments":[{"body":%q}]}`, string(data))
			}
			return exec.Command("sh", "-c", "printf '%s' '"+body+"'")
		}
		posted++
		return exec.Command("sh", "-c", "cat > "+commentFile)
	}
	r := GHReviewer{Command: command}
	repo := dashboard.Repository{FullName: "acme/repo"}
	pr := dashboard.PullRequest{Number: 4}
	if err := r.PostFindings(context.Background(), repo, pr, "fix it", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.PostFindings(context.Background(), repo, pr, "fix it", 1); err != nil {
		t.Fatal(err)
	}
	if posted != 1 {
		t.Fatalf("posted=%d, want one comment", posted)
	}
	content, err := os.ReadFile(commentFile)
	if err != nil || !strings.Contains(string(content), "hermestrator-review:1") {
		t.Fatalf("comment=%q err=%v", content, err)
	}
}

func TestGHReviewerPostFindingsRoundMarkerIncrements(t *testing.T) {
	markers := []string{}
	command := func(_ context.Context, name string, args ...string) *exec.Cmd {
		if args[1] == "view" {
			return exec.Command("sh", "-c", "printf '%s' '{}'")
		}
		return exec.Command("sh", "-c", "cat")
	}
	// A distinct round must be eligible for publication even after an earlier
	// round was posted; the command fake records the body through its stdin.
	r := GHReviewer{Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := command(ctx, name, args...)
		if args[1] == "comment" {
			markers = append(markers, fmt.Sprint(args))
		}
		return cmd
	}}
	for round := 1; round <= 2; round++ {
		if err := r.PostFindings(context.Background(), dashboard.Repository{FullName: "acme/repo"}, dashboard.PullRequest{Number: 4}, "finding", round); err != nil {
			t.Fatal(err)
		}
	}
	if len(markers) != 2 {
		t.Fatalf("comment posts=%d, want two", len(markers))
	}
}

func TestGHReviewerPostFindingsRetriesAfterTimedOutMutation(t *testing.T) {
	posted := false
	queries := 0
	command := func(_ context.Context, name string, args ...string) *exec.Cmd {
		if args[1] == "view" {
			queries++
			if posted {
				return exec.Command("sh", "-c", "printf '%s' '{\"comments\":[{\"body\":\"<!-- hermestrator-review:1 -->\"}]}'")
			}
			return exec.Command("sh", "-c", "printf '%s' '{}'")
		}
		posted = true
		return exec.Command("sh", "-c", "exit 1")
	}
	r := GHReviewer{Command: command}
	if err := r.PostFindings(context.Background(), dashboard.Repository{FullName: "acme/repo"}, dashboard.PullRequest{Number: 4}, "finding", 1); err == nil {
		t.Fatal("first post should report the simulated timeout")
	}
	if err := r.PostFindings(context.Background(), dashboard.Repository{FullName: "acme/repo"}, dashboard.PullRequest{Number: 4}, "finding", 1); err != nil {
		t.Fatal(err)
	}
	if queries != 2 {
		t.Fatalf("queries=%d, want two", queries)
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

func TestGHReviewerRedactsReviewInputAndFindings(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	var prompts []string
	command := func(_ context.Context, name string, args ...string) *exec.Cmd {
		if len(args) >= 2 && args[0] == "pr" {
			return exec.Command("sh", "-c", "printf 'diff "+secret+"'")
		}
		return exec.Command("sh", "-c", "printf '{\"title\":\"issue\",\"body\":\"criteria "+secret+"\"}'")
	}
	model := func(_ context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "finding " + secret, nil
	}
	r := GHReviewer{Command: command, StandardsModel: model, SpecModel: model}
	result, err := r.Review(context.Background(), dashboard.Repository{FullName: "acme/repo"}, dashboard.PullRequest{Number: 4}, dashboard.IntakeStatus{PublishedIssue: dashboard.PublishedIssue{Number: 9}})
	if err != nil || strings.Contains(result.Findings, secret) || !strings.Contains(result.Findings, "[redacted]") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, prompt := range prompts {
		if strings.Contains(prompt, secret) {
			t.Fatalf("model prompt leaked secret: %q", prompt)
		}
	}
}
