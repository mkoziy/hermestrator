package live

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-pkgz/auth/v2"
	"github.com/go-pkgz/auth/v2/avatar"
	"github.com/go-pkgz/auth/v2/token"
)

// GitHubOAuth configures go-pkgz/auth's GitHub OAuth and JWT cookie middleware.
type GitHubOAuth struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	JWTSecret    string
	SecureCookie bool
}

func (c GitHubOAuth) Wrap(next http.Handler) (http.Handler, error) {
	if c.BaseURL == "" || c.ClientID == "" || c.ClientSecret == "" || c.JWTSecret == "" {
		return nil, fmt.Errorf("GitHub OAuth base URL, client ID, client secret, and JWT secret are required")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid dashboard URL")
	}
	svc := auth.NewService(auth.Opts{
		URL:                  strings.TrimRight(c.BaseURL, "/"),
		SecretReader:         token.SecretFunc(func(string) (string, error) { return c.JWTSecret, nil }),
		SecureCookies:        c.SecureCookie,
		TokenDuration:        24 * time.Hour,
		CookieDuration:       7 * 24 * time.Hour,
		SameSiteCookie:       http.SameSiteLaxMode,
		XSRFIgnoreMethods:    []string{http.MethodGet, http.MethodHead, http.MethodOptions},
		AllowedRedirectHosts: token.AllowedHostsFunc(func() ([]string, error) { return []string{}, nil }),
		AvatarStore:          avatar.NewNoOp(),
	})
	// GitHub's provider places the profile display name in User.Name. Preserve
	// the immutable account login separately so the configured allowlist has
	// the semantics its name promises.
	svc.AddProviderWithUserAttributes("github", c.ClientID, c.ClientSecret, map[string]string{"login": "login"})
	authRoutes, _ := svc.Handlers()
	mux := http.NewServeMux()
	mux.Handle("/auth/", authRoutes)
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		login := &url.URL{Path: "/auth/github/login"}
		query := login.Query()
		query.Set("from", strings.TrimRight(c.BaseURL, "/")+"/repositories")
		login.RawQuery = query.Encode()
		http.Redirect(w, r, login.String(), http.StatusFound)
	})
	middleware := svc.Middleware()
	mux.Handle("/", formXSRFBridge(middleware.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := token.GetUserInfo(r)
		login := user.StrAttr("login")
		if err != nil || login == "" {
			http.Error(w, "GitHub operator identity required", http.StatusForbidden)
			return
		}
		r = r.Clone(r.Context())
		r.Header.Set("X-PM-User", login)
		next.ServeHTTP(w, r)
	}))))
	return mux, nil
}

// formXSRFBridge supplies the double-submit token for regular same-origin HTML
// form posts. HTMX already sends this header itself. The auth middleware still
// compares the value against the signed JWT claim before allowing the request.
func formXSRFBridge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("X-XSRF-TOKEN") == "" && r.Header.Get("HX-Request") != "true" {
			if cookie, err := r.Cookie("XSRF-TOKEN"); err == nil && cookie.Value != "" {
				r.Header.Set("X-XSRF-TOKEN", cookie.Value)
			}
		}
		next.ServeHTTP(w, r)
	})
}
