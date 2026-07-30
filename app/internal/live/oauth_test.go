package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-pkgz/auth/v2/token"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mkoziy/hermestrator/internal/dashboard"
)

type oauthTestGitHub struct{}

func (oauthTestGitHub) Repositories(context.Context) ([]dashboard.Repository, error) {
	return []dashboard.Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}, nil
}

type oauthTestModel struct{}

func (oauthTestModel) Reply(context.Context, dashboard.Conversation, string) (dashboard.Reply, error) {
	return dashboard.Reply{Text: "What should we build?"}, nil
}

func (oauthTestModel) Status(context.Context, string) (dashboard.Status, error) {
	return dashboard.Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "0s", RecentActivity: "awaiting discovery"}, nil
}

type oauthStreamingModel struct{ oauthTestModel }

func (oauthStreamingModel) Stream(_ context.Context, _ dashboard.Conversation, _ string, emit func(string) error) (dashboard.Reply, error) {
	for _, chunk := range []string{"A focused ", "next step"} {
		if err := emit(chunk); err != nil {
			return dashboard.Reply{}, err
		}
	}
	return dashboard.Reply{Text: "A focused next step", Tokens: 12, CostUSD: 0.0012}, nil
}

type oauthTestTelegram struct{ notifications []dashboard.Notification }

func (t *oauthTestTelegram) Notify(_ context.Context, notification dashboard.Notification) error {
	t.notifications = append(t.notifications, notification)
	return nil
}

func TestFormXSRFBridgePromotesCookieForRegularFormPosts(t *testing.T) {
	handler := formXSRFBridge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-XSRF-TOKEN"); got != "form-token" {
			t.Fatalf("XSRF header = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/repositories/42", nil)
	req.AddCookie(&http.Cookie{Name: "XSRF-TOKEN", Value: "form-token"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestFormXSRFBridgeDoesNotOverrideHTMXHeader(t *testing.T) {
	handler := formXSRFBridge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-XSRF-TOKEN"); got != "htmx-token" {
			t.Fatalf("XSRF header = %q", got)
		}
	}))
	req := httptest.NewRequest(http.MethodPost, "/repositories/42", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-XSRF-TOKEN", "htmx-token")
	req.AddCookie(&http.Cookie{Name: "XSRF-TOKEN", Value: "form-token"})

	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestGitHubOAuthLoginRedirectsToRepositoryPicker(t *testing.T) {
	handler, err := (GitHubOAuth{
		BaseURL:      "http://localhost:8080",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		JWTSecret:    "jwt-secret",
	}).Wrap(http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/login", nil))
	if response.Code != http.StatusFound {
		t.Fatalf("login status = %d, want %d", response.Code, http.StatusFound)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Path != "/auth/github/login" {
		t.Fatalf("login path = %q, want /auth/github/login", location.Path)
	}
	if got := location.Query().Get("from"); got != "http://localhost:8080/repositories" {
		t.Fatalf("login return URL = %q", got)
	}
}

func TestGitHubOAuthRootRedirectsUnauthenticatedOperatorToLogin(t *testing.T) {
	handler, err := (GitHubOAuth{
		BaseURL:      "http://localhost:8080",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		JWTSecret:    "jwt-secret",
	}).Wrap(http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/login" {
		t.Fatalf("root response = %d %q", response.Code, response.Header().Get("Location"))
	}
}

func TestGitHubOAuthPassesAllowedIdentityToDashboardHandler(t *testing.T) {
	const secret = "test-jwt-secret"
	app, err := dashboard.New(dashboard.Dependencies{
		GitHub:       oauthTestGitHub{},
		Model:        oauthTestModel{},
		Store:        t.TempDir() + "/pm.db",
		AllowedUsers: map[string]bool{"michael": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	handler, err := (GitHubOAuth{BaseURL: "http://localhost:8080", ClientID: "client-id", ClientSecret: "client-secret", JWTSecret: secret}).Wrap(app)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, oauthRequest(t, secret, "Michael"))
	if response.Code != http.StatusOK || response.Body.String() == "" {
		t.Fatalf("allowed response = %d %q", response.Code, response.Body.String())
	}

	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, oauthRequest(t, secret, "stranger"))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d", denied.Code)
	}
}

func TestGitHubOAuthComposesFullDashboardLifecycle(t *testing.T) {
	const secret = "test-jwt-secret"
	database := t.TempDir() + "/pm.db"
	telegram := &oauthTestTelegram{}
	deps := dashboard.Dependencies{
		GitHub:       oauthTestGitHub{},
		Model:        oauthStreamingModel{},
		Telegram:     telegram,
		Store:        database,
		AllowedUsers: map[string]bool{"michael": true},
		DashboardURL: "https://pm.example",
	}
	app, err := dashboard.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()
	handler := oauthDashboardHandler(t, secret, app)

	repositories, repositoriesRequest := oauthDashboardRequest(t, secret, "Michael", http.MethodGet, "/repositories", "", false)
	handler.ServeHTTP(repositories, repositoriesRequest)
	if repositories.Code != http.StatusOK || !strings.Contains(repositories.Body.String(), "mkoziy/hermestrator") {
		t.Fatalf("repositories = %d %q", repositories.Code, repositories.Body.String())
	}

	selectRepo, selectRepoRequest := oauthDashboardRequest(t, secret, "Michael", http.MethodPost, "/repositories/42", "", false)
	handler.ServeHTTP(selectRepo, selectRepoRequest)
	if selectRepo.Code != http.StatusSeeOther || selectRepo.Header().Get("Location") != "/repositories/42" {
		t.Fatalf("repository selection = %d %q", selectRepo.Code, selectRepo.Header().Get("Location"))
	}

	turn, turnRequest := oauthDashboardRequest(t, secret, "Michael", http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"a dashboard"}}.Encode(), true)
	handler.ServeHTTP(turn, turnRequest)
	turnID := streamTurnID(t, turn.Body.String())

	stream, streamRequest := oauthDashboardRequest(t, secret, "Michael", http.MethodGet, "/repositories/42/conversation/"+turnID+"/stream", "", false)
	handler.ServeHTTP(stream, streamRequest)
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "A focused next step") || !strings.Contains(stream.Body.String(), "event: done") {
		t.Fatalf("stream = %d %q", stream.Code, stream.Body.String())
	}

	notification, notificationRequest := oauthDashboardRequest(t, secret, "Michael", http.MethodPost, "/notifications/test", "", false)
	handler.ServeHTTP(notification, notificationRequest)
	if notification.Code != http.StatusNoContent || len(telegram.notifications) != 1 || telegram.notifications[0].URL != "https://pm.example/repositories" {
		t.Fatalf("notification = %d %#v", notification.Code, telegram.notifications)
	}

	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := dashboard.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	restartedHandler := oauthDashboardHandler(t, secret, restarted)
	workspace, workspaceRequest := oauthDashboardRequest(t, secret, "Michael", http.MethodGet, "/repositories/42", "", false)
	restartedHandler.ServeHTTP(workspace, workspaceRequest)
	for _, want := range []string{"a dashboard", "A focused next step", "Tokens: 12", "Cost: 0.0012"} {
		if !strings.Contains(workspace.Body.String(), want) {
			t.Fatalf("restarted workspace missing %q: %q", want, workspace.Body.String())
		}
	}
}

