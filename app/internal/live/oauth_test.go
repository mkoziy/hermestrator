package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func oauthRequest(t *testing.T, secret, login string) *http.Request {
	t.Helper()
	service := token.NewService(token.Opts{SecretReader: token.SecretFunc(func(string) (string, error) { return secret, nil }), SecureCookies: false})
	signed, err := service.Token(token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{"test"}},
		User:             &token.User{ID: "github_1", Attributes: map[string]any{"login": login}},
		AuthProvider:     &token.AuthProvider{Name: "github"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/repositories", nil)
	request.AddCookie(&http.Cookie{Name: service.JWTCookieName, Value: signed})
	return request
}
