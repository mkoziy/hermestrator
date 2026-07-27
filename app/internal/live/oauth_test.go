package live

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

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
