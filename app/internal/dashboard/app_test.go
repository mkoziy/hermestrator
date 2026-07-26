package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type fakeGitHub struct{ repos []Repository }

func (f fakeGitHub) Repositories(context.Context) ([]Repository, error) { return f.repos, nil }

type fakeModel struct{}

func (fakeModel) Reply(_ context.Context, _ Conversation, prompt string) (Reply, error) {
	return Reply{Text: "What outcome would make " + prompt + " successful?", Tokens: 21, CostUSD: 0.0042}, nil
}

type streamingFakeModel struct{ fakeModel }

func (streamingFakeModel) Stream(_ context.Context, _ Conversation, _ string, emit func(string) error) (Reply, error) {
	if err := emit("A focused "); err != nil {
		return Reply{}, err
	}
	if err := emit("next step"); err != nil {
		return Reply{}, err
	}
	return Reply{Text: "A focused next step", Tokens: 12, CostUSD: 0.0012}, nil
}

type fakeTelegram struct{ messages []string }

func (f *fakeTelegram) Notify(_ context.Context, message string) error {
	f.messages = append(f.messages, message)
	return nil
}

func TestOperatorCanSelectRepositoryAndContinueConversationAfterRestart(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	telegram := &fakeTelegram{}
	deps := Dependencies{
		GitHub:       fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        fakeModel{},
		Telegram:     telegram,
		Store:        database,
		AllowedUsers: map[string]bool{"michael": true},
	}
	app := mustApp(t, deps)

	repos := request(t, app, http.MethodGet, "/repositories", "", "michael")
	if !strings.Contains(repos.Body.String(), "mkoziy/hermestrator") {
		t.Fatalf("repository picker = %q", repos.Body.String())
	}

	selectRepo := request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	if selectRepo.Code != http.StatusSeeOther {
		t.Fatalf("select status = %d", selectRepo.Code)
	}

	turn := request(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"a dashboard"}}.Encode(), "michael")
	if turn.Code != http.StatusOK || !strings.Contains(turn.Body.String(), "What outcome would make a dashboard successful?") {
		t.Fatalf("turn = %d %q", turn.Code, turn.Body.String())
	}
	if !strings.Contains(turn.Body.String(), "discovery") || !strings.Contains(turn.Body.String(), "0.0042") {
		t.Fatalf("telemetry absent: %q", turn.Body.String())
	}
	if len(telegram.messages) != 0 {
		t.Fatalf("ordinary turn notified Telegram: %#v", telegram.messages)
	}

	restarted := mustApp(t, deps)
	page := request(t, restarted, http.MethodGet, "/repositories/42", "", "michael")
	if !strings.Contains(page.Body.String(), "a dashboard") || !strings.Contains(page.Body.String(), "What outcome") {
		t.Fatalf("durable conversation missing: %q", page.Body.String())
	}
}

func TestProtectedRoutesRejectUnapprovedOperator(t *testing.T) {
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	response := request(t, app, http.MethodGet, "/repositories", "", "stranger")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestTestNotificationIsReadOnlyAndLinksToDashboard(t *testing.T) {
	telegram := &fakeTelegram{}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Telegram: telegram, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}, DashboardURL: "https://pm.example"})
	response := request(t, app, http.MethodPost, "/notifications/test", "", "michael")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if len(telegram.messages) != 1 || !strings.Contains(telegram.messages[0], "https://pm.example/repositories") {
		t.Fatalf("message = %#v", telegram.messages)
	}
}

func TestConversationStreamsHTMXFragmentsAndPersistsTheCompletedTurn(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	deps := Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: streamingFakeModel{}, Store: database, AllowedUsers: map[string]bool{"michael": true}}
	app := mustApp(t, deps)
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")

	response := request(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"ship it"}}.Encode(), "michael")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "hx-swap-oob") || !strings.Contains(response.Body.String(), "A focused next step") {
		t.Fatalf("stream fragments missing: %q", response.Body.String())
	}

	restarted := mustApp(t, deps)
	page := request(t, restarted, http.MethodGet, "/repositories/42", "", "michael")
	if !strings.Contains(page.Body.String(), "A focused next step") {
		t.Fatalf("completed stream was not durable: %q", page.Body.String())
	}
}

func mustApp(t *testing.T, deps Dependencies) http.Handler {
	t.Helper()
	app, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	return app
}
func request(t *testing.T, app http.Handler, method, target, body, user string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("X-PM-User", user)
	if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}
