// Package dashboard serves the operator-facing PM workspace.
package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/mkoziy/hermestrator/internal/redaction"
	_ "modernc.org/sqlite"
)

type turnState string

const (
	turnPending   turnState = "pending"
	turnRunning   turnState = "running"
	turnCompleted turnState = "completed"
	turnFailed    turnState = "failed"
	turnCanceled  turnState = "canceled"
)

type Repository struct{ ID, FullName string }
type Message struct {
	Role, Text string
	CreatedAt  time.Time
}
type Conversation struct {
	RepositoryID       string
	RepositoryEvidence string
	Messages           []Message
	PendingTurns       []PendingTurn
	HasPending         bool
	LastReply          Reply
	Status             Status
}
type Workspace struct {
	Conversation
	Intake            IntakeStatus
	Artifacts         []Artifact
	ExecutorSelection *ExecutorSelection
}
type PendingTurn struct {
	TurnID, RepositoryID string
	Sending              bool
}
type Status struct{ Phase, ModelRole, Elapsed, RecentActivity string }
type Reply struct {
	Text    string
	Tokens  int
	CostUSD float64
}
type Notification struct{ Text, URL string }

// Publication is a confirmed, English-language GitHub issue candidate. It is
// intentionally separate from GitHub repository discovery: listing uses the
// automation identity while publishing is available only after the dashboard
// has enforced its confirmation gate.
type Publication struct {
	Title, Body string
	BlockedBy   []int
	Key         string
}

type PublishedIssue struct {
	Number int
	URL    string
}

type PullRequest struct {
	Number     int
	URL, State string
}
type PRCreator interface {
	CreateOrReuse(context.Context, Repository, IntakeStatus) (PullRequest, error)
}
type ReviewResult struct {
	Approved bool
	Findings string
	Blocked  bool
}
type Reviewer interface {
	Review(context.Context, Repository, PullRequest, IntakeStatus) (ReviewResult, error)
}
type ReviewCommenter interface {
	PostFindings(context.Context, Repository, PullRequest, string, int) error
}
type MergeabilityChecker interface {
	CheckMergeable(context.Context, Repository, int) (bool, string, error)
}
type MergeExecutor interface {
	ConfirmMerged(context.Context, Repository, int) error
	Merge(context.Context, Repository, int) error
	CloseIssue(context.Context, Repository, int) error
}

// Publisher owns the only tracker mutation available in the intake slice.
// Implementations must not publish an unconfirmed draft.
type Publisher interface {
	Publish(context.Context, Repository, []Publication) ([]PublishedIssue, error)
}

// Intake creates an isolated, read-only discovery clone and promotes it only
// after issue publication. It never receives executor authority.
type Intake interface {
	Start(context.Context, Repository) (string, error)
	Promote(context.Context, string, PublishedIssue) (string, error)
	Cleanup(context.Context, string) error
}

// ContextUpdater is deliberately optional: it is the sole documentation write
// capability granted to an intake clone, never a production-code writer.
type ContextUpdater interface {
	UpdateContext(context.Context, string, string) error
}

// Inspector supplies read-only repository facts for discovery. It is kept
// distinct from ContextUpdater so inspection never implies write authority.
type Inspector interface {
	Inspect(context.Context, string) (string, error)
}

// Synthesizer turns settled discovery output into drafts. Implementations
// must not call GitHub or grant write authority; see Intake and Publisher.
type Synthesizer interface {
	GrillWithDocs(context.Context, Conversation) ([]string, error)
	ToSpec(context.Context, Repository, []string) (string, error)
	ToTickets(context.Context, Repository, []string) (string, error)
	AssessADR(context.Context, string) (assessment, proposal string, err error)
}

// localSynthesizer calls the exported pure functions directly with no I/O.
type localSynthesizer struct{}

func (localSynthesizer) GrillWithDocs(_ context.Context, c Conversation) ([]string, error) {
	return GrillWithDocs(c), nil
}
func (localSynthesizer) ToSpec(_ context.Context, r Repository, resolved []string) (string, error) {
	return ToSpec(r, resolved), nil
}
func (localSynthesizer) ToTickets(_ context.Context, r Repository, resolved []string) (string, error) {
	return ToTickets(r, resolved), nil
}
func (localSynthesizer) AssessADR(_ context.Context, decision string) (string, string, error) {
	assessment, proposal := AssessADR(decision)
	return assessment, proposal, nil
}

type artifactKind string

const (
	artifactGlossary            artifactKind = "glossary"
	artifactRepositoryEvidence  artifactKind = "repository-evidence"
	artifactSpec                artifactKind = "spec"
	artifactTickets             artifactKind = "tickets"
	artifactADRAssessmentPrefix              = "adr-assessment-"
	artifactADRProposalPrefix                = "adr-proposal-"
	artifactExecutorOutput      artifactKind = "executor-output"
	artifactVerificationOutput  artifactKind = "verification-output"
)

type Artifact struct {
	Kind              artifactKind
	Body              string
	Confirmed         bool
	NeedsConfirmation bool
	URL               string
}

type intakeState string

const (
	intakeDraft      intakeState = "draft"
	intakeReady      intakeState = "ready"
	intakeConfirmed  intakeState = "confirmed"
	intakePublishing intakeState = "publishing"
	intakePromoting  intakeState = "promoting"
	intakeAbandoning intakeState = "abandoning"
	intakePublished  intakeState = "published"
	intakeAbandoned  intakeState = "abandoned"
)

type executorState string

const MaxReviewRounds = 3

const (
	executorSelected      executorState = "selected"
	executorPlanning      executorState = "planning"
	executorPlanned       executorState = "planned"
	executorApproved      executorState = "approved"
	executorRunning       executorState = "running"
	executorCompleted     executorState = "completed"
	executorVerifying     executorState = "verifying"
	executorVerified      executorState = "verified"
	executorCreatingPR    executorState = "creating_pr"
	executorPRCreated     executorState = "pr_created"
	executorReviewing     executorState = "reviewing"
	executorReviewBlocked executorState = "review_blocked"
	executorFixing        executorState = "fixing"
	executorMergeReady    executorState = "merge_ready"
	executorMergeApproved executorState = "merge_approved"
	executorMerging       executorState = "merging"
	executorMerged        executorState = "merged"
	executorCleanupDone   executorState = "cleanup_done"
	executorFailed        executorState = "failed"
)

type IntakeStatus struct {
	ID                    string
	State                 intakeState
	Path                  string
	MessageStart          int64
	PendingQuestion       string
	PublishedIssue        PublishedIssue
	PublishedIssues       []PublishedIssue
	ExecutorKind          string
	ExecutorRationale     string
	ExecutorState         executorState
	ExecutorHeartbeat     string
	ExecutorDuration      time.Duration
	ExecutorExitCode      int
	ExecutorCancelled     bool
	ExecutorCompleted     bool
	ExecutorWorkspacePath string
	RunID                 string
	ReviewRound           int
	ReviewFindings        string
	PR                    PullRequest
	VerificationOutput    string
}

// GitHub is deliberately the small automation boundary used by the handler.
type GitHub interface {
	Repositories(context.Context) ([]Repository, error)
}

// ExecutorRunner executes a subprocess and streams output line-by-line.
// The callback receives sanitised lines; the caller must not persist
// raw executor output before redaction.
type ExecutorRunner interface {
	Run(ctx context.Context, workspacePath string, onLine func(line string) error, name string, args ...string) (ExecutorRunResult, error)
}

type RunLease interface {
	Acquire(context.Context, string, int, ExecutorKind) (string, error)
	Release(context.Context, string, string, string) error
	RecentFailures(context.Context, string, int) ([]FailureRecord, error)
}

type PRStateReader interface {
	State(ctx context.Context, repo Repository, number int) (string, error)
}

type ActiveRun struct{ RunID string }

type ActiveRunLister interface {
	ListActive(context.Context) ([]ActiveRun, error)
}

// ExecutorRunResult captures the final state of an executor run.
type ExecutorRunResult struct {
	ExitCode  int
	Duration  time.Duration
	Cancelled bool
}

// Model is implemented by the Genkit/OpenRouter adapter in production and faked at the HTTP seam.
type Model interface {
	Reply(context.Context, Conversation, string) (Reply, error)
}

// StreamingModel is implemented by models that can yield a turn while it is
// being generated. The handler deliberately keeps Model small so HTTP tests
// can use a simple deterministic fake.
type StreamingModel interface {
	Stream(context.Context, Conversation, string, func(string) error) (Reply, error)
}
type StatusModel interface {
	Status(context.Context, string) (Status, error)
}
type Telegram interface {
	Notify(context.Context, Notification) error
}

type Dependencies struct {
	GitHub                    GitHub
	Model                     Model
	Telegram                  Telegram
	Publisher                 Publisher
	Intake                    Intake
	Store                     string
	AllowedUsers              map[string]bool
	DashboardURL              string
	Synthesizer               Synthesizer
	ExecutorRunner            ExecutorRunner
	Planner                   Planner
	Critiquer                 Critiquer
	IssueWorkspace            IssueClone
	Preflight                 Preflight
	VerificationRunner        VerificationRunner
	RunLease                  RunLease
	PRCreator                 PRCreator
	MergeabilityChecker       MergeabilityChecker
	MergeExecutor             MergeExecutor
	Reviewer                  Reviewer
	ReviewCommenter           ReviewCommenter
	RalphexExecutionConfigDir string
}

type application struct {
	deps      Dependencies
	db        *sql.DB
	handler   http.Handler
	templates *template.Template
	ctx       context.Context
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	mu        sync.Mutex
	turnMu    sync.Mutex
	closed    bool

	execCancels   map[string]context.CancelFunc
	execCancelsMu sync.Mutex
}

var errTurnInProgress = errors.New("PM response already in progress")

