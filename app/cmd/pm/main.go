// Command pm runs the authenticated Genkit PM dashboard.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mkoziy/hermestrator/internal/dashboard"
	"github.com/mkoziy/hermestrator/internal/live"
)

func main() {
	ctx := context.Background()
	store := envOr("PM_SQLITE_PATH", "pm.db")
	model, err := live.NewOpenRouterModel(ctx, os.Getenv("OPENROUTER_API_KEY"), envOr("PM_MODEL_DISCOVERY", "openai/gpt-4.1-mini"), store)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = model.Close() }()
	dashboardURL := envOr("PM_DASHBOARD_URL", "http://localhost:8080")
	intakeBase := envOr("PM_INTAKE_DIR", filepath.Join(os.TempDir(), "hermestrator-intakes"))
	app, err := dashboard.New(dashboard.Dependencies{
		GitHub:    live.GitHub{Token: os.Getenv("GH_TOKEN")},
		Model:     model,
		Telegram:  live.Telegram{BotToken: os.Getenv("TELEGRAM_BOT_TOKEN"), ChatID: os.Getenv("TELEGRAM_CHAT_ID")},
		Publisher: live.GHPublisher{},
		Intake: live.CloneIntake{
			BaseDir:      intakeBase,
			WorkspaceDir: envOr("PM_ISSUE_WORKSPACE_DIR", "issue-workspaces"),
		},
		Store:        store,
		AllowedUsers: live.AllowedUsers(os.Getenv("PM_ALLOWED_GITHUB_USERS")),
		DashboardURL: dashboardURL,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = app.Close() }()
	handler, err := (live.GitHubOAuth{BaseURL: dashboardURL, ClientID: os.Getenv("GITHUB_OAUTH_CLIENT_ID"), ClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"), JWTSecret: os.Getenv("PM_JWT_SECRET"), SecureCookie: !strings.HasPrefix(dashboardURL, "http://localhost")}).Wrap(app)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("PM dashboard listening on %s", envOr("PM_LISTEN_ADDR", ":8080"))
	if err := http.ListenAndServe(envOr("PM_LISTEN_ADDR", ":8080"), handler); err != nil {
		log.Print(err)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
