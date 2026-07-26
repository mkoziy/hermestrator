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
		AllowedRedirectHosts: token.AllowedHostsFunc(func() ([]string, error) { return []string{}, nil }),
		AvatarStore:          avatar.NewNoOp(),
	})
	svc.AddProvider("github", c.ClientID, c.ClientSecret)
	authRoutes, _ := svc.Handlers()
	mux := http.NewServeMux()
	mux.Handle("/auth/", authRoutes)
	middleware := svc.Middleware()
	mux.Handle("/", middleware.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := token.GetUserInfo(r)
		if err != nil || user.Name == "" {
			http.Error(w, "GitHub operator identity required", http.StatusForbidden)
			return
		}
		r = r.Clone(r.Context())
		r.Header.Set("X-PM-User", user.Name)
		next.ServeHTTP(w, r)
	})))
	return mux, nil
}
