// Command pm runs the authenticated Genkit PM dashboard.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	issueWorkspaceBase := envOr("PM_EXECUTOR_WORKSPACE_DIR", filepath.Join(os.TempDir(), "hermestrator-executor-workspaces"))
	planningProfile := envOr("PM_PLANNING_PROFILE", filepath.Join(os.TempDir(), "hermestrator-planning-profile.json"))
	processRunner := &live.ProcessRunner{}
	runStore, err := live.NewImplementationRunStore(store)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = runStore.Close() }()
	planner := &live.Planner{Runner: processRunner, ProfilePath: planningProfile, RalphexPlanningConfigDir: envOr("PM_RALPHEX_PLANNING_CONFIG_DIR", filepath.Join(os.TempDir(), "hermestrator-ralphex-planning"))}
	reviewer := live.GHReviewer{
		StandardsModel: live.NewReviewModelFunc(model.Genkit(), envOr("PM_MODEL_REVIEW_STANDARDS", envOr("PM_MODEL_DISCOVERY", "openai/gpt-4.1-mini"))),
		SpecModel:      live.NewReviewModelFunc(model.Genkit(), envOr("PM_MODEL_REVIEW_SPEC", envOr("PM_MODEL_DISCOVERY", "openai/gpt-4.1-mini"))),
	}
	app, err := dashboard.New(dashboard.Dependencies{
		GitHub:      live.GitHub{Token: os.Getenv("GH_TOKEN")},
		Model:       model,
		Telegram:    live.Telegram{BotToken: os.Getenv("TELEGRAM_BOT_TOKEN"), ChatID: os.Getenv("TELEGRAM_CHAT_ID")},
		Publisher:   live.GHPublisher{},
		Synthesizer: live.NewGenkitSynthesizer(model.Genkit()),
		Intake: live.CloneIntake{
			BaseDir:      intakeBase,
			WorkspaceDir: envOr("PM_ISSUE_WORKSPACE_DIR", "issue-workspaces"),
		},
		Store:          store,
		AllowedUsers:   live.AllowedUsers(os.Getenv("PM_ALLOWED_GITHUB_USERS")),
		DashboardURL:   dashboardURL,
		ExecutorRunner: live.DashboardExecutorRunner{Runner: processRunner},
		Planner:        planner,
		Critiquer: live.DashboardCritiquer{Critiquer: &live.Critiquer{
			Planner:   planner,
			ModelFunc: live.NewCritiqueModelFunc(model.Genkit(), envOr("PM_MODEL_CRITIQUE", envOr("PM_MODEL_DISCOVERY", "openai/gpt-4.1-mini")), 0),
		}},
		IssueWorkspace:            live.IssueWorkspace{BaseDir: issueWorkspaceBase},
		Preflight:                 live.DashboardPreflight{Preflight: &live.Preflight{}},
		VerificationRunner:        live.DashboardVerificationRunner{Runner: &live.VerificationRunner{Runner: processRunner}},
		RunLease:                  live.DashboardRunLease{Store: runStore},
		PRCreator:                 live.GHPRCreator{},
		Reviewer:                  reviewer,
		ReviewCommenter:           reviewer,
		MergeabilityChecker:       live.GHPRCreator{},
		MergeExecutor:             live.GHMergeExecutor{},
		RalphexExecutionConfigDir: envOr("PM_RALPHEX_EXECUTION_CONFIG_DIR", filepath.Join(os.TempDir(), "hermestrator-ralphex-execution")),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := app.ReconcileStartup(ctx, live.GHPRCreator{}); err != nil {
		log.Printf("startup PR reconciliation: %v", err)
	}
	if err := live.RecoverLocks(ctx, runStore, live.WorkspaceClassifier{}, issueWorkspaceBase, nil); err != nil {
		log.Printf("startup implementation-run recovery: %v", err)
	}
	if err := app.CleanupExpiredFailedWorkspaces(ctx, live.RetentionWindowFromEnv(7*24*time.Hour)); err != nil {
		log.Printf("failed workspace retention: %v", err)
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
