package live

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestGHMergeExecutorSkipsAlreadyDoneMutations(t *testing.T) {
	calls := 0
	e := GHMergeExecutor{Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		payload := `{"state":"MERGED","mergedAt":"2026-08-03T00:00:00Z"}`
		if strings.Contains(strings.Join(args, " "), "issue") {
			payload = `{"state":"CLOSED"}`
		}
		return exec.CommandContext(ctx, "printf", "%s", payload)
	}}
	if err := e.Merge(context.Background(), dashboard.Repository{FullName: "acme/repo"}, 7); err != nil {
		t.Fatal(err)
	}
	if err := e.CloseIssue(context.Background(), dashboard.Repository{FullName: "acme/repo"}, 8); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2 queries and no mutations", calls)
	}
}
