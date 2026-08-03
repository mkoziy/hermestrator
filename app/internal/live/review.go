package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/mkoziy/hermestrator/internal/dashboard"
	"github.com/openai/openai-go"
)

const MaxReviewDiffBytes = 200000

type ReviewModelFunc func(context.Context, string) (string, error)

// NewReviewModelFunc adapts a configured Genkit model to one review axis.
func NewReviewModelFunc(g *genkit.Genkit, modelName string) ReviewModelFunc {
	return func(ctx context.Context, prompt string) (string, error) {
		for result, err := range genkit.GenerateStream(ctx, g,
			ai.WithModelName(modelName),
			ai.WithMessages(ai.NewUserTextMessage(prompt)),
			ai.WithConfig(&openai.ChatCompletionNewParams{MaxCompletionTokens: openai.Int(2048)}),
		) {
			if err != nil {
				return "", fmt.Errorf("review model stream: %w", err)
			}
			if result.Done {
				text := strings.TrimSpace(result.Response.Message.Text())
				if text == "" {
					return "", fmt.Errorf("review model returned empty response")
				}
				return text, nil
			}
		}
		return "", fmt.Errorf("review model ended without a response")
	}
}

type GHReviewer struct {
	Command        func(context.Context, string, ...string) *exec.Cmd
	StandardsModel ReviewModelFunc
	SpecModel      ReviewModelFunc
	MaxDiffBytes   int
}

// ReviewCommenter publishes review findings without duplicating comments when
// a request is retried after the GitHub mutation may already have succeeded.
type ReviewCommenter interface {
	PostFindings(context.Context, dashboard.Repository, dashboard.PullRequest, string, int) error
}

const reviewCommentMarker = "<!-- hermestrator-review:%d -->"

func (r GHReviewer) PostFindings(ctx context.Context, repo dashboard.Repository, pr dashboard.PullRequest, findings string, round int) error {
	command := r.Command
	if command == nil {
		command = exec.CommandContext
	}
	marker := fmt.Sprintf(reviewCommentMarker, round)
	comments, err := command(ctx, "gh", "pr", "view", fmt.Sprint(pr.Number), "--repo", repo.FullName, "--json", "comments").Output()
	if err != nil {
		return fmt.Errorf("read pull request comments: %w", err)
	}
	var data struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(comments, &data); err != nil {
		return fmt.Errorf("decode pull request comments: %w", err)
	}
	for _, comment := range data.Comments {
		if strings.Contains(comment.Body, marker) {
			return nil
		}
	}

	body := marker + "\n" + strings.TrimSpace(findings)
	cmd := command(ctx, "gh", "pr", "comment", fmt.Sprint(pr.Number), "--repo", repo.FullName, "--body-file", "-")
	cmd.Stdin = strings.NewReader(body)
	if _, err := cmd.Output(); err != nil {
		return fmt.Errorf("post pull request review findings: %w", err)
	}
	return nil
}

var _ ReviewCommenter = GHReviewer{}

var _ dashboard.Reviewer = GHReviewer{}

func (r GHReviewer) Review(ctx context.Context, repo dashboard.Repository, pr dashboard.PullRequest, status dashboard.IntakeStatus) (dashboard.ReviewResult, error) {
	command := r.Command
	if command == nil {
		command = exec.CommandContext
	}
	diff, err := command(ctx, "gh", "pr", "diff", fmt.Sprint(pr.Number), "--repo", repo.FullName).Output()
	if err != nil {
		return dashboard.ReviewResult{}, fmt.Errorf("read pull request diff: %w", err)
	}
	limit := r.MaxDiffBytes
	if limit <= 0 {
		limit = MaxReviewDiffBytes
	}
	if len(diff) > limit {
		return dashboard.ReviewResult{Blocked: true, Findings: fmt.Sprintf("Automated review blocked: pull request diff is %d bytes, above the %d-byte limit.", len(diff), limit)}, nil
	}
	issue, err := command(ctx, "gh", "issue", "view", fmt.Sprint(status.PublishedIssue.Number), "--repo", repo.FullName, "--json", "title,body").Output()
	if err != nil {
		return dashboard.ReviewResult{}, fmt.Errorf("read issue acceptance criteria: %w", err)
	}
	var issueData struct{ Title, Body string }
	if err := json.Unmarshal(issue, &issueData); err != nil {
		return dashboard.ReviewResult{}, fmt.Errorf("decode issue: %w", err)
	}
	input := fmt.Sprintf("Repository: %s\nIssue: %s\n%s\nVerification output:\n%s\nFull diff:\n%s", repo.FullName, issueData.Title, issueData.Body, status.VerificationOutput, diff)
	if r.StandardsModel == nil || r.SpecModel == nil {
		return dashboard.ReviewResult{}, fmt.Errorf("review models are not configured")
	}
	standards, err := r.StandardsModel(ctx, "Review this full PR diff for standards compliance. Report only material findings, or respond APPROVED.\n\n"+input)
	if err != nil {
		return dashboard.ReviewResult{}, fmt.Errorf("standards review: %w", err)
	}
	spec, err := r.SpecModel(ctx, "Review this full PR diff against the issue acceptance criteria. Report only material findings, or respond APPROVED.\n\n"+input)
	if err != nil {
		return dashboard.ReviewResult{}, fmt.Errorf("spec review: %w", err)
	}
	findings := []string{}
	if !approved(standards) {
		findings = append(findings, "Standards findings:\n"+strings.TrimSpace(standards))
	}
	if !approved(spec) {
		findings = append(findings, "Spec findings:\n"+strings.TrimSpace(spec))
	}
	return dashboard.ReviewResult{Approved: len(findings) == 0, Findings: strings.Join(findings, "\n\n")}, nil
}

func approved(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "" || strings.HasPrefix(s, "approved") || strings.Contains(s, "no material findings")
}
