package dashboard

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

type fakeGitHub struct{ repos []Repository }

func (f fakeGitHub) Repositories(context.Context) ([]Repository, error) { return f.repos, nil }

type fakeModel struct{}

func (fakeModel) Reply(_ context.Context, _ Conversation, prompt string) (Reply, error) {
	return Reply{Text: "What outcome would make " + prompt + " successful?", Tokens: 21, CostUSD: 0.0042}, nil
}

func (fakeModel) Status(context.Context, string) (Status, error) {
	return Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "12s", RecentActivity: "awaiting operator"}, nil
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

type blockingStreamingModel struct {
	fakeModel
	started sync.Once
	ready   chan struct{}
	release chan struct{}
}

func (m *blockingStreamingModel) Stream(_ context.Context, _ Conversation, _ string, emit func(string) error) (Reply, error) {
	m.started.Do(func() { close(m.ready) })
	<-m.release
	if err := emit("resumed reply"); err != nil {
		return Reply{}, err
	}
	return Reply{Text: "resumed reply"}, nil
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
	if !strings.Contains(page.Body.String(), "Tokens: 21") || !strings.Contains(page.Body.String(), "Cost: 0.0042") {
		t.Fatalf("durable telemetry missing: %q", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "Elapsed: 12s") || !strings.Contains(page.Body.String(), "awaiting operator") {
		t.Fatalf("durable agent status missing: %q", page.Body.String())
	}
}

func TestProtectedRoutesRejectUnapprovedOperator(t *testing.T) {
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	response := request(t, app, http.MethodGet, "/repositories", "", "stranger")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestProtectedRoutesMatchGitHubLoginsCaseInsensitively(t *testing.T) {
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	response := request(t, app, http.MethodGet, "/repositories", "", "Michael")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestRepositoryRegistrationRejectsUnknownRepository(t *testing.T) {
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	response := request(t, app, http.MethodPost, "/repositories/not-listed", "", "michael")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestConversationRedactsSecretsBeforePersistenceAndRendering(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Store: database, AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	response := request(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"sk-abcdefghijklmnopqrstuvwxyz"}}.Encode(), "michael")
	if strings.Contains(response.Body.String(), "sk-abcdefghijklmnopqrstuvwxyz") || !strings.Contains(response.Body.String(), "[redacted]") {
		t.Fatalf("secret rendered in %q", response.Body.String())
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

	response := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"ship it"}}.Encode(), "michael")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "sse-connect") {
		t.Fatalf("SSE bridge missing: %q", response.Body.String())
	}
	streamURL := streamURL(t, response.Body.String())
	stream := request(t, app, http.MethodGet, streamURL, "", "michael")
	if stream.Code != http.StatusOK || stream.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(stream.Body.String(), "event: chunk") || !strings.Contains(stream.Body.String(), "A focused next step") || !strings.Contains(stream.Body.String(), "event: done") {
		t.Fatalf("SSE events missing: %d %q", stream.Code, stream.Body.String())
	}
	replay := request(t, app, http.MethodGet, streamURL, "", "michael")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), "event: done") {
		t.Fatalf("SSE replay missing: %d %q", replay.Code, replay.Body.String())
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var replies int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE repository_id='42' AND role='pm'`).Scan(&replies); err != nil {
		t.Fatal(err)
	}
	if replies != 1 {
		t.Fatalf("replayed SSE generated %d replies", replies)
	}

	restarted := mustApp(t, deps)
	page := request(t, restarted, http.MethodGet, "/repositories/42", "", "michael")
	if !strings.Contains(page.Body.String(), "A focused next step") {
		t.Fatalf("completed stream was not durable: %q", page.Body.String())
	}
}

func TestWorkspaceReconnectsToPendingTurnAfterReload(t *testing.T) {
	model := &blockingStreamingModel{ready: make(chan struct{}), release: make(chan struct{})}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: model, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	start := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"resume me"}}.Encode(), "michael")
	stream := streamURL(t, start.Body.String())
	<-model.ready

	page := request(t, app, http.MethodGet, "/repositories/42", "", "michael")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `sse-connect="`+stream+`"`) {
		t.Fatalf("pending turn was not reconnected: %d %q", page.Code, page.Body.String())
	}
	close(model.release)
	completed := request(t, app, http.MethodGet, stream, "", "michael")
	if !strings.Contains(completed.Body.String(), "event: done") {
		t.Fatalf("resumed turn did not complete: %q", completed.Body.String())
	}
}

func streamURL(t *testing.T, body string) string {
	t.Helper()
	const prefix = `sse-connect="`
	start := strings.Index(body, prefix)
	if start == -1 {
		t.Fatalf("no stream URL in %q", body)
	}
	value := body[start+len(prefix):]
	end := strings.Index(value, `"`)
	if end == -1 {
		t.Fatalf("unterminated stream URL in %q", body)
	}
	return value[:end]
}

func TestExistingConversationDatabaseMigratesTelemetryColumns(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	legacy, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE messages (repository_id TEXT NOT NULL, role TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	deps := Dependencies{
		GitHub:       fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        fakeModel{},
		Store:        database,
		AllowedUsers: map[string]bool{"michael": true},
	}
	app := mustApp(t, deps)
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")

	response := request(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"migrate"}}.Encode(), "michael")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Tokens: 21") {
		t.Fatalf("migrated conversation = %d %q", response.Code, response.Body.String())
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

func requestHTMX(t *testing.T, app http.Handler, method, target, body, user string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("X-PM-User", user)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}
