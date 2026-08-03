package live

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestGHPRCreatorRequiresRepositoryAndWorkspace(t *testing.T) {
	creator := GHPRCreator{}
	_, err := creator.CreateOrReuse(context.Background(), dashboard.Repository{}, dashboard.IntakeStatus{})
	if err == nil {
		t.Fatal("CreateOrReuse succeeded without repository and workspace")
	}
}

func TestGHPRCreatorRetriesAfterTimedOutMutation(t *testing.T) {
	created := false
	createCalls := 0
	queries := 0
	creator := GHPRCreator{Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		joined := strings.Join(args, " ")
		switch {
		case name == "git" && strings.Contains(joined, "branch --show-current"):
			return exec.CommandContext(ctx, "printf", "%s", "feature/issue-9")
		case name == "gh" && strings.Contains(joined, "pr list"):
			queries++
			if created {
				return exec.CommandContext(ctx, "printf", "%s", `[{"number":42,"url":"https://github.com/acme/repo/pull/42","state":"OPEN","body":"<!-- hermestrator-pr:9 -->"}]`)
			}
			return exec.CommandContext(ctx, "printf", "%s", "[]")
		case name == "git" && strings.Contains(joined, "push"):
			return exec.CommandContext(ctx, "true")
		case name == "gh" && strings.Contains(joined, "pr create"):
			createCalls++
			created = true // GitHub accepted the mutation, but the client timed out.
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' 'https://github.com/acme/repo/pull/42'; exit 1")
		default:
			return exec.CommandContext(ctx, "false")
		}
	}}
	status := dashboard.IntakeStatus{
		ExecutorWorkspacePath: t.TempDir(),
		PublishedIssue:        dashboard.PublishedIssue{Number: 9},
	}
	repo := dashboard.Repository{FullName: "acme/repo"}
	if _, err := creator.CreateOrReuse(context.Background(), repo, status); err == nil {
		t.Fatal("first create should report the simulated timeout")
	}
	pr, err := creator.CreateOrReuse(context.Background(), repo, status)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 42 || createCalls != 1 || queries != 2 {
		t.Fatalf("pr=%+v createCalls=%d queries=%d, want existing PR, one create, two queries", pr, createCalls, queries)
	}
}

func TestGHPRCreatorCheckMergeable(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		want          bool
		wantReason    string
	}{
		{name: "clean", payload: `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"}`, want: true},
		{name: "conflicting", payload: `{"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY"}`, wantReason: "conflicting"},
		{name: "checks pending", payload: `{"mergeable":"MERGEABLE","mergeStateStatus":"BEHIND"}`, wantReason: "behind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := GHPRCreator{Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "printf", "%s", tc.payload)
			}}
			mergeable, reason, err := checker.CheckMergeable(context.Background(), dashboard.Repository{FullName: "acme/repo"}, 42)
			if err != nil || mergeable != tc.want {
				t.Fatalf("CheckMergeable = %v, %q, %v; want %v", mergeable, reason, err, tc.want)
			}
			if tc.wantReason != "" && reason != fmt.Sprintf("GitHub merge state is %s", tc.wantReason) && reason != fmt.Sprintf("GitHub reports pull request is %s", tc.wantReason) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.wantReason)
			}
		})
	}
}

func TestPullRequestURL(t *testing.T) {
	match := pullRequestURL.FindStringSubmatch("https://github.com/acme/repo/pull/42")
	if len(match) != 2 || match[1] != "42" {
		t.Fatalf("match = %#v", match)
	}
}
