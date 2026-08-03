package live

import (
	"context"
	"fmt"
	"os/exec"
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