func New(deps Dependencies) (*application, error) {
	if deps.GitHub == nil || deps.Model == nil || deps.Store == "" {
		return nil, errors.New("dashboard requires GitHub, model, and SQLite store")
	}
	if deps.Synthesizer == nil {
		deps.Synthesizer = localSynthesizer{}
	}
	db, err := sql.Open("sqlite", deps.Store)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE IF NOT EXISTS repositories (id TEXT PRIMARY KEY, full_name TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS genkit_sessions (repository_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated_at TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS messages (repository_id TEXT NOT NULL, role TEXT NOT NULL, text TEXT NOT NULL, tokens INTEGER, cost_usd REAL, created_at TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS activity (repository_id TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS pending_turns (id TEXT PRIMARY KEY, repository_id TEXT NOT NULL, prompt TEXT NOT NULL, started_at TEXT, completed_at TEXT, state TEXT NOT NULL DEFAULT 'pending', terminal_reason TEXT);
		CREATE TABLE IF NOT EXISTS turn_events (id INTEGER PRIMARY KEY AUTOINCREMENT, turn_id TEXT NOT NULL, event TEXT NOT NULL, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS intakes (repository_id TEXT PRIMARY KEY, intake_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, clone_path TEXT NOT NULL DEFAULT '', inspection TEXT NOT NULL DEFAULT '', message_start INTEGER NOT NULL DEFAULT 0, pending_question TEXT NOT NULL DEFAULT '', issue_number INTEGER, issue_url TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL, executor_kind TEXT NOT NULL DEFAULT '', executor_rationale TEXT NOT NULL DEFAULT '');
		CREATE TABLE IF NOT EXISTS intake_artifacts (repository_id TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL, confirmed_at TEXT, url TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, PRIMARY KEY(repository_id, kind));
		CREATE TABLE IF NOT EXISTS intake_issues (repository_id TEXT NOT NULL, ticket_index INTEGER NOT NULL, issue_number INTEGER NOT NULL, issue_url TEXT NOT NULL, PRIMARY KEY(repository_id, ticket_index));`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "messages", "tokens", "INTEGER"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "messages", "cost_usd", "REAL"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "pending_turns", "started_at", "TEXT"); err != nil {
		_ = db.Close()
		return nil, err
	}
	stateExists, err := columnExists(db, "pending_turns", "state")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !stateExists {
		if err = addColumnIfMissing(db, "pending_turns", "state", "TEXT NOT NULL DEFAULT 'pending'"); err != nil {
			_ = db.Close()
			return nil, err
		}
		if _, err = db.Exec(`UPDATE pending_turns SET state=CASE WHEN completed_at IS NULL THEN 'pending' ELSE 'completed' END`); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err = addColumnIfMissing(db, "pending_turns", "terminal_reason", "TEXT"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "inspection", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "issue_number", "INTEGER"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "issue_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "message_start", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "pending_question", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "intake_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "executor_kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "executor_rationale", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "executor_state", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "executor_heartbeat", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "executor_duration_ns", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "executor_exit_code", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "executor_cancelled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "executor_workspace_path", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "pr_number", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "review_findings", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, fmt.Errorf("add review findings column: %w", err)
	}
	if err = addColumnIfMissing(db, "intakes", "review_round", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, fmt.Errorf("add review round column: %w", err)
	}
	if err = addColumnIfMissing(db, "intakes", "pr_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err = addColumnIfMissing(db, "intakes", "run_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = recoverDuplicatePendingTurns(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS one_active_pending_turn_per_repository ON pending_turns(repository_id) WHERE state IN ('pending','running')`); err != nil {
		_ = db.Close()
		return nil, err
	}
	executorControls := `{{else if eq .Intake.ExecutorState "verified"}}<p>State: verified</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/create-pr"><button class="btn btn-primary">Create pull request</button></form>{{else if eq .Intake.ExecutorState "creating_pr"}}<p>State: creating pull request</p>{{else if eq .Intake.ExecutorState "pr_created"}}<p>State: pull request created</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/review"><button class="btn btn-primary">Review pull request</button></form>{{else if eq .Intake.ExecutorState "reviewing"}}<p>State: reviewing pull request</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/cancel"><button class="btn btn-outline-danger">Cancel review</button></form>{{else if eq .Intake.ExecutorState "review_blocked"}}<p>State: review blocked</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/fix"><button class="btn btn-primary">Fix review findings</button></form>{{else if eq .Intake.ExecutorState "merge_ready"}}<p>State: merge ready</p>{{if .Intake.PR.URL}}<a href="{{.Intake.PR.URL}}">View pull request</a>{{end}}<form method="post" action="/repositories/{{.RepositoryID}}/executor/approve-merge"><button class="btn btn-success">Approve merge</button></form>{{else if eq .Intake.ExecutorState "merge_approved"}}<p>State: merge approved</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/merge"><button class="btn btn-success">Merge pull request</button></form>{{else if eq .Intake.ExecutorState "merging"}}<p>State: merging pull request</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/cancel"><button class="btn btn-outline-danger">Cancel merge</button></form>{{else if eq .Intake.ExecutorState "merged"}}<p>State: merged</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/cleanup"><button class="btn btn-primary">Clean up workspace</button></form>{{else if eq .Intake.ExecutorState "failed"}}<p>State: failed</p><p>Duration: {{.Intake.ExecutorDuration}}</p><p>Exit code: {{.Intake.ExecutorExitCode}}</p>{{if .Intake.ExecutorCancelled}}<p class="text-warning">Run was cancelled</p>{{end}}<form method="post" action="/repositories/{{.RepositoryID}}/executor/retry-review"><button class="btn btn-outline-primary">Retry pull request review</button></form>{{else if .Intake.ExecutorCompleted}}`
	t, err := template.New("page").Parse(strings.Replace(pageTemplate, `{{else if .Intake.ExecutorCompleted}}`, executorControls, 1))
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &application{deps: deps, db: db, templates: t, ctx: ctx, cancel: cancel}
	if err := a.recoverIntakes(); err != nil {
		cancel()
		_ = db.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.root)
	mux.HandleFunc("GET /repositories", a.repositories)
	mux.HandleFunc("POST /repositories/{id}", a.selectRepository)
	mux.HandleFunc("GET /repositories/{id}", a.workspace)
	mux.HandleFunc("POST /repositories/{id}/conversation", a.converse)
	mux.HandleFunc("GET /repositories/{id}/conversation/{turn}/stream", a.stream)
	mux.HandleFunc("POST /repositories/{id}/intake/start", a.startIntake)
	mux.HandleFunc("POST /repositories/{id}/intake/synthesize", a.synthesizeIntake)
	mux.HandleFunc("POST /repositories/{id}/intake/complete-discovery", a.completeDiscovery)
	mux.HandleFunc("POST /repositories/{id}/intake/adr", a.proposeADR)
	mux.HandleFunc("POST /repositories/{id}/intake/{artifact}/confirm", a.confirmArtifact)
	mux.HandleFunc("POST /repositories/{id}/intake/publish", a.publishIntake)
	mux.HandleFunc("POST /repositories/{id}/intake/abandon", a.abandonIntake)
	mux.HandleFunc("POST /repositories/{id}/executor/select", a.executorSelect)
	mux.HandleFunc("POST /repositories/{id}/executor/plan", a.executorPlan)
	mux.HandleFunc("POST /repositories/{id}/executor/approve", a.executorApprove)
	mux.HandleFunc("POST /repositories/{id}/executor/run", a.executorRun)
	mux.HandleFunc("POST /repositories/{id}/executor/create-pr", a.executorCreatePR)
	mux.HandleFunc("POST /repositories/{id}/executor/review", a.executorReview)
	mux.HandleFunc("POST /repositories/{id}/executor/approve-merge", a.executorApproveMerge)
	mux.HandleFunc("POST /repositories/{id}/executor/merge", a.executorMerge)
	mux.HandleFunc("POST /repositories/{id}/executor/cleanup", a.executorCleanup)
	mux.HandleFunc("POST /repositories/{id}/executor/fix", a.executorFix)
	mux.HandleFunc("POST /repositories/{id}/executor/cancel", a.executorCancel)
	mux.HandleFunc("POST /repositories/{id}/executor/retry-pr", a.executorRetryPR)
	mux.HandleFunc("POST /repositories/{id}/executor/retry-review", a.executorRetryReview)
	mux.HandleFunc("POST /repositories/{id}/executor/retry-merge", a.executorRetryMerge)
	mux.HandleFunc("POST /repositories/{id}/executor/retry-cleanup", a.executorRetryCleanup)
	mux.HandleFunc("POST /notifications/test", a.testNotification)
	a.handler = a.authorized(mux)
	if err := a.resumePendingTurns(); err != nil {
		cancel()
		_ = db.Close()
		return nil, err
	}
	return a, nil
}

// ReconcileStartup repairs intake state after a process crash. GitHub is the
// authority for PR state; the lease scan is intentionally independent so a
// crash before run_id was persisted is recoverable too.
func (a *application) ReconcileStartup(ctx context.Context, prs PRStateReader) error {
	rows, err := a.db.QueryContext(ctx, `SELECT i.repository_id,r.full_name,COALESCE(i.pr_number,0),i.executor_state,i.run_id FROM intakes i JOIN repositories r ON r.id=i.repository_id WHERE i.executor_state IN ('creating_pr','pr_created','reviewing','review_blocked','fixing','merge_ready','merge_approved','merging','merged','failed','cleanup_done')`)
	if err != nil {
		return fmt.Errorf("startup reconciliation: query intakes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, fullName, runID, state string
		var number int
		if err := rows.Scan(&id, &fullName, &number, &state, &runID); err != nil {
			return err
		}
		if (state == string(executorFailed) || state == string(executorCleanupDone)) && runID != "" && a.deps.RunLease != nil {
			leaseState := "failed"
			if state == string(executorCleanupDone) {
				leaseState = "completed"
			}
			_ = a.deps.RunLease.Release(ctx, runID, leaseState, "startup terminal reconciliation")
			continue
		}
		if state == string(executorCreatingPR) {
			if _, err := a.db.ExecContext(ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorFailed, time.Now().UTC().Format(time.RFC3339Nano), id, executorCreatingPR); err != nil {
				return err
			}
			if runID != "" && a.deps.RunLease != nil {
				_ = a.deps.RunLease.Release(ctx, runID, string(executorFailed), "startup recovery during pull request creation")
			}
			continue
		}
		if prs == nil || number == 0 {
			continue
		}
		remote, err := prs.State(ctx, Repository{ID: id, FullName: fullName}, number)
		if err != nil {
			continue
		}
		next := executorState(state)
		switch remote {
		case "MERGED":
			next = executorMerged
		case "CLOSED":
			next = executorFailed
		case "OPEN":
			switch next {
			case executorPRCreated, executorReviewing:
				next = executorReviewing
			case executorFixing:
				next = executorReviewBlocked
			}
		}
		if next != executorState(state) {
			if _, err := a.db.ExecContext(ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, next, time.Now().UTC().Format(time.RFC3339Nano), id, state); err != nil {
				return err
			}
		}
		if next == executorFailed && runID != "" && a.deps.RunLease != nil {
			_ = a.deps.RunLease.Release(ctx, runID, string(next), "startup reconciliation")
		}
		if next == executorMerged {
			if err := a.cleanupMerged(ctx, id); err != nil {
				return fmt.Errorf("startup reconciliation: cleanup merged pull request: %w", err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if lister, ok := a.deps.RunLease.(ActiveRunLister); ok {
		active, err := lister.ListActive(ctx)
		if err != nil {
			return err
		}
		for _, run := range active {
			var found int
			if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM intakes WHERE run_id=?`, run.RunID).Scan(&found); err != nil {
				return err
			}
			if found == 0 {
				_ = a.deps.RunLease.Release(ctx, run.RunID, string(executorFailed), "orphaned lease recovered")
			}
		}
	}
	return nil
}

// CleanupExpiredFailedWorkspaces removes only failed or cancelled implementation
// clones whose age exceeds retention. It leaves all database history intact.
func (a *application) CleanupExpiredFailedWorkspaces(ctx context.Context, retention time.Duration) error {
	if a.deps.IssueWorkspace == nil {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT executor_workspace_path FROM intakes WHERE executor_workspace_path != '' AND (executor_state=? OR executor_cancelled=1)`, executorFailed)
	if err != nil {
		return fmt.Errorf("query expired failed workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return err
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) || err != nil || info.ModTime().After(time.Now().Add(-retention)) {
			continue
		}
		if err := a.deps.IssueWorkspace.Cleanup(ctx, path); err != nil {
			return fmt.Errorf("cleanup expired failed workspace %q: %w", path, err)
		}
	}
	return nil
}

// recoverIntakes completes only local, already-authorized work after a
// restart. It never publishes new GitHub issues: publication retry remains the
// idempotent publisher's responsibility.
func (a *application) recoverIntakes() error {
	rows, err := a.db.Query(`SELECT repository_id,state,clone_path,issue_number,issue_url FROM intakes WHERE state IN (?,?,?)`, intakePublishing, intakePromoting, intakeAbandoning)
	if err != nil {
		return fmt.Errorf("list interrupted intakes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, path, issueURL string
		var state intakeState
		var issueNumber sql.NullInt64
		if err := rows.Scan(&id, &state, &path, &issueNumber, &issueURL); err != nil {
			return fmt.Errorf("scan interrupted intake: %w", err)
		}
		if state == intakeAbandoning {
			if a.deps.Intake != nil && path != "" {
				if err := a.deps.Intake.Cleanup(context.Background(), path); err != nil {
					return fmt.Errorf("clean interrupted intake %q: %w", id, err)
				}
			}
			if _, err := a.db.Exec(`UPDATE intakes SET state=?,clone_path='',updated_at=? WHERE repository_id=? AND state=?`, intakeAbandoned, time.Now().UTC().Format(time.RFC3339Nano), id, intakeAbandoning); err != nil {
				return fmt.Errorf("record abandoned intake recovery %q: %w", id, err)
			}
			continue
		}
		if state == intakePublishing {
			tickets, artifactErr := a.artifact(context.Background(), id, artifactTickets)
			if errors.Is(artifactErr, sql.ErrNoRows) {
				continue
			}
			if artifactErr != nil {
				return fmt.Errorf("load ticket set for intake %q: %w", id, artifactErr)
			}
			expected, parseErr := ticketSetPublications(tickets.Body)
			if parseErr != nil {
				return fmt.Errorf("parse ticket set for intake %q: %w", id, parseErr)
			}
			recorded, recordErr := a.publishedIntakeIssues(context.Background(), id)
			if recordErr != nil {
				return fmt.Errorf("load partial publication for intake %q: %w", id, recordErr)
			}
			if len(recorded) < len(expected) {
				continue
			}
			if len(recorded) != len(expected) {
				return fmt.Errorf("intake %q has more recorded issues than confirmed tickets", id)
			}
			first := recorded[0]
			if _, err := a.db.Exec(`UPDATE intakes SET state=?,issue_number=?,issue_url=?,updated_at=? WHERE repository_id=? AND state=?`, intakePromoting, first.Number, first.URL, time.Now().UTC().Format(time.RFC3339Nano), id, intakePublishing); err != nil {
				return fmt.Errorf("reconcile partial publication for intake %q: %w", id, err)
			}
			state = intakePromoting
			issueNumber = sql.NullInt64{Int64: int64(first.Number), Valid: true}
			issueURL = first.URL
		}
		if !issueNumber.Valid || issueNumber.Int64 < 1 || issueURL == "" {
			return fmt.Errorf("promoting intake %q lacks published issue details", id)
		}
		promotedPath := path
		if a.deps.Intake != nil && path != "" {
			promotedPath, err = a.deps.Intake.Promote(context.Background(), path, PublishedIssue{Number: int(issueNumber.Int64), URL: issueURL})
			if err != nil {
				return fmt.Errorf("promote interrupted intake %q: %w", id, err)
			}
		}
		if _, err := a.db.Exec(`UPDATE intakes SET state=?,clone_path=?,updated_at=? WHERE repository_id=? AND state=?`, intakePublished, promotedPath, time.Now().UTC().Format(time.RFC3339Nano), id, intakePromoting); err != nil {
			return fmt.Errorf("record promoted intake recovery %q: %w", id, err)
		}
	}
	return rows.Err()
}

func (a *application) root(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/repositories", http.StatusSeeOther)
}

func (a *application) Close() error {
	a.mu.Lock()
	a.closed = true
	a.cancel()
	a.mu.Unlock()
	a.workers.Wait()
	return a.db.Close()
}

func (a *application) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	exists, err := columnExists(db, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate %s columns: %w", table, err)
	}
	return false, nil
}

func recoverDuplicatePendingTurns(db *sql.DB) error {
	if err := resumeInterruptedRunningTurns(db); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT id,repository_id FROM pending_turns WHERE state=? ORDER BY repository_id,rowid DESC`, turnPending)
	if err != nil {
		return fmt.Errorf("list active PM responses for recovery: %w", err)
	}
	defer func() { _ = rows.Close() }()
	retained := make(map[string]bool)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for rows.Next() {
		var turnID, repositoryID string
		if err := rows.Scan(&turnID, &repositoryID); err != nil {
			return fmt.Errorf("scan active PM response for recovery: %w", err)
		}
		if !retained[repositoryID] {
			retained[repositoryID] = true
			continue
		}
		if err := cancelRecoveredTurn(db, repositoryID, turnID, "canceled after duplicate submission recovery", "duplicate PM response canceled during recovery", `<p class="text-danger">This duplicate PM response was canceled during recovery.</p>`, now); err != nil {
			return err
		}
		log.Printf("canceled duplicate PM response for repository %q turn %q during recovery", repositoryID, turnID)
	}
	return rows.Err()
}

func resumeInterruptedRunningTurns(db *sql.DB) error {
	rows, err := db.Query(`SELECT id,repository_id FROM pending_turns WHERE state=?`, turnRunning)
	if err != nil {
		return fmt.Errorf("list interrupted PM responses for recovery: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var turnID, repositoryID string
		if err := rows.Scan(&turnID, &repositoryID); err != nil {
			return fmt.Errorf("scan interrupted PM response for recovery: %w", err)
		}
		if _, err := db.Exec(`UPDATE pending_turns SET state=?,started_at=NULL,completed_at=NULL,terminal_reason=NULL WHERE id=?`, turnPending, turnID); err != nil {
			return fmt.Errorf("resume interrupted PM response %q: %w", turnID, err)
		}
		if _, err := db.Exec(`DELETE FROM turn_events WHERE turn_id=?`, turnID); err != nil {
			return fmt.Errorf("clear interrupted PM response events for %q: %w", turnID, err)
		}
		if _, err := db.Exec(`INSERT INTO activity VALUES(?,?,?)`, repositoryID, "interrupted PM response resumed after service restart", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record interrupted PM response recovery: %w", err)
		}
		log.Printf("resuming interrupted PM response for repository %q turn %q after service restart", repositoryID, turnID)
	}
	return rows.Err()
}

func cancelRecoveredTurn(db *sql.DB, repositoryID, turnID, reason, activity, event, now string) error {
	if _, err := db.Exec(`UPDATE pending_turns SET state=?,terminal_reason=?,completed_at=? WHERE id=?`, turnCanceled, reason, now, turnID); err != nil {
		return fmt.Errorf("cancel PM response %q: %w", turnID, err)
	}
	if _, err := db.Exec(`INSERT INTO activity VALUES(?,?,?)`, repositoryID, activity, now); err != nil {
		return fmt.Errorf("record PM response recovery: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO turn_events(turn_id,event,data) VALUES(?,?,?)`, turnID, "error", event); err != nil {
		return fmt.Errorf("record PM response event: %w", err)
	}
	return nil
}

func (a *application) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := strings.ToLower(r.Header.Get("X-PM-User"))
		if user == "" || !a.deps.AllowedUsers[user] {
			http.Error(w, "operator access required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *application) repositories(w http.ResponseWriter, r *http.Request) {
	repos, err := a.deps.GitHub.Repositories(r.Context())
	if err != nil {
		http.Error(w, "GitHub repositories unavailable", http.StatusBadGateway)
		return
	}
	for _, repo := range repos {
		if _, err := a.db.ExecContext(r.Context(), `INSERT INTO repositories(id,full_name) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET full_name=excluded.full_name`, repo.ID, repo.FullName); err != nil {
			http.Error(w, "could not persist GitHub repositories", http.StatusInternalServerError)
			return
		}
	}
	a.render(w, "repositories", repos)
}
func (a *application) selectRepository(w http.ResponseWriter, r *http.Request) {
	var exists string
	if err := a.db.QueryRowContext(r.Context(), `SELECT id FROM repositories WHERE id=?`, r.PathValue("id")).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not register repository", http.StatusInternalServerError)
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO genkit_sessions(repository_id,session_id,updated_at) VALUES(?,?,?) ON CONFLICT(repository_id) DO NOTHING`, r.PathValue("id"), "pm-"+r.PathValue("id"), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "could not register repository", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/repositories/"+url.PathEscape(r.PathValue("id")), http.StatusSeeOther)
}
func (a *application) workspace(w http.ResponseWriter, r *http.Request) {
	c, err := a.conversation(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if model, ok := a.deps.Model.(StatusModel); ok {
		c.Status, err = model.Status(r.Context(), c.RepositoryID)
		if err != nil {
			log.Printf("load PM status for repository %q: %s", c.RepositoryID, redactSecrets(err.Error()))
			http.Error(w, "could not load PM status", http.StatusInternalServerError)
			return
		}
	}
	intake, err := a.intakeStatus(r.Context(), c.RepositoryID)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	artifacts, err := a.artifacts(r.Context(), c.RepositoryID)
	if err != nil {
		http.Error(w, "could not load intake artifacts", http.StatusInternalServerError)
		return
	}
	for _, issue := range intake.PublishedIssues {
		artifacts = append(artifacts, Artifact{Kind: artifactKind(fmt.Sprintf("GitHub issue #%d", issue.Number)), URL: issue.URL})
	}
	var selection *ExecutorSelection
	if intake.ExecutorKind != "" {
		selection = &ExecutorSelection{Kind: ExecutorKind(intake.ExecutorKind), Rationale: intake.ExecutorRationale}
	}
	a.render(w, "workspace", Workspace{Conversation: c, Intake: intake, Artifacts: artifacts, ExecutorSelection: selection})
}
func (a *application) converse(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	prompt := redactSecrets(strings.TrimSpace(r.Form.Get("message")))
	if prompt == "" {
		http.Error(w, "message required", 400)
		return
	}
	id := r.PathValue("id")
	intake, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if intake.State != "" && intake.State != intakeDraft {
		http.Error(w, "discovery is complete; synthesize or publish the existing intake", http.StatusConflict)
		return
	}
	c, err := a.conversation(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, streaming := a.deps.Model.(StreamingModel); streaming && r.Header.Get("HX-Request") == "true" {
		turnID := uuid.NewString()
		if err = a.beginStreamingTurn(r.Context(), id, turnID, prompt); err != nil {
			if errors.Is(err, errTurnInProgress) {
				http.Error(w, "A PM response is already in progress.", http.StatusConflict)
				return
			}
			http.Error(w, "could not start response stream", http.StatusInternalServerError)
			return
		}
		log.Printf("start PM response for repository %q turn %q", id, turnID)
		a.startTurn(id, turnID, prompt)
		a.render(w, "stream-start", PendingTurn{RepositoryID: id, TurnID: turnID, Sending: true})
		return
	}
	now := time.Now().UTC()
	if _, err = a.db.ExecContext(r.Context(), `INSERT INTO messages(repository_id,role,text,created_at) VALUES(?,?,?,?)`, id, "operator", prompt, now.Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "could not save message", 500)
		return
	}
	c.Messages = append(c.Messages, Message{Role: "operator", Text: prompt, CreatedAt: now})
	reply, err := a.reply(r.Context(), c, prompt, nil)
	if err != nil {
		http.Error(w, "model response unavailable", http.StatusBadGateway)
		return
	}
	status, err := a.completeTurn(r.Context(), id, reply)
	if err != nil {
		http.Error(w, "could not save response", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/repositories/"+url.PathEscape(id), http.StatusSeeOther)
		return
	}
	a.render(w, "turn", struct {
		Reply  Reply
		Status Status
	}{reply, status})
}

func (a *application) beginStreamingTurn(ctx context.Context, repositoryID, turnID, prompt string) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()

	var activeTurn string
	err := a.db.QueryRowContext(ctx, `SELECT id FROM pending_turns WHERE repository_id=? AND state IN ('pending','running') LIMIT 1`, repositoryID).Scan(&activeTurn)
	if err == nil {
		return errTurnInProgress
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check pending PM response: %w", err)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin PM response: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages(repository_id,role,text,created_at) VALUES(?,?,?,?)`, repositoryID, "operator", prompt, now); err != nil {
		return fmt.Errorf("save operator message: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO pending_turns(id,repository_id,prompt) VALUES(?,?,?)`, turnID, repositoryID, prompt); err != nil {
		return fmt.Errorf("save pending PM response: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit PM response: %w", err)
	}
	return nil
}

func (a *application) resumePendingTurns() error {
	rows, err := a.db.Query(`SELECT id,repository_id,prompt FROM pending_turns WHERE state=?`, turnPending)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var turnID, repositoryID, prompt string
		if err := rows.Scan(&turnID, &repositoryID, &prompt); err != nil {
			return err
		}
		a.startTurn(repositoryID, turnID, prompt)
	}
	return rows.Err()
}

func (a *application) startTurn(repositoryID, turnID, prompt string) {
	result, err := a.db.Exec(`UPDATE pending_turns SET started_at=?,state=? WHERE id=? AND state=?`, time.Now().UTC().Format(time.RFC3339Nano), turnRunning, turnID, turnPending)
	if err != nil {
		return
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		a.runTurn(a.ctx, repositoryID, turnID, prompt)
	}()
}

func (a *application) stream(w http.ResponseWriter, r *http.Request) {
	id, turnID := r.PathValue("id"), r.PathValue("turn")
	var repositoryID string
	var state turnState
	err := a.db.QueryRowContext(r.Context(), `SELECT repository_id,state FROM pending_turns WHERE id=?`, turnID).Scan(&repositoryID, &state)
	if errors.Is(err, sql.ErrNoRows) || repositoryID != id {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load response stream", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flush, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	lastID := int64(0)
	for {
		rows, err := a.db.QueryContext(r.Context(), `SELECT id,event,data FROM turn_events WHERE turn_id=? AND id>? ORDER BY id`, turnID, lastID)
		if err != nil {
			return
		}
		for rows.Next() {
			var event, data string
			if err := rows.Scan(&lastID, &event, &data); err != nil {
				_ = rows.Close()
				return
			}
			a.writeEvent(w, event, data)
			flush.Flush()
		}
		if err := rows.Close(); err != nil {
			return
		}
		if err := a.db.QueryRowContext(r.Context(), `SELECT state FROM pending_turns WHERE id=?`, turnID).Scan(&state); err != nil || (state != turnPending && state != turnRunning) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (a *application) runTurn(ctx context.Context, repositoryID, turnID, prompt string) {
	c, err := a.conversation(ctx, repositoryID)
	if err == nil {
		var text strings.Builder
		var visibleText strings.Builder
		var safe streamTextBuffer
		var reply Reply
		reply, err = a.reply(ctx, c, prompt, func(chunk string) error {
			text.WriteString(chunk)
			return safe.Append(chunk, func(visible string) error {
				visibleText.WriteString(visible)
				return a.recordTurnEvent(ctx, turnID, "chunk", `<strong>pm:</strong> `+template.HTMLEscapeString(visibleText.String()))
			})
		})
		if flushErr := safe.Flush(func(visible string) error {
			visibleText.WriteString(visible)
			return a.recordTurnEvent(ctx, turnID, "chunk", `<strong>pm:</strong> `+template.HTMLEscapeString(visibleText.String()))
		}); err == nil && flushErr != nil {
			err = flushErr
		}
		if err == nil && reply.Text == "" {
			reply.Text = text.String()
		}
		var status Status
		if err == nil {
			status, err = a.completeTurn(ctx, repositoryID, reply)
		}
		if err == nil {
			var fragment string
			fragment, err = a.fragment("streamed-turn", struct {
				Reply  Reply
				Status Status
				TurnID string
			}{reply, status, turnID})
			if err == nil {
				err = a.recordTurnEvent(ctx, turnID, "done", fragment)
			}
		}
	}
	state, reason := turnCompleted, ""
	if err != nil {
		state, reason = turnFailed, "PM response could not be completed"
		log.Printf("complete PM response for repository %q turn %q: %s", repositoryID, turnID, redactSecrets(err.Error()))
		_ = a.recordTurnEvent(ctx, turnID, "error", `<p class="text-danger">The PM response could not be completed.</p><button id="pm-send" class="btn btn-primary" hx-swap-oob="outerHTML">Send</button>`)
	} else {
		log.Printf("completed PM response for repository %q turn %q", repositoryID, turnID)
	}
	if _, updateErr := a.db.ExecContext(ctx, `UPDATE pending_turns SET state=?,terminal_reason=?,completed_at=? WHERE id=?`, state, reason, time.Now().UTC().Format(time.RFC3339Nano), turnID); updateErr != nil {
		log.Printf("record PM response terminal state for repository %q turn %q: %s", repositoryID, turnID, redactSecrets(updateErr.Error()))
	}
}

func (a *application) recordTurnEvent(ctx context.Context, turnID, event, data string) error {
	_, err := a.db.ExecContext(ctx, `INSERT INTO turn_events(turn_id,event,data) VALUES(?,?,?)`, turnID, event, data)
	return err
}

func (a *application) reply(ctx context.Context, c Conversation, prompt string, emit func(string) error) (Reply, error) {
	if stream, ok := a.deps.Model.(StreamingModel); ok {
		var text strings.Builder
		var question singleQuestionStream
		reply, err := stream.Stream(ctx, c, prompt, func(chunk string) error {
			chunk = question.write(chunk)
			if chunk == "" {
				return nil
			}
			text.WriteString(chunk)
			if emit == nil {
				return nil
			}
			return emit(chunk)
		})
		if err == nil && reply.Text == "" {
			reply.Text = text.String()
		}
		reply.Text = discoveryReply(redactSecrets(reply.Text))
		return reply, err
	}
	reply, err := a.deps.Model.Reply(ctx, c, prompt)
	reply.Text = discoveryReply(redactSecrets(reply.Text))
	return reply, err
}

type singleQuestionStream struct{ complete bool }

func (s *singleQuestionStream) write(chunk string) string {
	if s.complete {
		return ""
	}
	if end := strings.Index(chunk, "?"); end >= 0 {
		s.complete = true
		return chunk[:end+1]
	}
	return chunk
}

// discoveryReply makes the workflow boundary observable and durable: a turn
// may contain context, but it can leave the operator with only one question.
func discoveryReply(reply string) string {
	first := strings.Index(reply, "?")
	if first < 0 {
		return strings.TrimSpace(reply)
	}
	return strings.TrimSpace(reply[:first+1])
}

func (a *application) completeTurn(ctx context.Context, id string, reply Reply) (Status, error) {
	reply.Text = redactSecrets(reply.Text)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `INSERT INTO messages(repository_id,role,text,tokens,cost_usd,created_at) VALUES(?,?,?,?,?,?)`, id, "pm", reply.Text, reply.Tokens, reply.CostUSD, now); err != nil {
		return Status{}, err
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO activity VALUES(?,?,?)`, id, "discovery turn completed", now); err != nil {
		return Status{}, err
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE intakes SET pending_question=?,updated_at=? WHERE repository_id=? AND state IN (?,?)`, nextDiscoveryQuestion(reply.Text), now, id, intakeDraft, intakeConfirmed); err != nil {
		return Status{}, err
	}
	status := Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "0s", RecentActivity: "discovery turn completed"}
	if model, ok := a.deps.Model.(StatusModel); ok {
		return model.Status(ctx, id)
	}
	return status, nil
}

func nextDiscoveryQuestion(reply string) string {
	questionEnd := strings.Index(reply, "?")
	if questionEnd >= 0 {
		question := strings.TrimSpace(reply[:questionEnd+1])
		if question != "?" {
			return question
		}
	}
	return "What detail should we resolve next?"
}

func (a *application) fragment(name string, data any) (string, error) {
	var out strings.Builder
	if err := a.templates.ExecuteTemplate(&out, name, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

func (a *application) writeEvent(w http.ResponseWriter, event, data string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, strings.ReplaceAll(data, "\n", "&#10;"))
}

func redactSecrets(value string) string { return redaction.Secrets(value) }

type streamTextBuffer struct{ pending string }

func (b *streamTextBuffer) Append(chunk string, emit func(string) error) error {
	b.pending += chunk
	boundary := strings.LastIndexAny(b.pending, " \t\r\n")
	if boundary < 0 {
		return nil
	}
	return b.emit(b.pending[:boundary+1], emit)
}

func (b *streamTextBuffer) Flush(emit func(string) error) error { return b.emit(b.pending, emit) }

func (b *streamTextBuffer) emit(value string, emit func(string) error) error {
	if value == "" {
		return nil
	}
	b.pending = strings.TrimPrefix(b.pending, value)
	return emit(redactSecrets(value))
}
func (a *application) testNotification(w http.ResponseWriter, r *http.Request) {
	if a.deps.Telegram != nil {
		base := strings.TrimRight(a.deps.DashboardURL, "/")
		if base == "" {
			base = "http://localhost:8080"
		}
		notification := Notification{Text: "PM dashboard test notification.", URL: base + "/repositories"}
		if err := a.deps.Telegram.Notify(r.Context(), notification); err != nil {
			log.Printf("send PM test notification: %s", redactSecrets(err.Error()))
			http.Error(w, "Telegram unavailable", http.StatusBadGateway)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *application) startIntake(w http.ResponseWriter, r *http.Request) {
	repo, err := a.repository(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	status, err := a.intakeStatus(r.Context(), repo.ID)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if status.State != "" && status.State != intakeAbandoned {
		http.Error(w, "an intake is already active for this repository", http.StatusConflict)
		return
	}
	path := ""
	persisted := false
	defer func() {
		if !persisted && a.deps.Intake != nil && path != "" {
			if err := a.deps.Intake.Cleanup(r.Context(), path); err != nil {
				log.Printf("clean unpersisted intake for repository %q: %s", repo.ID, redactSecrets(err.Error()))
			}
		}
	}()
	if a.deps.Intake != nil {
		path, err = a.deps.Intake.Start(r.Context(), repo)
		if err != nil {
			log.Printf("start isolated intake for repository %q: %s", repo.ID, redactSecrets(err.Error()))
			http.Error(w, "could not start isolated intake", http.StatusBadGateway)
			return
		}
	}
	inspection := ""
	if inspector, ok := a.deps.Intake.(Inspector); ok && path != "" {
		inspection, err = inspector.Inspect(r.Context(), path)
		if err != nil {
			log.Printf("inspect isolated intake for repository %q: %s", repo.ID, redactSecrets(err.Error()))
			http.Error(w, "could not inspect isolated intake", http.StatusBadGateway)
			return
		}
	}
	inspection = redactSecrets(inspection)
	var messageStart int64
	if err = a.db.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(rowid), 0) FROM messages WHERE repository_id=?`, repo.ID).Scan(&messageStart); err != nil {
		http.Error(w, "could not prepare intake", http.StatusInternalServerError)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "could not persist intake", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()
	initialQuestion := "What outcome should this work deliver?"
	intakeID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO intakes(repository_id,intake_id,state,clone_path,inspection,message_start,pending_question,issue_number,issue_url,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(repository_id) DO UPDATE SET intake_id=excluded.intake_id,state=excluded.state,clone_path=excluded.clone_path,inspection=excluded.inspection,message_start=excluded.message_start,pending_question=excluded.pending_question,issue_number=NULL,issue_url='',updated_at=excluded.updated_at`, repo.ID, intakeID, intakeDraft, path, inspection, messageStart, initialQuestion, nil, "", now); err != nil {
		http.Error(w, "could not persist intake", http.StatusInternalServerError)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO messages(repository_id,role,text,created_at) VALUES(?,?,?,?)`, repo.ID, "pm", initialQuestion, now); err != nil {
		http.Error(w, "could not persist intake", http.StatusInternalServerError)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `DELETE FROM intake_artifacts WHERE repository_id=?`, repo.ID); err != nil {
		http.Error(w, "could not discard previous intake drafts", http.StatusInternalServerError)
		return
	}
	if inspection != "" {
		body := "# Repository evidence\n\nThe PM resolved repository-answerable discovery facts from this isolated, read-only inspection instead of asking the operator to repeat them.\n\n" + inspection
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO intake_artifacts(repository_id,kind,body,created_at) VALUES(?,?,?,?)`, repo.ID, artifactRepositoryEvidence, body, now); err != nil {
			http.Error(w, "could not persist repository evidence", http.StatusInternalServerError)
			return
		}
	}
	if _, err = tx.ExecContext(r.Context(), `DELETE FROM intake_issues WHERE repository_id=?`, repo.ID); err != nil {
		http.Error(w, "could not discard previous intake publications", http.StatusInternalServerError)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "could not persist intake", http.StatusInternalServerError)
		return
	}
	persisted = true
	a.redirectWorkspace(w, r, repo.ID)
}

// synthesizeIntake turns already-settled conversation into inspectable drafts;
// it deliberately does not send another discovery question or mutate GitHub.
func (a *application) synthesizeIntake(w http.ResponseWriter, r *http.Request) {
	repo, err := a.repository(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	status, err := a.intakeStatus(r.Context(), repo.ID)
	if err != nil || status.State != intakeReady {
		http.Error(w, "complete discovery before synthesizing artifacts", http.StatusConflict)
		return
	}
	conversation, err := a.conversationAfter(r.Context(), repo.ID, status.MessageStart)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !discoveryComplete(conversation) {
		http.Error(w, "complete one focused discovery exchange before synthesizing artifacts", http.StatusConflict)
		return
	}
	artifacts, err := a.synthesizeArtifacts(r.Context(), repo, conversation)
	if err != nil {
		http.Error(w, "could not synthesize artifacts", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "could not create drafts", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()
	for _, artifact := range artifacts {
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO intake_artifacts(repository_id,kind,body,confirmed_at,url,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(repository_id,kind) DO UPDATE SET body=excluded.body,confirmed_at=NULL,url='',created_at=excluded.created_at`, repo.ID, artifact.Kind, artifact.Body, nil, "", now); err != nil {
			http.Error(w, "could not persist drafts", http.StatusInternalServerError)
			return
		}
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE intakes SET state=?,updated_at=? WHERE repository_id=?`, intakeDraft, now, repo.ID); err != nil {
		http.Error(w, "could not update intake", http.StatusInternalServerError)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "could not save drafts", http.StatusInternalServerError)
		return
	}
	if updater, ok := a.deps.Intake.(ContextUpdater); ok && status.Path != "" {
		for _, artifact := range artifacts {
			if artifact.Kind == artifactGlossary {
				if err = updater.UpdateContext(r.Context(), status.Path, artifact.Body); err != nil {
					if revertErr := a.revertSynthesis(r.Context(), repo.ID); revertErr != nil {
						log.Printf("revert failed intake synthesis for repository %q: %s", repo.ID, redactSecrets(revertErr.Error()))
					}
					log.Printf("update intake context for repository %q: %s", repo.ID, redactSecrets(err.Error()))
					http.Error(w, "could not update intake glossary", http.StatusInternalServerError)
					return
				}
				break
			}
		}
	}
	a.redirectWorkspace(w, r, repo.ID)
}

func (a *application) revertSynthesis(ctx context.Context, id string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_artifacts WHERE repository_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE intakes SET state=?,updated_at=? WHERE repository_id=? AND state=?`, intakeReady, time.Now().UTC().Format(time.RFC3339Nano), id, intakeDraft); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *application) completeDiscovery(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.repository(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil || status.State != intakeDraft {
		http.Error(w, "an active discovery is required", http.StatusConflict)
		return
	}
	conversation, err := a.conversationAfter(r.Context(), id, status.MessageStart)
	if err != nil || !discoveryComplete(conversation) {
		http.Error(w, "complete one focused discovery exchange first", http.StatusConflict)
		return
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET state=?,pending_question='',updated_at=? WHERE repository_id=? AND state=?`, intakeReady, time.Now().UTC().Format(time.RFC3339Nano), id, intakeDraft)
	if err != nil {
		http.Error(w, "could not complete discovery", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "discovery state changed before completion", http.StatusConflict)
		return
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) confirmArtifact(w http.ResponseWriter, r *http.Request) {
	id, kind := r.PathValue("id"), artifactKind(r.PathValue("artifact"))
	if !requiresConfirmation(kind) {
		http.NotFound(w, r)
		return
	}
	if _, err := a.repository(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "could not confirm artifact", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(r.Context(), `UPDATE intake_artifacts SET confirmed_at=? WHERE repository_id=? AND kind=? AND EXISTS (SELECT 1 FROM intakes WHERE repository_id=? AND state=?)`, time.Now().UTC().Format(time.RFC3339Nano), id, kind, id, intakeDraft)
	if err != nil {
		http.Error(w, "could not confirm artifact", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "artifact draft required before confirmation", http.StatusConflict)
		return
	}
	allConfirmed, err := allRequiredArtifactsConfirmed(r.Context(), tx, id)
	if err != nil {
		http.Error(w, "could not confirm artifact", http.StatusInternalServerError)
		return
	}
	if allConfirmed {
		if _, err = tx.ExecContext(r.Context(), `UPDATE intakes SET state=?,updated_at=? WHERE repository_id=? AND state=?`, intakeConfirmed, time.Now().UTC().Format(time.RFC3339Nano), id, intakeDraft); err != nil {
			http.Error(w, "could not record intake confirmation", http.StatusInternalServerError)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "could not record intake confirmation", http.StatusInternalServerError)
		return
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) proposeADR(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "ADR proposals are created only by the PM eligibility assessment", http.StatusUnprocessableEntity)
}

func (a *application) publishIntake(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	repo, err := a.repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if a.deps.Publisher == nil {
		http.Error(w, "GitHub issue publisher is not configured", http.StatusServiceUnavailable)
		return
	}
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if status.State == intakePromoting {
		a.promotePublishedIntake(w, r, id, status)
		return
	}
	if status.State != intakeConfirmed && status.State != intakePublishing {
		http.Error(w, "a confirmed intake is required before publishing", http.StatusConflict)
		return
	}
	allConfirmed, err := allRequiredArtifactsConfirmed(r.Context(), a.db, id)
	if err != nil || !allConfirmed {
		http.Error(w, "a confirmed intake is required before publishing", http.StatusConflict)
		return
	}
	if status.State == intakeConfirmed {
		claimed, claimErr := a.claimPublication(r.Context(), id)
		if claimErr != nil {
			http.Error(w, "could not claim intake publication", http.StatusInternalServerError)
			return
		}
		if !claimed {
			http.Error(w, "intake publication is already in progress", http.StatusConflict)
			return
		}
	}
	spec, err := a.artifact(r.Context(), id, artifactSpec)
	if err != nil {
		_ = a.returnPublicationToConfirmed(r.Context(), id)
		http.Error(w, "specification draft unavailable", http.StatusConflict)
		return
	}
	tickets, err := a.artifact(r.Context(), id, artifactTickets)
	if err != nil {
		_ = a.returnPublicationToConfirmed(r.Context(), id)
		http.Error(w, "ticket draft unavailable", http.StatusConflict)
		return
	}
	publications, err := ticketSetPublications(tickets.Body)
	if err != nil {
		_ = a.returnPublicationToConfirmed(r.Context(), id)
		http.Error(w, "ticket draft is not publishable", http.StatusConflict)
		return
	}
	for _, publication := range publications {
		if err := validateEnglishPublication(publication); err != nil {
			_ = a.returnPublicationToConfirmed(r.Context(), id)
			http.Error(w, "confirmed GitHub tickets must be written in English", http.StatusUnprocessableEntity)
			return
		}
	}
	published, err := a.publishedIntakeIssues(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load published tickets", http.StatusInternalServerError)
		return
	}
	for index, publication := range publications {
		if index < len(published) {
			continue
		}
		resolvedBlockers := make([]string, 0, len(publication.BlockedBy))
		for _, blocker := range publication.BlockedBy {
			if blocker < 1 || blocker > len(published) {
				_ = a.returnPublicationToConfirmed(r.Context(), id)
				http.Error(w, "ticket blocker was not published", http.StatusConflict)
				return
			}
			resolvedBlockers = append(resolvedBlockers, "#"+strconv.Itoa(published[blocker-1].Number))
		}
		publication.Body = ticketBlockers.ReplaceAllString(publication.Body, "Blocked by: "+strings.Join(resolvedBlockers, ", "))
		publication.BlockedBy = nil
		publication.Key = fmt.Sprintf("%s-%d", status.ID, index+1)
		publication.Body = "## Confirmed specification\n\n" + spec.Body + "\n\n## Confirmed ticket\n\n" + publication.Body
		issues, publishErr := a.deps.Publisher.Publish(r.Context(), repo, []Publication{publication})
		if len(issues) == 1 {
			if err = a.recordPublishedTicket(r.Context(), id, index+1, issues[0]); err != nil {
				http.Error(w, "ticket was published but could not be recorded", http.StatusInternalServerError)
				return
			}
			published = append(published, issues[0])
		}
		if publishErr != nil || len(issues) != 1 {
			_ = a.returnPublicationToConfirmed(r.Context(), id)
			if publishErr != nil {
				log.Printf("publish intake for repository %q: %s", id, redactSecrets(publishErr.Error()))
			}
			http.Error(w, "could not publish GitHub tickets", http.StatusBadGateway)
			return
		}
	}
	issues := published
	if err = a.recordPublishedIssues(r.Context(), id, issues); err != nil {
		http.Error(w, "ticket was published but could not be recorded", http.StatusInternalServerError)
		return
	}
	status.PublishedIssue = issues[0]
	status.State = intakePromoting
	a.promotePublishedIntake(w, r, id, status)
}

func (a *application) recordPublishedTicket(ctx context.Context, id string, ticketIndex int, issue PublishedIssue) error {
	_, err := a.db.ExecContext(ctx, `INSERT INTO intake_issues(repository_id,ticket_index,issue_number,issue_url) VALUES(?,?,?,?) ON CONFLICT(repository_id,ticket_index) DO NOTHING`, id, ticketIndex, issue.Number, issue.URL)
	return err
}

func (a *application) publishedIntakeIssues(ctx context.Context, id string) ([]PublishedIssue, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT issue_number,issue_url FROM intake_issues WHERE repository_id=? ORDER BY ticket_index`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var issues []PublishedIssue
	for rows.Next() {
		var issue PublishedIssue
		if err := rows.Scan(&issue.Number, &issue.URL); err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

func (a *application) claimPublication(ctx context.Context, id string) (bool, error) {
	result, err := a.db.ExecContext(ctx, `UPDATE intakes SET state=?,updated_at=? WHERE repository_id=? AND state=?`, intakePublishing, time.Now().UTC().Format(time.RFC3339Nano), id, intakeConfirmed)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (a *application) returnPublicationToConfirmed(ctx context.Context, id string) error {
	_, err := a.db.ExecContext(ctx, `UPDATE intakes SET state=?,updated_at=? WHERE repository_id=? AND state=?`, intakeConfirmed, time.Now().UTC().Format(time.RFC3339Nano), id, intakePublishing)
	return err
}

func (a *application) recordPublishedIssues(ctx context.Context, id string, issues []PublishedIssue) error {
	if len(issues) == 0 {
		return errors.New("at least one published issue is required")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE intakes SET state=?,issue_number=?,issue_url=?,updated_at=? WHERE repository_id=? AND state=?`, intakePromoting, issues[0].Number, issues[0].URL, time.Now().UTC().Format(time.RFC3339Nano), id, intakePublishing)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("intake publication state changed before the issue could be recorded")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE intake_artifacts SET url=? WHERE repository_id=? AND kind=?`, issues[0].URL, id, artifactTickets); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_issues WHERE repository_id=?`, id); err != nil {
		return err
	}
	for index, issue := range issues {
		if _, err = tx.ExecContext(ctx, `INSERT INTO intake_issues(repository_id,ticket_index,issue_number,issue_url) VALUES(?,?,?,?)`, id, index+1, issue.Number, issue.URL); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *application) promotePublishedIntake(w http.ResponseWriter, r *http.Request, id string, status IntakeStatus) {
	if status.PublishedIssue.Number <= 0 || status.PublishedIssue.URL == "" {
		http.Error(w, "published ticket details are unavailable", http.StatusInternalServerError)
		return
	}
	promotedPath := status.Path
	var err error
	if a.deps.Intake != nil && status.Path != "" {
		promotedPath, err = a.deps.Intake.Promote(r.Context(), status.Path, status.PublishedIssue)
		if err != nil {
			log.Printf("promote intake for repository %q: %s", id, redactSecrets(err.Error()))
			http.Error(w, "ticket was published; retry to complete intake promotion", http.StatusInternalServerError)
			return
		}
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET state=?,clone_path=?,updated_at=? WHERE repository_id=? AND state=?`, intakePublished, promotedPath, time.Now().UTC().Format(time.RFC3339Nano), id, intakePromoting)
	if err != nil {
		log.Printf("record promoted intake workspace for repository %q: %s", id, redactSecrets(err.Error()))
		http.Error(w, "ticket was published but promotion could not be recorded", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "ticket was published but promotion state changed", http.StatusConflict)
		return
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) abandonIntake(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil || (status.State != intakeDraft && status.State != intakeReady && status.State != intakeConfirmed) {
		http.Error(w, "only unpublished intakes can be abandoned", http.StatusConflict)
		return
	}
	claimed, err := a.claimAbandonment(r.Context(), id, status.State)
	if err != nil {
		http.Error(w, "could not claim intake abandonment", http.StatusInternalServerError)
		return
	}
	if !claimed {
		http.Error(w, "intake state changed before abandonment", http.StatusConflict)
		return
	}
	if a.deps.Intake != nil && status.Path != "" {
		if err = a.deps.Intake.Cleanup(r.Context(), status.Path); err != nil {
			_ = a.releaseAbandonment(r.Context(), id, status.State)
			log.Printf("clean abandoned intake for repository %q: %s", id, redactSecrets(err.Error()))
			http.Error(w, "could not clean abandoned intake", http.StatusInternalServerError)
			return
		}
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET state=?,clone_path='',updated_at=? WHERE repository_id=? AND state=?`, intakeAbandoned, time.Now().UTC().Format(time.RFC3339Nano), id, intakeAbandoning)
	if err != nil {
		http.Error(w, "could not abandon intake", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "intake state changed before abandonment", http.StatusConflict)
		return
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) claimAbandonment(ctx context.Context, id string, state intakeState) (bool, error) {
	result, err := a.db.ExecContext(ctx, `UPDATE intakes SET state=?,updated_at=? WHERE repository_id=? AND state=?`, intakeAbandoning, time.Now().UTC().Format(time.RFC3339Nano), id, state)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (a *application) releaseAbandonment(ctx context.Context, id string, state intakeState) error {
	_, err := a.db.ExecContext(ctx, `UPDATE intakes SET state=?,updated_at=? WHERE repository_id=? AND state=?`, state, time.Now().UTC().Format(time.RFC3339Nano), id, intakeAbandoning)
	return err
}

func (a *application) redirectWorkspace(w http.ResponseWriter, r *http.Request, id string) {
	http.Redirect(w, r, "/repositories/"+url.PathEscape(id), http.StatusSeeOther)
}

func (a *application) executorSelect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.repository(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	var priorFailures []FailureRecord
	if a.deps.RunLease != nil {
		var err error
		priorFailures, err = a.deps.RunLease.RecentFailures(r.Context(), id, 10)
		if err != nil {
			http.Error(w, "could not load executor history", http.StatusInternalServerError)
			return
		}
	}
	selection := SelectExecutor("medium", "", priorFailures)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(r.Context(),
		`INSERT INTO intakes(repository_id,state,executor_kind,executor_rationale,executor_state,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(repository_id) DO UPDATE SET executor_kind=excluded.executor_kind,executor_rationale=excluded.executor_rationale,executor_state=excluded.executor_state,updated_at=excluded.updated_at`,
		id, "", string(selection.Kind), selection.Rationale, string(executorSelected), now); err != nil {
		http.Error(w, "could not store executor selection", http.StatusInternalServerError)
		return
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) executorPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if status.ExecutorKind == "" {
		http.Error(w, "executor must be selected before planning begins", http.StatusConflict)
		return
	}
	if status.ExecutorState != executorSelected {
		http.Error(w, "executor is not in the correct state for planning", http.StatusConflict)
		return
	}
	claim, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorPlanning, time.Now().UTC().Format(time.RFC3339Nano), id, executorSelected)
	if err != nil {
		http.Error(w, "could not claim executor planning", http.StatusInternalServerError)
		return
	}
	claimed, err := claim.RowsAffected()
	if err != nil || claimed != 1 {
		http.Error(w, "executor state changed before planning could start", http.StatusConflict)
		return
	}
	planned := false
	defer func() {
		if !planned {
			_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorSelected, time.Now().UTC().Format(time.RFC3339Nano), id, executorPlanning)
		}
	}()

	repo, err := a.repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	workspacePath := status.ExecutorWorkspacePath
	if a.deps.IssueWorkspace != nil {
		workspacePath, err = a.executorWorkspace(r.Context(), repo, status)
		if err != nil {
			http.Error(w, fmt.Sprintf("could not prepare issue workspace: %v", err), http.StatusFailedDependency)
			return
		}
	}
	scope := "medium" // TODO: Get from conversation state
	executorKind := ExecutorKind(status.ExecutorKind)
	if executorKind == VerificationOnly {
		a.runVerification(w, r, id, workspacePath)
		return
	}

	var planContent string

	// Generate plan if Planner is configured
	if a.deps.Planner != nil {
		var err error
		planContent, err = a.deps.Planner.GeneratePlan(r.Context(), workspacePath, executorKind, scope)
		if err != nil {
			http.Error(w, fmt.Sprintf("planning failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Run preflight checks if Preflight is configured
	if a.deps.Preflight != nil {
		preflightResult := a.deps.Preflight.Verify(r.Context(), workspacePath, repo.FullName)
		if !preflightResult.Passed {
			http.Error(w, "preflight checks failed", http.StatusFailedDependency)
			return
		}
	}

	// Run critique if Critiquer is configured
	if a.deps.Critiquer != nil {
		critiqueResult, err := a.deps.Critiquer.CritiquePlan(r.Context(), workspacePath, executorKind, scope)
		if err != nil {
			http.Error(w, fmt.Sprintf("critique failed: %v", err), http.StatusInternalServerError)
			return
		}
		if critiqueResult.Blocked {
			http.Error(w, "plan blocked after max critique rounds", http.StatusFailedDependency)
			return
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "could not record planning state", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO intake_artifacts(repository_id,kind,body,created_at) VALUES(?,?,?,?) ON CONFLICT(repository_id,kind) DO UPDATE SET body=excluded.body,created_at=excluded.created_at`,
		id, "plan", planContent, now); err != nil {
		http.Error(w, "could not store plan", http.StatusInternalServerError)
		return
	}
	result, err := tx.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorPlanned, now, id, executorPlanning)
	if err != nil {
		http.Error(w, "could not record planning state", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "executor state changed before planning could complete", http.StatusConflict)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "could not persist planning result", http.StatusInternalServerError)
		return
	}
	planned = true

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("planning completed"))
}

func (a *application) executorApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.repository(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if status.ExecutorState != executorPlanned {
		http.Error(w, "a planned executor run is required before approval", http.StatusConflict)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := a.db.ExecContext(r.Context(),
		`UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`,
		string(executorApproved), now, id, string(executorPlanned))
	if err != nil {
		http.Error(w, "could not record approval", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "executor state changed before approval could be recorded", http.StatusConflict)
		return
	}
	a.redirectWorkspace(w, r, id)
}

// verifyExecutorRun runs the canonical checks after a coding executor has
// completed successfully. The state transition is compare-and-swap guarded
// so a run cannot be verified twice by competing requests.
func (a *application) verifyExecutorRun(id, workspacePath string) bool {
	if a.deps.VerificationRunner == nil {
		// Deployments without the optional verifier retain the historical
		// completed state; configured deployments always pass through checks.
		return true
	}
	result, err := a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state IN (?,?)`, executorVerifying, time.Now().UTC().Format(time.RFC3339Nano), id, executorCompleted, executorVerifying)
	if err != nil {
		return false
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return false
	}
	verification, err := a.deps.VerificationRunner.Run(a.ctx, workspacePath, nil)
	a.storeVerificationOutput(id, verification)
	state := executorVerified
	if err != nil || !verification.ReadyForPR {
		state = executorFailed
	}
	_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, state, time.Now().UTC().Format(time.RFC3339Nano), id, executorVerifying)
	if state == executorFailed {
		if status, statusErr := a.intakeStatus(a.ctx, id); statusErr == nil {
			a.releaseLease(a.ctx, status, "verification failed")
		}
	}
	return state == executorVerified
}

func (a *application) storeVerificationOutput(id string, verification VerificationResult) {
	var body strings.Builder
	body.WriteString("# Verification output\n\n")
	for _, check := range verification.Checks {
		fmt.Fprintf(&body, "## %s\n\nPassed: %t\nExit code: %d\n\n%s\n\n", check.Name, check.Passed, check.ExitCode, redactSecrets(check.Output))
	}
	if len(verification.Checks) == 0 {
		fmt.Fprintf(&body, "Ready for PR: %t\n", verification.ReadyForPR)
	}
	_, _ = a.db.ExecContext(a.ctx, `INSERT INTO intake_artifacts(repository_id,kind,body,created_at) VALUES(?,?,?,?) ON CONFLICT(repository_id,kind) DO UPDATE SET body=excluded.body,created_at=excluded.created_at`, id, artifactVerificationOutput, body.String(), time.Now().UTC().Format(time.RFC3339Nano))
}

func (a *application) executorRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if status.ExecutorKind == "" {
		http.Error(w, "executor must be selected before execution", http.StatusConflict)
		return
	}
	if status.ExecutorState != executorApproved {
		http.Error(w, "executor run requires operator approval", http.StatusConflict)
		return
	}
	if a.deps.IssueWorkspace != nil && status.ExecutorWorkspacePath == "" {
		http.Error(w, "issue workspace is not configured", http.StatusFailedDependency)
		return
	}
	if ExecutorKind(status.ExecutorKind) == VerificationOnly {
		a.runVerification(w, r, id, status.ExecutorWorkspacePath)
		return
	}
	if a.deps.ExecutorRunner == nil {
		http.Error(w, "executor runner is not configured", http.StatusServiceUnavailable)
		return
	}
	if ExecutorKind(status.ExecutorKind) == Ralphex && a.deps.RalphexExecutionConfigDir == "" {
		http.Error(w, "ralphex execution configuration is not configured", http.StatusServiceUnavailable)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := a.db.ExecContext(r.Context(),
		`UPDATE intakes SET executor_state=?,executor_heartbeat=?,updated_at=? WHERE repository_id=? AND executor_state=?`,
		string(executorRunning), now, now, id, string(executorApproved))
	if err != nil {
		http.Error(w, "could not start executor", http.StatusInternalServerError)
		return
	}
	claimed, err := result.RowsAffected()
	if err != nil || claimed != 1 {
		http.Error(w, "executor state changed before run could be started", http.StatusConflict)
		return
	}
	runID := ""
	if a.deps.RunLease != nil {
		runID, err = a.deps.RunLease.Acquire(r.Context(), id, status.PublishedIssue.Number, ExecutorKind(status.ExecutorKind))
		if err != nil {
			_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorApproved, time.Now().UTC().Format(time.RFC3339Nano), id, executorRunning)
			http.Error(w, fmt.Sprintf("could not acquire executor lease: %v", err), http.StatusConflict)
			return
		}
		if _, err := a.db.ExecContext(a.ctx, `UPDATE intakes SET run_id=?,updated_at=? WHERE repository_id=? AND executor_state=?`, runID, time.Now().UTC().Format(time.RFC3339Nano), id, executorRunning); err != nil {
			_ = a.deps.RunLease.Release(a.ctx, runID, "failed", "could not persist executor lease")
			_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorApproved, time.Now().UTC().Format(time.RFC3339Nano), id, executorRunning)
			http.Error(w, "could not persist executor lease", http.StatusInternalServerError)
			return
		}
	}
	// Register cancellation only after this request has atomically claimed the
	// approved run. A competing request must never replace its cancel handler.
	runCtx, runCancel := context.WithCancel(a.ctx)
	a.execCancelsMu.Lock()
	if a.execCancels == nil {
		a.execCancels = make(map[string]context.CancelFunc)
	}
	a.execCancels[id] = runCancel
	a.execCancelsMu.Unlock()
	defer func() {
		a.execCancelsMu.Lock()
		delete(a.execCancels, id)
		a.execCancelsMu.Unlock()
		runCancel()
	}()
	// Collect sanitised output lines for storage.
	var outputLines []string
	var outputLinesMu sync.Mutex
	nowTime := time.Now()
	onLine := func(line string) error {
		line = redactSecrets(line)
		outputLinesMu.Lock()
		outputLines = append(outputLines, line)
		outputLinesMu.Unlock()
		// Update heartbeat so the dashboard can show the run is alive.
		// Use application context (not r.Context()) so heartbeats persist
		// even if the HTTP client disconnects.
		heartbeat := time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = a.db.ExecContext(a.ctx,
			`UPDATE intakes SET executor_heartbeat=? WHERE repository_id=? AND executor_state=?`,
			heartbeat, id, string(executorRunning))
		return nil
	}
	// Determine command based on executor kind
	var commandName string
	var commandArgs []string

	workspacePath := status.ExecutorWorkspacePath
	planPath := workspacePath + "/plan.md"

	switch ExecutorKind(status.ExecutorKind) {
	case Ralphex:
		commandName = "ralphex"
		commandArgs = []string{"--config-dir", a.deps.RalphexExecutionConfigDir, planPath}
	case Codex:
		commandName = "codex"
		commandArgs = []string{"exec", planPath}
	case Pi:
		commandName = "pi"
		commandArgs = []string{"-p", fmt.Sprintf("Execute the plan at %s", planPath)}
	default:
		http.Error(w, fmt.Sprintf("unsupported executor kind: %s", status.ExecutorKind), http.StatusBadRequest)
		return
	}

	runResult, runErr := a.deps.ExecutorRunner.Run(runCtx, workspacePath, onLine, commandName, commandArgs...)
	duration := time.Since(nowTime)
	terminalState := executorCompleted
	if runResult.Cancelled {
		terminalState = executorFailed
	} else if runErr != nil || runResult.ExitCode != 0 {
		terminalState = executorFailed
	}
	// Store sanitised output as an artifact.
	outputBody := "# Executor output\n\n"
	for _, line := range outputLines {
		outputBody += line + "\n"
	}
	outputBody += fmt.Sprintf("\n---\nExit code: %d\nDuration: %v\nCancelled: %v\n", runResult.ExitCode, runResult.Duration, runResult.Cancelled)
	now = time.Now().UTC().Format(time.RFC3339Nano)
	// Use application context (not r.Context()) for terminal state writes.
	// This ensures the executor state transitions to terminal even if the
	// HTTP client disconnects during the run. Without this, a disconnected
	// client would leave the executor permanently stuck in "running" state.
	tx, txErr := a.db.BeginTx(a.ctx, nil)
	if txErr != nil {
		http.Error(w, "could not store executor output", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(a.ctx,
		`INSERT INTO intake_artifacts(repository_id,kind,body,created_at) VALUES(?,?,?,?) ON CONFLICT(repository_id,kind) DO UPDATE SET body=excluded.body,created_at=excluded.created_at`,
		id, artifactExecutorOutput, outputBody, now); err != nil {
		http.Error(w, "could not store executor output", http.StatusInternalServerError)
		return
	}
	durationNs := duration.Nanoseconds()
	cancelledInt := 0
	if runResult.Cancelled {
		cancelledInt = 1
	}
	if _, err := tx.ExecContext(a.ctx,
		`UPDATE intakes SET executor_state=?,executor_heartbeat=?,executor_duration_ns=?,executor_exit_code=?,executor_cancelled=?,updated_at=? WHERE repository_id=? AND executor_state=?`,
		string(terminalState), now, durationNs, runResult.ExitCode, cancelledInt, now, id, string(executorRunning)); err != nil {
		http.Error(w, "could not record executor result", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "could not persist executor result", http.StatusInternalServerError)
		return
	}
	if terminalState == executorCompleted {
		a.verifyExecutorRun(id, workspacePath)
	} else if runID != "" {
		_ = a.deps.RunLease.Release(a.ctx, runID, "failed", "executor failed")
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) executorCreatePR(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.deps.PRCreator == nil {
		http.Error(w, "pull request creator is not configured", http.StatusServiceUnavailable)
		return
	}
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if status.ExecutorState != executorVerified {
		http.Error(w, "pull request creation requires verified work", http.StatusConflict)
		return
	}
	claim, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorCreatingPR, time.Now().UTC().Format(time.RFC3339Nano), id, executorVerified)
	if err != nil {
		http.Error(w, "could not claim pull request creation", http.StatusInternalServerError)
		return
	}
	claimed, err := claim.RowsAffected()
	if err != nil || claimed != 1 {
		http.Error(w, "executor state changed before pull request creation could start", http.StatusConflict)
		return
	}
	repo, err := a.repository(r.Context(), id)
	if err != nil {
		_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorVerified, time.Now().UTC().Format(time.RFC3339Nano), id, executorCreatingPR)
		http.Error(w, "could not load repository", http.StatusInternalServerError)
		return
	}
	pr, err := a.deps.PRCreator.CreateOrReuse(r.Context(), repo, status)
	if err != nil {
		_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorVerified, time.Now().UTC().Format(time.RFC3339Nano), id, executorCreatingPR)
		http.Error(w, fmt.Sprintf("could not create pull request: %v", err), http.StatusBadGateway)
		return
	}
	if _, err := a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,pr_number=?,pr_url=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorPRCreated, pr.Number, pr.URL, time.Now().UTC().Format(time.RFC3339Nano), id, executorCreatingPR); err != nil {
		http.Error(w, "could not persist pull request", http.StatusInternalServerError)
		return
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) executorReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.deps.Reviewer == nil {
		http.Error(w, "reviewer is not configured", http.StatusServiceUnavailable)
		return
	}
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", 500)
		return
	}
	if status.ExecutorState != executorPRCreated {
		http.Error(w, "review requires a created pull request", http.StatusConflict)
		return
	}
	repo, err := a.repository(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load repository", 500)
		return
	}
	claim, err := a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorReviewing, time.Now().UTC().Format(time.RFC3339Nano), id, executorPRCreated)
	if err != nil {
		http.Error(w, "could not start review", 500)
		return
	}
	claimed, err := claim.RowsAffected()
	if err != nil || claimed != 1 {
		http.Error(w, "review state changed before it could start", http.StatusConflict)
		return
	}
	result, err := a.deps.Reviewer.Review(r.Context(), repo, status.PR, status)
	if err != nil {
		_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorFailed, time.Now().UTC().Format(time.RFC3339Nano), id, executorReviewing)
		a.releaseLease(r.Context(), status, "review failed")
		http.Error(w, "review failed", http.StatusBadGateway)
		return
	}
	state := executorMergeReady
	if !result.Approved || result.Blocked {
		state = executorReviewBlocked
		if a.deps.ReviewCommenter != nil {
			if err := a.deps.ReviewCommenter.PostFindings(r.Context(), repo, status.PR, result.Findings, status.ReviewRound); err != nil {
				_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorFailed, time.Now().UTC().Format(time.RFC3339Nano), id, executorReviewing)
				a.releaseLease(r.Context(), status, "publish review findings failed")
				http.Error(w, "could not publish review findings", http.StatusBadGateway)
				return
			}
		}
	}
	if _, err = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,review_findings=?,updated_at=? WHERE repository_id=? AND executor_state=?`, state, result.Findings, time.Now().UTC().Format(time.RFC3339Nano), id, executorReviewing); err != nil {
		http.Error(w, "could not persist review", 500)
		return
	}
	if state == executorMergeReady && a.deps.Telegram != nil {
		_ = a.deps.Telegram.Notify(r.Context(), Notification{Text: "Pull request is ready for merge approval", URL: strings.TrimRight(a.deps.DashboardURL, "/") + "/repositories/" + id})
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) executorApproveMerge(w http.ResponseWriter, r *http.Request) {
	if a.deps.MergeabilityChecker == nil {
		http.Error(w, "mergeability checker is not configured", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	repo, err := a.repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if status.ExecutorState != executorMergeReady {
		http.Error(w, "merge approval requires a review-ready pull request", http.StatusConflict)
		return
	}
	mergeable, reason, err := a.deps.MergeabilityChecker.CheckMergeable(r.Context(), repo, status.PR.Number)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not check pull request mergeability: %v", err), http.StatusBadGateway)
		return
	}
	if !mergeable {
		http.Error(w, reason, http.StatusConflict)
		return
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorMergeApproved, time.Now().UTC().Format(time.RFC3339Nano), id, executorMergeReady)
	if err != nil {
		http.Error(w, "could not record merge approval", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "executor state changed before merge approval could be recorded", http.StatusConflict)
		return
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) executorFix(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.deps.ExecutorRunner == nil {
		http.Error(w, "executor runner is not configured", http.StatusServiceUnavailable)
		return
	}
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil || status.ExecutorState != executorReviewBlocked {
		http.Error(w, "review fix requires blocked review", http.StatusConflict)
		return
	}
	if status.ReviewRound >= MaxReviewRounds {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,review_findings=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorFailed, "maximum review fix rounds exhausted", time.Now().UTC().Format(time.RFC3339Nano), id, executorReviewBlocked)
		a.releaseLease(r.Context(), status, "maximum review fix rounds exhausted")
		http.Error(w, "maximum review fix rounds exhausted", http.StatusConflict)
		return
	}
	var name string
	var args []string
	prompt := fmt.Sprintf("Fix the following independent review findings in the workspace, then run the relevant checks:\n\n%s", status.ReviewFindings)
	switch ExecutorKind(status.ExecutorKind) {
	case Ralphex:
		if a.deps.RalphexExecutionConfigDir == "" {
			http.Error(w, "ralphex execution configuration is not configured", http.StatusServiceUnavailable)
			return
		}
		name, args = "ralphex", []string{"--config-dir", a.deps.RalphexExecutionConfigDir, "-p", prompt}
	case Codex:
		name, args = "codex", []string{"exec", prompt}
	case Pi:
		name, args = "pi", []string{"-p", prompt}
	default:
		http.Error(w, "unsupported executor kind for review fix", http.StatusBadRequest)
		return
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorFixing, time.Now().UTC().Format(time.RFC3339Nano), id, executorReviewBlocked)
	if err != nil {
		http.Error(w, "could not claim review fix", http.StatusInternalServerError)
		return
	}
	changed, rowsErr := result.RowsAffected()
	if rowsErr != nil || changed != 1 {
		http.Error(w, "review state changed before fix could start", http.StatusConflict)
		return
	}
	runResult, runErr := a.deps.ExecutorRunner.Run(r.Context(), status.ExecutorWorkspacePath, nil, name, args...)
	if runErr != nil || runResult.ExitCode != 0 || runResult.Cancelled {
		_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorFailed, time.Now().UTC().Format(time.RFC3339Nano), id, executorFixing)
		a.releaseLease(r.Context(), status, "review fix executor failed")
		http.Error(w, "review fix executor failed", http.StatusBadGateway)
		return
	}
	_, err = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,review_round=review_round+1,updated_at=? WHERE repository_id=? AND executor_state=?`, executorVerifying, time.Now().UTC().Format(time.RFC3339Nano), id, executorFixing)
	if err != nil {
		http.Error(w, "could not start post-fix verification", http.StatusInternalServerError)
		return
	}
	a.verifyExecutorRun(id, status.ExecutorWorkspacePath)
	_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorPRCreated, time.Now().UTC().Format(time.RFC3339Nano), id, executorVerified)
	a.redirectWorkspace(w, r, id)
}

func (a *application) executorMerge(w http.ResponseWriter, r *http.Request) {
	if a.deps.MergeExecutor == nil {
		http.Error(w, "merge executor is not configured", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", 500)
		return
	}
	if status.ExecutorState != executorMergeApproved {
		http.Error(w, "merge requires approved pull request", http.StatusConflict)
		return
	}
	repo, err := a.repository(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load repository", 500)
		return
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorMerging, time.Now().UTC().Format(time.RFC3339Nano), id, executorMergeApproved)
	if err != nil {
		http.Error(w, "could not start merge", 500)
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		http.Error(w, "merge approval was already consumed", http.StatusConflict)
		return
	}
	if err = a.deps.MergeExecutor.Merge(r.Context(), repo, status.PR.Number); err != nil {
		_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,review_findings=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorMergeReady, err.Error(), time.Now().UTC().Format(time.RFC3339Nano), id, executorMerging)
		http.Error(w, fmt.Sprintf("pull request merge rejected: %v", err), http.StatusConflict)
		return
	}
	if _, err = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorMerged, time.Now().UTC().Format(time.RFC3339Nano), id, executorMerging); err != nil {
		http.Error(w, "could not record merged state", 500)
		return
	}
	if err = a.deps.MergeExecutor.CloseIssue(r.Context(), repo, status.PublishedIssue.Number); err != nil {
		http.Error(w, fmt.Sprintf("issue closure failed; retry cleanup: %v", err), http.StatusBadGateway)
		return
	}
	a.redirectWorkspace(w, r, id)
}

// executorCleanup removes the implementation workspace only after GitHub
// confirms that the pull request is merged. Local state is deliberately not
// sufficient: a crash can leave it ahead of GitHub.
func (a *application) executorCleanup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.deps.MergeExecutor == nil || a.deps.IssueWorkspace == nil {
		http.Error(w, "cleanup is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := a.cleanupMerged(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.redirectWorkspace(w, r, id)
}

// cleanupMerged completes the post-merge work only after GitHub confirms the
// merge. It is shared by the operator action and startup recovery.
func (a *application) cleanupMerged(ctx context.Context, id string) error {
	if a.deps.MergeExecutor == nil || a.deps.IssueWorkspace == nil {
		return errors.New("cleanup is not configured")
	}
	status, err := a.intakeStatus(ctx, id)
	if err != nil {
		return fmt.Errorf("load intake: %w", err)
	}
	if status.ExecutorState != executorMerged {
		return errors.New("cleanup requires a merged pull request")
	}
	repo, err := a.repository(ctx, id)
	if err != nil {
		return fmt.Errorf("load repository: %w", err)
	}
	if err := a.deps.MergeExecutor.ConfirmMerged(ctx, repo, status.PR.Number); err != nil {
		return fmt.Errorf("pull request is not confirmed merged: %w", err)
	}
	if err := a.deps.MergeExecutor.CloseIssue(ctx, repo, status.PublishedIssue.Number); err != nil {
		return fmt.Errorf("close issue: %w", err)
	}
	if err := a.deps.IssueWorkspace.Cleanup(ctx, status.ExecutorWorkspacePath); err != nil {
		return fmt.Errorf("cleanup workspace: %w", err)
	}
	result, err := a.db.ExecContext(ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorCleanupDone, time.Now().UTC().Format(time.RFC3339Nano), id, executorMerged)
	if err != nil {
		return fmt.Errorf("record cleanup: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed == 1 && a.deps.RunLease != nil && status.RunID != "" {
		if err := a.deps.RunLease.Release(ctx, status.RunID, "completed", ""); err != nil {
			return fmt.Errorf("release implementation lease: %w", err)
		}
	}
	return nil
}

// executorWorkspace returns the durable clone for an implementation issue,
// creating it exactly once before planning or verification. Intake clones are
// deliberately never reused for executor work.
func (a *application) executorWorkspace(ctx context.Context, repo Repository, status IntakeStatus) (string, error) {
	if status.ExecutorWorkspacePath != "" {
		return status.ExecutorWorkspacePath, nil
	}
	if a.deps.IssueWorkspace == nil {
		return "", errors.New("issue workspace is not configured")
	}
	if status.PublishedIssue.Number < 1 {
		return "", errors.New("a published issue is required")
	}
	path, err := a.deps.IssueWorkspace.Start(ctx, repo, status.PublishedIssue.Number)
	if err != nil {
		return "", err
	}
	result, err := a.db.ExecContext(ctx,
		`UPDATE intakes SET executor_workspace_path=?,updated_at=? WHERE repository_id=? AND executor_workspace_path=''`,
		path, time.Now().UTC().Format(time.RFC3339Nano), repo.ID)
	if err != nil {
		return "", fmt.Errorf("persist issue workspace: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("inspect issue workspace persistence: %w", err)
	}
	if changed == 1 {
		return path, nil
	}
	var persisted string
	if err := a.db.QueryRowContext(ctx, `SELECT executor_workspace_path FROM intakes WHERE repository_id=?`, repo.ID).Scan(&persisted); err != nil {
		return "", fmt.Errorf("load concurrently created issue workspace: %w", err)
	}
	if persisted == "" {
		return "", errors.New("issue workspace was not persisted")
	}
	return persisted, nil
}

// runVerification is the terminal path for verification-only selections. It
// intentionally bypasses planning, critique, approval, and executor binaries.
func (a *application) runVerification(w http.ResponseWriter, r *http.Request, id, workspacePath string) {
	if a.deps.VerificationRunner == nil {
		http.Error(w, "verification runner is not configured", http.StatusServiceUnavailable)
		return
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state IN (?,?,?)`, executorVerifying, time.Now().UTC().Format(time.RFC3339Nano), id, executorSelected, executorApproved, executorPlanning)
	if err != nil {
		http.Error(w, "could not start verification", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "executor state changed before verification could start", http.StatusConflict)
		return
	}
	if a.deps.RunLease != nil {
		status, statusErr := a.intakeStatus(r.Context(), id)
		if statusErr != nil {
			http.Error(w, "could not load verification intake", http.StatusInternalServerError)
			return
		}
		runID, acquireErr := a.deps.RunLease.Acquire(r.Context(), id, status.PublishedIssue.Number, VerificationOnly)
		if acquireErr != nil {
			_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorSelected, time.Now().UTC().Format(time.RFC3339Nano), id, executorVerifying)
			http.Error(w, fmt.Sprintf("could not acquire executor lease: %v", acquireErr), http.StatusConflict)
			return
		}
		if _, persistErr := a.db.ExecContext(a.ctx, `UPDATE intakes SET run_id=?,updated_at=? WHERE repository_id=? AND executor_state=?`, runID, time.Now().UTC().Format(time.RFC3339Nano), id, executorVerifying); persistErr != nil {
			_ = a.deps.RunLease.Release(a.ctx, runID, "failed", "could not persist verification lease")
			_, _ = a.db.ExecContext(a.ctx, `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorSelected, time.Now().UTC().Format(time.RFC3339Nano), id, executorVerifying)
			http.Error(w, "could not persist executor lease", http.StatusInternalServerError)
			return
		}
	}
	verification, err := a.deps.VerificationRunner.Run(r.Context(), workspacePath, nil)
	a.storeVerificationOutput(id, verification)
	state := executorVerified
	if err != nil || !verification.ReadyForPR {
		state = executorFailed
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, state, time.Now().UTC().Format(time.RFC3339Nano), id, executorVerifying); err != nil {
		http.Error(w, "could not record verification result", http.StatusInternalServerError)
		return
	}
	if state == executorFailed {
		if status, statusErr := a.intakeStatus(r.Context(), id); statusErr == nil {
			a.releaseLease(r.Context(), status, "verification failed")
		}
	}
	a.redirectWorkspace(w, r, id)
}

func (a *application) executorCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load intake", http.StatusInternalServerError)
		return
	}
	if status.ExecutorState == executorReviewing || status.ExecutorState == executorMerging {
		result, updateErr := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state IN (?,?)`, executorFailed, time.Now().UTC().Format(time.RFC3339Nano), id, executorReviewing, executorMerging)
		if updateErr != nil {
			http.Error(w, "could not cancel executor phase", http.StatusInternalServerError)
			return
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			http.Error(w, "executor phase already changed", http.StatusConflict)
			return
		}
		a.releaseFailedLease(r.Context(), status)
		a.redirectWorkspace(w, r, id)
		return
	}
	if status.ExecutorState != executorRunning {
		// Already finished — cancellation is a no-op, not an error.
		a.redirectWorkspace(w, r, id)
		return
	}

	a.execCancelsMu.Lock()
	cancel := a.execCancels[id]
	if cancel != nil {
		delete(a.execCancels, id)
	}
	a.execCancelsMu.Unlock()

	if cancel != nil {
		cancel()
	}

	a.redirectWorkspace(w, r, id)
}

// Retry endpoints only rewind the failed phase's own state. The phase handler
// then performs its normal idempotent GitHub query before mutating anything.
func (a *application) executorRetryPR(w http.ResponseWriter, r *http.Request) {
	a.retryPhase(w, r, executorVerified, executorPRCreated)
}

func (a *application) executorRetryReview(w http.ResponseWriter, r *http.Request) {
	a.retryPhase(w, r, executorPRCreated, executorReviewing)
}

func (a *application) executorRetryMerge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, executorMergeApproved, time.Now().UTC().Format(time.RFC3339Nano), id, executorMergeReady)
	if err != nil {
		http.Error(w, "could not retry merge", http.StatusInternalServerError)
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		http.Error(w, "merge retry requires a merge-ready intake", http.StatusConflict)
		return
	}
	a.executorMerge(w, r)
}

func (a *application) executorRetryCleanup(w http.ResponseWriter, r *http.Request) {
	a.executorCleanup(w, r)
}

func (a *application) retryPhase(w http.ResponseWriter, r *http.Request, from, to executorState) {
	id := r.PathValue("id")
	result, err := a.db.ExecContext(r.Context(), `UPDATE intakes SET executor_state=?,updated_at=? WHERE repository_id=? AND executor_state=?`, from, time.Now().UTC().Format(time.RFC3339Nano), id, executorFailed)
	if err != nil {
		http.Error(w, "could not retry executor phase", http.StatusInternalServerError)
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		http.Error(w, "retry requires a failed phase", http.StatusConflict)
		return
	}
	if to == executorPRCreated {
		a.executorCreatePR(w, r)
		return
	}
	a.executorReview(w, r)
}

func (a *application) releaseLease(ctx context.Context, status IntakeStatus, reason string) {
	if a.deps.RunLease != nil && status.RunID != "" {
		_ = a.deps.RunLease.Release(ctx, status.RunID, "failed", reason)
	}
}

func (a *application) releaseFailedLease(ctx context.Context, status IntakeStatus) {
	a.releaseLease(ctx, status, "operator cancelled")
}

func (a *application) synthesizeArtifacts(ctx context.Context, repo Repository, conversation Conversation) ([]Artifact, error) {
	resolved, err := a.deps.Synthesizer.GrillWithDocs(ctx, conversation)
	if err != nil {
		return nil, err
	}
	artifacts := []Artifact{
		{Kind: artifactGlossary, Body: glossaryArtifact(resolved)},
	}
	spec, err := a.deps.Synthesizer.ToSpec(ctx, repo, resolved)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, Artifact{Kind: artifactSpec, Body: spec})
	tickets, err := a.deps.Synthesizer.ToTickets(ctx, repo, resolved)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, Artifact{Kind: artifactTickets, Body: tickets})
	for index, decision := range resolved {
		assessment, proposal, err := a.deps.Synthesizer.AssessADR(ctx, strings.TrimSpace(strings.TrimPrefix(decision, "-")))
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, Artifact{Kind: artifactKind(artifactADRAssessmentPrefix + strconv.Itoa(index+1)), Body: assessment})
		if proposal != "" {
			artifacts = append(artifacts, Artifact{Kind: artifactKind(artifactADRProposalPrefix + strconv.Itoa(index+1)), Body: proposal})
		}
	}
	return artifacts, nil
}

// AssessADR is a bounded policy capability: an ADR is eligible only when the
// settled decision states the alternative, trade-off, and cost of reversal.
// This keeps the operator from self-attesting eligibility in the dashboard.
func AssessADR(decision string) (string, string) {
	fields := make(map[string]string)
	for _, part := range strings.Split(decision, ";") {
		name, value, found := strings.Cut(part, ":")
		if found {
			fields[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
	chosen, alternative := fields["decision"], fields["alternative"]
	tradeoff, reversalCost := fields["trade-off"], fields["reversal cost"]
	consequential, hardToReverse := consequentialDecision(chosen, tradeoff, reversalCost), hardToReverseDecision(reversalCost)
	if chosen == "" || alternative == "" || tradeoff == "" || reversalCost == "" || !consequential || !hardToReverse {
		return "# ADR eligibility assessment\n\n## Decision\n\n" + decision + "\n\n## Criteria\n\n- Consequential: " + strconv.FormatBool(consequential) + "\n- Hard to reverse: " + strconv.FormatBool(hardToReverse) + "\n- Alternative, trade-off, and reversal cost recorded: " + strconv.FormatBool(chosen != "" && alternative != "" && tradeoff != "" && reversalCost != "") + "\n\n## Result\n\nIneligible. The PM cannot establish every ADR criterion from this settled decision.", ""
	}
	assessment := "# ADR eligibility assessment\n\n## Decision\n\n" + chosen + "\n\n## Alternative\n\n" + alternative + "\n\n## Trade-off\n\n" + tradeoff + "\n\n## Reversal cost\n\n" + reversalCost + "\n\n## Result\n\nEligible: this is a consequential, hard-to-reverse decision with a real trade-off."
	proposal := "# ADR proposal\n\n## Decision\n\n" + chosen + "\n\n## Alternative\n\n" + alternative + "\n\n## Trade-off\n\n" + tradeoff + "\n\n## Reversal cost\n\n" + reversalCost
	return assessment, proposal
}

func consequentialDecision(chosen, tradeoff, reversalCost string) bool {
	evidence := strings.ToLower(chosen + " " + tradeoff + " " + reversalCost)
	return containsAny(evidence, "migration", "durable", "database", "schema", "protocol", "security", "authentication")
}

func hardToReverseDecision(reversalCost string) bool {
	return containsAny(strings.ToLower(reversalCost), "migration", "data", "session", "deployment", "rollback")
}

func containsAny(value string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(value, term) {
			return true
		}
	}
	return false
}

// GrillWithDocs is the bounded discovery capability. It accepts only settled
// operator decisions from completed discovery turns; repository evidence stays
// outside the synthesized artifacts unless the operator adopts it explicitly.
func GrillWithDocs(conversation Conversation) []string {
	var resolved []string
	for index, message := range conversation.Messages {
		if message.Role == "operator" {
			if index+1 < len(conversation.Messages) && conversation.Messages[index+1].Role == "pm" {
				resolved = append(resolved, "- "+redactSecrets(message.Text))
			}
		}
	}
	if len(resolved) == 0 {
		resolved = []string{"- No operator decisions have been recorded yet."}
	}
	return resolved
}

func discoveryComplete(conversation Conversation) bool {
	for index, message := range conversation.Messages {
		if message.Role == "operator" && index+1 < len(conversation.Messages) && conversation.Messages[index+1].Role == "pm" {
			return true
		}
	}
	return false
}

func glossaryArtifact(resolved []string) string {
	return "# Glossary updates\n\n- **Intake draft** — unconfirmed discovery output for a repository.\n- **Confirmed ticket set** — vertical slices approved by the operator before GitHub publication.\n\n## Settled terms\n\n" + strings.Join(resolved, "\n")
}

func ToSpec(repo Repository, resolved []string) string {
	return "# Spec: " + repo.FullName + " intake\n\n## Resolved conversation\n\n" + strings.Join(resolved, "\n") + "\n\n## Scope\n\nImplement the smallest vertical slice that satisfies the resolved conversation.\n\n## Non-goals\n\nDo not expand beyond the confirmed intake."
}

func ToTickets(repo Repository, resolved []string) string {
	var tickets strings.Builder
	fmt.Fprintf(&tickets, "# Ticket set: %s\n", repo.FullName)
	for index, decision := range resolved {
		title, acceptance, blocker := ticketFromDecision(decision)
		fmt.Fprintf(&tickets, "\n## Ticket %d: %s\n\nBlocked by: %s\n\n### Acceptance criteria\n\n%s\n\nThis ticket is intentionally a tracer-bullet slice: it crosses the relevant product seam end to end.\n", index+1, title, blocker, acceptance)
	}
	return strings.TrimSpace(tickets.String())
}

var decisionBlockers = regexp.MustCompile(`(?mi)^\s*blocked by:\s*(none|Ticket [1-9][0-9]*(?:\s*,\s*Ticket [1-9][0-9]*)*)\s*$`)

// ticketFromDecision preserves only dependency edges explicitly settled during
// discovery. Unrelated vertical slices remain independently publishable.
func ticketFromDecision(decision string) (title, acceptance, blocker string) {
	acceptance = strings.TrimSpace(decisionBlockers.ReplaceAllString(decision, ""))
	blocker = "none"
	if match := decisionBlockers.FindStringSubmatch(decision); len(match) == 2 {
		blocker = strings.TrimSpace(match[1])
	}
	return ticketTitle(acceptance), acceptance, blocker
}

func ticketTitle(decision string) string {
	title := strings.TrimSpace(strings.TrimPrefix(decision, "-"))
	if title == "" {
		return "deliver the confirmed vertical slice"
	}
	return title
}

var ticketHeading = regexp.MustCompile(`(?m)^## Ticket ([1-9][0-9]*):\s*(.+)$`)
var ticketBlockers = regexp.MustCompile(`(?m)^Blocked by:\s*(.+)$`)
var ticketReference = regexp.MustCompile(`^Ticket ([1-9][0-9]*)$`)

func ticketSetPublications(body string) ([]Publication, error) {
	matches := ticketHeading.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil, errors.New("ticket set has no ticket headings")
	}
	publications := make([]Publication, 0, len(matches))
	for index, match := range matches {
		end := len(body)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		section := strings.TrimSpace(body[match[0]:end])
		number, err := strconv.Atoi(body[match[2]:match[3]])
		if err != nil {
			return nil, fmt.Errorf("parse ticket number: %w", err)
		}
		if number != index+1 {
			return nil, fmt.Errorf("ticket numbers must be consecutive starting at 1")
		}
		title := strings.TrimSpace(body[match[4]:match[5]])
		if title == "" {
			return nil, fmt.Errorf("ticket %d has no title", number)
		}
		blocker := ticketBlockers.FindStringSubmatch(section)
		if len(blocker) != 2 {
			return nil, fmt.Errorf("ticket %d has no blocking edge", number)
		}
		publication := Publication{Title: "feat: " + title, Body: section}
		if strings.TrimSpace(blocker[1]) != "none" {
			for _, reference := range strings.Split(blocker[1], ",") {
				parts := ticketReference.FindStringSubmatch(strings.TrimSpace(reference))
				if len(parts) != 2 {
					return nil, fmt.Errorf("ticket %d has invalid blocker %q", number, reference)
				}
				blockedBy, err := strconv.Atoi(parts[1])
				if err != nil {
					return nil, fmt.Errorf("parse blocker for ticket %d: %w", number, err)
				}
				if blockedBy >= number {
					return nil, fmt.Errorf("ticket %d must only depend on an earlier ticket", number)
				}
				publication.BlockedBy = append(publication.BlockedBy, blockedBy)
			}
		}
		publications = append(publications, publication)
	}
	return publications, nil
}

// validateEnglishPublication keeps the GitHub tracker boundary predictable:
// the discovery conversation may contain any language, but published tickets
// must use the project's portable English/plain-ASCII format. We validate the
// complete set before the first GitHub mutation, so a rejected draft cannot
// leave a partially published ticket set behind.
func validateEnglishPublication(publication Publication) error {
	for _, value := range []string{publication.Title, publication.Body} {
		for _, r := range value {
			if r > unicode.MaxASCII {
				return errors.New("GitHub tickets must use English plain-ASCII text")
			}
		}
	}
	return nil
}
func (a *application) conversation(ctx context.Context, id string) (Conversation, error) {
	return a.conversationAfter(ctx, id, 0)
}

func (a *application) conversationAfter(ctx context.Context, id string, messageStart int64) (Conversation, error) {
	var exists string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM repositories WHERE id=?`, id).Scan(&exists); err != nil {
		return Conversation{}, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT role,text,tokens,cost_usd,created_at FROM messages WHERE repository_id=? AND rowid>? ORDER BY rowid`, id, messageStart)
	if err != nil {
		return Conversation{}, err
	}
	defer func() { _ = rows.Close() }()
	c := Conversation{RepositoryID: id, PendingTurns: []PendingTurn{}, Messages: []Message{}}
	if err := a.db.QueryRowContext(ctx, `SELECT inspection FROM intakes WHERE repository_id=?`, id).Scan(&c.RepositoryEvidence); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, fmt.Errorf("load repository inspection: %w", err)
	}
	for rows.Next() {
		var m Message
		var created string
		var tokens sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&m.Role, &m.Text, &tokens, &cost, &created); err != nil {
			return Conversation{}, err
		}
		m.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return Conversation{}, fmt.Errorf("parse message timestamp: %w", err)
		}
		c.Messages = append(c.Messages, m)
		if m.Role == "pm" {
			c.LastReply = Reply{Text: m.Text, Tokens: int(tokens.Int64), CostUSD: cost.Float64}
		}
	}
	if err := rows.Err(); err != nil {
		return Conversation{}, err
	}
	pending, err := a.db.QueryContext(ctx, `SELECT id FROM pending_turns WHERE repository_id=? AND state IN ('pending','running') ORDER BY rowid`, id)
	if err != nil {
		return Conversation{}, err
	}
	defer func() { _ = pending.Close() }()
	for pending.Next() {
		var turn PendingTurn
		if err := pending.Scan(&turn.TurnID); err != nil {
			return Conversation{}, err
		}
		turn.RepositoryID = id
		c.PendingTurns = append(c.PendingTurns, turn)
	}
	c.HasPending = len(c.PendingTurns) > 0
	return c, pending.Err()
}

func (a *application) repository(ctx context.Context, id string) (Repository, error) {
	var repo Repository
	err := a.db.QueryRowContext(ctx, `SELECT id,full_name FROM repositories WHERE id=?`, id).Scan(&repo.ID, &repo.FullName)
	if err != nil {
		return Repository{}, err
	}
	return repo, nil
}

func (a *application) intakeStatus(ctx context.Context, id string) (IntakeStatus, error) {
	var status IntakeStatus
	var number sql.NullInt64
	var executorStateStr string
	var durationNs int64
	var cancelledInt int
	var prNumber int
	var prURL string
	err := a.db.QueryRowContext(ctx, `SELECT intake_id,state,clone_path,message_start,pending_question,issue_number,issue_url,executor_kind,executor_rationale,executor_state,executor_heartbeat,executor_duration_ns,executor_exit_code,executor_cancelled,executor_workspace_path,run_id,pr_number,pr_url,review_round,review_findings FROM intakes WHERE repository_id=?`, id).Scan(&status.ID, &status.State, &status.Path, &status.MessageStart, &status.PendingQuestion, &number, &status.PublishedIssue.URL, &status.ExecutorKind, &status.ExecutorRationale, &executorStateStr, &status.ExecutorHeartbeat, &durationNs, &status.ExecutorExitCode, &cancelledInt, &status.ExecutorWorkspacePath, &status.RunID, &prNumber, &prURL, &status.ReviewRound, &status.ReviewFindings)
	status.ExecutorState = executorState(executorStateStr)
	status.PR = PullRequest{Number: prNumber, URL: prURL, State: "open"}
	_ = a.db.QueryRowContext(ctx, `SELECT body FROM intake_artifacts WHERE repository_id=? AND kind=?`, id, artifactVerificationOutput).Scan(&status.VerificationOutput)
	status.ExecutorDuration = time.Duration(durationNs)
	status.ExecutorCancelled = cancelledInt != 0
	status.ExecutorCompleted = status.ExecutorState == executorCompleted || status.ExecutorState == executorFailed
	status.PublishedIssue.Number = int(number.Int64)
	if errors.Is(err, sql.ErrNoRows) {
		return IntakeStatus{}, nil
	}
	if err != nil {
		return IntakeStatus{}, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT issue_number,issue_url FROM intake_issues WHERE repository_id=? ORDER BY ticket_index`, id)
	if err != nil {
		return IntakeStatus{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var issue PublishedIssue
		if err := rows.Scan(&issue.Number, &issue.URL); err != nil {
			return IntakeStatus{}, err
		}
		status.PublishedIssues = append(status.PublishedIssues, issue)
	}
	if err := rows.Err(); err != nil {
		return IntakeStatus{}, err
	}
	if len(status.PublishedIssues) == 0 && status.PublishedIssue.Number > 0 {
		status.PublishedIssues = []PublishedIssue{status.PublishedIssue}
	}
	return status, nil
}

func (a *application) artifacts(ctx context.Context, id string) ([]Artifact, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT kind,body,confirmed_at,url FROM intake_artifacts WHERE repository_id=? ORDER BY CASE kind WHEN 'glossary' THEN 1 WHEN 'adr-proposal' THEN 2 WHEN 'spec' THEN 3 WHEN 'tickets' THEN 4 ELSE 5 END`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var artifacts []Artifact
	for rows.Next() {
		var artifact Artifact
		var confirmed sql.NullString
		if err := rows.Scan(&artifact.Kind, &artifact.Body, &confirmed, &artifact.URL); err != nil {
			return nil, err
		}
		artifact.Confirmed = confirmed.Valid
		artifact.NeedsConfirmation = requiresConfirmation(artifact.Kind)
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (a *application) artifact(ctx context.Context, id string, kind artifactKind) (Artifact, error) {
	var artifact Artifact
	var confirmed sql.NullString
	err := a.db.QueryRowContext(ctx, `SELECT kind,body,confirmed_at,url FROM intake_artifacts WHERE repository_id=? AND kind=?`, id, kind).Scan(&artifact.Kind, &artifact.Body, &confirmed, &artifact.URL)
	artifact.Confirmed = confirmed.Valid
	return artifact, err
}

func requiresConfirmation(kind artifactKind) bool {
	return kind == artifactSpec || kind == artifactTickets || strings.HasPrefix(string(kind), artifactADRProposalPrefix)
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func allRequiredArtifactsConfirmed(ctx context.Context, db rowQueryer, id string) (bool, error) {
	var required, unconfirmed int
	err := db.QueryRowContext(ctx, `SELECT
		COUNT(CASE WHEN kind IN (?,?) THEN 1 END),
		COUNT(CASE WHEN (kind IN (?,?) OR kind LIKE ?) AND confirmed_at IS NULL THEN 1 END)
		FROM intake_artifacts WHERE repository_id=?`, artifactSpec, artifactTickets, artifactSpec, artifactTickets, artifactADRProposalPrefix+"%", id).Scan(&required, &unconfirmed)
	if err != nil {
		return false, err
	}
	return required == 2 && unconfirmed == 0, nil
}
func (a *application) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), 500)
	}
}

const pageTemplate = `{{define "repositories"}}<!doctype html><html><head><script src="https://unpkg.com/htmx.org@2.0.4"></script><script>document.addEventListener("htmx:configRequest",function(e){var m=document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/);if(m)e.detail.headers["X-XSRF-TOKEN"]=decodeURIComponent(m[1])})</script><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/core@1.4.0/dist/css/tabler.min.css"></head><body class="p-4"><main class="container-xl"><h1 class="mb-3">Repositories</h1>{{range .}}<form class="mb-2" method="post" action="/repositories/{{.ID}}"><button class="btn btn-primary">{{.FullName}}</button></form>{{end}}</main></body></html>{{end}}
{{define "workspace"}}<!doctype html><html><head><script src="https://unpkg.com/htmx.org@2.0.4"></script><script src="https://unpkg.com/htmx-ext-sse@2.2.2/sse.js"></script><script>document.addEventListener("htmx:configRequest",function(e){var m=document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/);if(m)e.detail.headers["X-XSRF-TOKEN"]=decodeURIComponent(m[1])})</script><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/core@1.4.0/dist/css/tabler.min.css"></head><body class="p-4"><main class="container-xl"><aside id="pm-status" class="card mb-3" role="status" aria-live="polite"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}}{{if .LastReply.Text}} · Tokens: {{.LastReply.Tokens}} · Cost: {{printf "%.4f" .LastReply.CostUSD}}{{end}}</div></aside><section id="conversation" class="card mb-3"><div class="card-body">{{range .Messages}}<article class="mb-2"><strong>{{.Role}}:</strong> {{.Text}}</article>{{end}}{{range .PendingTurns}}{{template "stream-start" .}}{{end}}</div></section><form class="card card-body mb-3" method="post" action="/repositories/{{.RepositoryID}}/conversation" hx-post="/repositories/{{.RepositoryID}}/conversation" hx-target="#conversation .card-body" hx-swap="beforeend" hx-disabled-elt="#pm-send"><label class="form-label" for="pm-message">Message the PM</label><input id="pm-message" class="form-control mb-2" name="message"><button id="pm-send" class="btn btn-primary"{{if .HasPending}} disabled{{end}}>Send</button></form><section class="card mb-3" aria-labelledby="intake-title"><div class="card-body"><h2 id="intake-title" class="card-title">Tracked-work intake</h2><p>State: {{if .Intake.State}}{{.Intake.State}}{{else}}not started{{end}}</p>{{if not .Intake.State}}<form method="post" action="/repositories/{{.RepositoryID}}/intake/start"><button class="btn btn-outline-primary">Start isolated intake</button></form>{{else if eq .Intake.State "draft"}}<form class="d-inline" method="post" action="/repositories/{{.RepositoryID}}/intake/complete-discovery"><button class="btn btn-outline-primary">Complete discovery</button></form><form class="d-inline" method="post" action="/repositories/{{.RepositoryID}}/intake/abandon"><button class="btn btn-outline-danger">Abandon intake</button></form>{{else if eq .Intake.State "ready"}}<form class="d-inline" method="post" action="/repositories/{{.RepositoryID}}/intake/synthesize"><button class="btn btn-outline-primary">Synthesize drafts</button></form>{{else if eq .Intake.State "confirmed"}}<form method="post" action="/repositories/{{.RepositoryID}}/intake/publish"><button class="btn btn-primary">Publish confirmed tickets to GitHub</button></form>{{else if eq .Intake.State "publishing"}}<form method="post" action="/repositories/{{.RepositoryID}}/intake/publish"><button class="btn btn-primary">Retry confirmed ticket publication</button></form>{{else if eq .Intake.State "promoting"}}<form method="post" action="/repositories/{{.RepositoryID}}/intake/publish"><button class="btn btn-primary">Retry intake promotion</button></form>{{end}}{{range .Artifacts}}<article class="card card-sm mt-3"><div class="card-body"><h3 class="h4">{{.Kind}}{{if .Confirmed}} <span class="badge bg-green">confirmed</span>{{end}}</h3><pre class="mb-2 text-wrap">{{.Body}}</pre>{{if .URL}}<a href="{{.URL}}">Published GitHub issue</a>{{end}}{{if and .NeedsConfirmation (not .Confirmed)}}<form method="post" action="/repositories/{{$.RepositoryID}}/intake/{{.Kind}}/confirm"><button class="btn btn-outline-primary">Confirm {{.Kind}}</button></form>{{end}}</div></article>{{end}}</div></section><section class="card mb-3" aria-labelledby="executor-title"><div class="card-body"><h2 id="executor-title" class="card-title">Implementation</h2>{{if .ExecutorSelection}}<p>Executor: {{.ExecutorSelection.Kind}} &mdash; {{.ExecutorSelection.Rationale}}</p>{{if eq .Intake.ExecutorState "selected"}}<form method="post" action="/repositories/{{.RepositoryID}}/executor/plan"><button class="btn btn-primary">Start planning</button></form>{{else if eq .Intake.ExecutorState "planned"}}<p>State: planned &mdash; awaiting operator approval</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/approve"><button class="btn btn-success">Approve plan</button></form>{{else if eq .Intake.ExecutorState "approved"}}<p>State: approved</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/run"><button class="btn btn-primary">Run</button></form>{{else if eq .Intake.ExecutorState "running"}}<p>State: running &mdash; heartbeat: {{.Intake.ExecutorHeartbeat}}</p><form method="post" action="/repositories/{{.RepositoryID}}/executor/cancel"><button class="btn btn-outline-danger">Cancel execution</button></form>{{else if .Intake.ExecutorCompleted}}<p>State: {{.Intake.ExecutorState}}</p><p>Duration: {{.Intake.ExecutorDuration}}</p><p>Exit code: {{.Intake.ExecutorExitCode}}</p>{{if .Intake.ExecutorCancelled}}<p class="text-warning">Run was cancelled</p>{{end}}{{end}}{{else}}<form method="post" action="/repositories/{{.RepositoryID}}/executor/select"><button class="btn btn-outline-primary">Select executor</button></form>{{end}}</div></section><form method="post" action="/notifications/test"><button class="btn btn-outline-secondary">Test Telegram notification</button></form></main></body></html>{{end}}
{{define "turn"}}<article class="card card-body mb-2"><strong>pm:</strong> {{.Reply.Text}}</article><aside id="pm-status" class="card mb-3" role="status" aria-live="polite" hx-swap-oob="true"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}} · Tokens: {{.Reply.Tokens}} · Cost: {{printf "%.4f" .Reply.CostUSD}}</div></aside>{{end}}
{{define "stream-start"}}<div id="pending-turn-{{.TurnID}}" class="card card-body mb-2" hx-ext="sse" sse-connect="/repositories/{{.RepositoryID}}/conversation/{{.TurnID}}/stream" sse-swap="chunk,done,error" sse-close="done" hx-swap="innerHTML"><strong>pm:</strong> Thinking…</div>{{if .Sending}}<button id="pm-send" class="btn btn-primary" disabled hx-swap-oob="outerHTML">Sending…</button>{{end}}{{end}}
{{define "streamed-turn"}}<article id="pending-turn-{{.TurnID}}" class="card card-body mb-2" hx-swap-oob="outerHTML"><strong>pm:</strong> {{.Reply.Text}}</article><button id="pm-send" class="btn btn-primary" hx-swap-oob="outerHTML">Send</button><aside id="pm-status" class="card mb-3" role="status" aria-live="polite" hx-swap-oob="true"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}} · Tokens: {{.Reply.Tokens}} · Cost: {{printf "%.4f" .Reply.CostUSD}}</div></aside>{{end}}`
