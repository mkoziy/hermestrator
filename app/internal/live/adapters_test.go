package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/firebase/genkit/go/core"
	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestGitHubRepositoriesFollowsPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer automation-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/first":
			w.Header().Set("Link", "<"+serverURL(r)+"/second>; rel=\"next\"")
			_, _ = w.Write([]byte(`[{"id":1,"full_name":"acme/first"}]`))
		case "/second":
			_, _ = w.Write([]byte(`[{"id":2,"full_name":"acme/second"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repos, err := (GitHub{Token: "automation-token", ReposURL: server.URL + "/first"}).Repositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].FullName != "acme/first" || repos[1].FullName != "acme/second" {
		t.Fatalf("repositories = %#v", repos)
	}
}

func TestIsMissingSnapshot(t *testing.T) {
	if !isMissingSnapshot(core.NewError(core.NOT_FOUND, "no snapshot found")) {
		t.Fatal("NOT_FOUND snapshot error was not recognized")
	}
	if isMissingSnapshot(core.NewError(core.INTERNAL, "store unavailable")) {
		t.Fatal("non-NOT_FOUND error was recognized as a missing snapshot")
	}
}

func serverURL(r *http.Request) string { return "http://" + r.Host }

func TestGitHubNextPage(t *testing.T) {
	if got := githubNextPage(`<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=3>; rel="last"`); got != "https://api.github.com/user/repos?page=2" {
		t.Fatalf("next page = %q", got)
	}
}

func TestOpenRouterStatusDefaultsBeforeFirstTurn(t *testing.T) {
	model, err := NewOpenRouterModel(context.Background(), "test-key", "openai/gpt-4.1-mini", t.TempDir()+"/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = model.Close() })

	status, err := model.Status(context.Background(), "42")
	if err != nil {
		t.Fatalf("status before first turn: %v", err)
	}
	want := dashboard.Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "0s", RecentActivity: "awaiting discovery"}
	if status != want {
		t.Fatalf("status = %#v, want %#v", status, want)
	}
}