func oauthDashboardHandler(t *testing.T, secret string, app http.Handler) http.Handler {
	t.Helper()
	handler, err := (GitHubOAuth{BaseURL: "http://localhost:8080", ClientID: "client-id", ClientSecret: "client-secret", JWTSecret: secret}).Wrap(app)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func oauthDashboardRequest(t *testing.T, secret, login, method, target, form string, htmx bool) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	request := oauthRequestWithMethod(t, secret, login, method, target, form)
	if htmx {
		request.Header.Set("HX-Request", "true")
		request.Header.Set("X-XSRF-TOKEN", "xsrf")
	}
	return httptest.NewRecorder(), request
}

var streamPath = regexp.MustCompile(`/repositories/42/conversation/([0-9a-f-]+)/stream`)

func streamTurnID(t *testing.T, body string) string {
	t.Helper()
	matches := streamPath.FindStringSubmatch(body)
	if len(matches) != 2 {
		t.Fatalf("stream start missing turn ID: %q", body)
	}
	return matches[1]
}

func oauthRequest(t *testing.T, secret, login string) *http.Request {
	return oauthRequestWithMethod(t, secret, login, http.MethodGet, "/repositories", "")
}

func oauthRequestWithMethod(t *testing.T, secret, login, method, target, form string) *http.Request {
	t.Helper()
	service := token.NewService(token.Opts{SecretReader: token.SecretFunc(func(string) (string, error) { return secret, nil }), SecureCookies: false})
	signed, err := service.Token(token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{ID: "xsrf", Audience: jwt.ClaimStrings{"test"}},
		User:             &token.User{ID: "github_1", Attributes: map[string]any{"login": login}},
		AuthProvider:     &token.AuthProvider{Name: "github"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, target, strings.NewReader(form))
	if form != "" {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	request.AddCookie(&http.Cookie{Name: service.JWTCookieName, Value: signed})
	request.AddCookie(&http.Cookie{Name: "XSRF-TOKEN", Value: "xsrf"})
	return request
}
