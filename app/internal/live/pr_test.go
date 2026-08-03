package live

import (
	"context"
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

func TestPullRequestURL(t *testing.T) {
	match := pullRequestURL.FindStringSubmatch("https://github.com/acme/repo/pull/42")
	if len(match) != 2 || match[1] != "42" {
		t.Fatalf("match = %#v", match)
	}
}
