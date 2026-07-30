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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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
	Intake    IntakeStatus
	Artifacts []Artifact
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

type artifactKind string

const (
	artifactGlossary artifactKind = "glossary"
	artifactADR      artifactKind = "adr-proposal"
	artifactSpec     artifactKind = "spec"
	artifactTickets  artifactKind = "tickets"
)

type Artifact struct {
	Kind      artifactKind
	Body      string
	Confirmed bool
	URL       string
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

type IntakeStatus struct {
	State           intakeState
	Path            string
	MessageStart    int64
	PendingQuestion string
	PublishedIssue  PublishedIssue
	PublishedIssues []PublishedIssue
}

// GitHub is deliberately the small automation boundary used by the handler.
type GitHub interface {
	Repositories(context.Context) ([]Repository, error)
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
	GitHub       GitHub
	Model        Model
	Telegram     Telegram
	Publisher    Publisher
	Intake       Intake
	Store        string
	AllowedUsers map[string]bool
	DashboardURL string
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
}

var errTurnInProgress = errors.New("PM response already in progress")

func New(deps Dependencies) (*application, error) {
	if deps.GitHub == nil || deps.Model == nil || deps.Store == "" {
		return nil, errors.New("dashboard requires GitHub, model, and SQLite store")
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
		CREATE TABLE IF NOT EXISTS intakes (repository_id TEXT PRIMARY KEY, state TEXT NOT NULL, clone_path TEXT NOT NULL DEFAULT '', inspection TEXT NOT NULL DEFAULT '', message_start INTEGER NOT NULL DEFAULT 0, pending_question TEXT NOT NULL DEFAULT '', issue_number INTEGER, issue_url TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL);
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
	if err = recoverDuplicatePendingTurns(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS one_active_pending_turn_per_repository ON pending_turns(repository_id) WHERE state IN ('pending','running')`); err != nil {
		_ = db.Close()
		return nil, err
	}
	t, err := template.New("page").Parse(pageTemplate)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &application{deps: deps, db: db, templates: t, ctx: ctx, cancel: cancel}
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
	mux.HandleFunc("POST /notifications/test", a.testNotification)
	a.handler = a.authorized(mux)
	if err := a.resumePendingTurns(); err != nil {
		cancel()
		_ = db.Close()
		return nil, err
	}
	return a, nil
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
	a.render(w, "workspace", Workspace{Conversation: c, Intake: intake, Artifacts: artifacts})
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
		reply, err := stream.Stream(ctx, c, prompt, func(chunk string) error {
			text.WriteString(chunk)
			if emit == nil {
				return nil
			}
			return emit(chunk)
		})
		if err == nil && reply.Text == "" {
			reply.Text = text.String()
		}
		reply.Text = redactSecrets(reply.Text)
		return reply, err
	}
	reply, err := a.deps.Model.Reply(ctx, c, prompt)
	reply.Text = redactSecrets(reply.Text)
	return reply, err
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO intakes(repository_id,state,clone_path,inspection,message_start,pending_question,issue_number,issue_url,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(repository_id) DO UPDATE SET state=excluded.state,clone_path=excluded.clone_path,inspection=excluded.inspection,message_start=excluded.message_start,pending_question=excluded.pending_question,issue_number=NULL,issue_url='',updated_at=excluded.updated_at`, repo.ID, intakeDraft, path, inspection, messageStart, initialQuestion, nil, "", now); err != nil {
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
	artifacts := synthesizeArtifacts(repo, conversation)
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
	if kind != artifactSpec && kind != artifactTickets {
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
	var confirmations int
	if err = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM intake_artifacts WHERE repository_id=? AND kind IN (?,?) AND confirmed_at IS NOT NULL`, id, artifactSpec, artifactTickets).Scan(&confirmations); err != nil {
		http.Error(w, "could not confirm artifact", http.StatusInternalServerError)
		return
	}
	if confirmations == 2 {
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
	id := r.PathValue("id")
	status, err := a.intakeStatus(r.Context(), id)
	if err != nil || (status.State != intakeDraft && status.State != intakeConfirmed) {
		http.Error(w, "an active intake is required for an ADR proposal", http.StatusConflict)
		return
	}
	if err = r.ParseForm(); err != nil {
		http.Error(w, "invalid ADR proposal", http.StatusBadRequest)
		return
	}
	decision := strings.TrimSpace(redactSecrets(r.Form.Get("decision")))
	alternative := strings.TrimSpace(redactSecrets(r.Form.Get("alternative")))
	tradeoff := strings.TrimSpace(redactSecrets(r.Form.Get("tradeoff")))
	if r.Form.Get("consequential") != "true" || r.Form.Get("hard_to_reverse") != "true" {
		http.Error(w, "an ADR requires a consequential, hard-to-reverse decision", http.StatusUnprocessableEntity)
		return
	}
	if decision == "" || alternative == "" || tradeoff == "" {
		http.Error(w, "an ADR needs a decision, alternative, and trade-off", http.StatusBadRequest)
		return
	}
	body := "# ADR proposal\n\n## Decision\n\n" + decision + "\n\n## Alternative\n\n" + alternative + "\n\n## Trade-off\n\n" + tradeoff
	if _, err = a.db.ExecContext(r.Context(), `INSERT INTO intake_artifacts(repository_id,kind,body,created_at) VALUES(?,?,?,?) ON CONFLICT(repository_id,kind) DO UPDATE SET body=excluded.body,confirmed_at=NULL,url='',created_at=excluded.created_at`, id, artifactADR, body, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "could not save ADR proposal", http.StatusInternalServerError)
		return
	}
	a.redirectWorkspace(w, r, id)
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
	if !a.confirmed(r.Context(), id, artifactSpec) || !a.confirmed(r.Context(), id, artifactTickets) {
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
		_ = a.releasePublication(r.Context(), id)
		http.Error(w, "specification draft unavailable", http.StatusConflict)
		return
	}
	tickets, err := a.artifact(r.Context(), id, artifactTickets)
	if err != nil {
		_ = a.releasePublication(r.Context(), id)
		http.Error(w, "ticket draft unavailable", http.StatusConflict)
		return
	}
	publications, err := ticketSetPublications(tickets.Body)
	if err != nil {
		_ = a.releasePublication(r.Context(), id)
		http.Error(w, "ticket draft is not publishable", http.StatusConflict)
		return
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
				_ = a.deferPublication(r.Context(), id)
				http.Error(w, "ticket blocker was not published", http.StatusConflict)
				return
			}
			resolvedBlockers = append(resolvedBlockers, "#"+strconv.Itoa(published[blocker-1].Number))
		}
		publication.Body = ticketBlockers.ReplaceAllString(publication.Body, "Blocked by: "+strings.Join(resolvedBlockers, ", "))
		publication.BlockedBy = nil
		publication.Key = fmt.Sprintf("%s-%d", id, index+1)
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
			_ = a.deferPublication(r.Context(), id)
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

func (a *application) releasePublication(ctx context.Context, id string) error {
	_, err := a.db.ExecContext(ctx, `UPDATE intakes SET state=?,updated_at=? WHERE repository_id=? AND state=?`, intakeConfirmed, time.Now().UTC().Format(time.RFC3339Nano), id, intakePublishing)
	return err
}

func (a *application) deferPublication(ctx context.Context, id string) error {
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
	if err != nil || (status.State != intakeDraft && status.State != intakeConfirmed) {
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

func synthesizeArtifacts(repo Repository, conversation Conversation) []Artifact {
	resolved := grillWithDocs(conversation)
	return []Artifact{
		{Kind: artifactGlossary, Body: glossaryArtifact(resolved)},
		{Kind: artifactSpec, Body: toSpec(repo, resolved)},
		{Kind: artifactTickets, Body: toTickets(repo, resolved)},
	}
}

// grillWithDocs is the bounded discovery capability. It accepts only settled
// operator decisions from completed discovery turns; repository evidence stays
// outside the synthesized artifacts unless the operator adopts it explicitly.
func grillWithDocs(conversation Conversation) []string {
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

func toSpec(repo Repository, resolved []string) string {
	return "# Spec: " + repo.FullName + " intake\n\n## Resolved conversation\n\n" + strings.Join(resolved, "\n") + "\n\n## Scope\n\nImplement the smallest vertical slice that satisfies the resolved conversation.\n\n## Non-goals\n\nDo not expand beyond the confirmed intake."
}

func toTickets(repo Repository, resolved []string) string {
	var tickets strings.Builder
	fmt.Fprintf(&tickets, "# Ticket set: %s\n", repo.FullName)
	for index, decision := range resolved {
		title := ticketTitle(decision)
		blocker := "none"
		if index > 0 {
			title = "extend the confirmed vertical slice"
			blocker = "Ticket " + strconv.Itoa(index)
		}
		fmt.Fprintf(&tickets, "\n## Ticket %d: %s\n\nBlocked by: %s\n\n### Acceptance criteria\n\n%s\n\nThis ticket is intentionally a tracer-bullet slice: it crosses the relevant product seam end to end.\n", index+1, title, blocker, decision)
	}
	return strings.TrimSpace(tickets.String())
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
	c := Conversation{RepositoryID: id}
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
	err := a.db.QueryRowContext(ctx, `SELECT state,clone_path,message_start,pending_question,issue_number,issue_url FROM intakes WHERE repository_id=?`, id).Scan(&status.State, &status.Path, &status.MessageStart, &status.PendingQuestion, &number, &status.PublishedIssue.URL)
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

func (a *application) confirmed(ctx context.Context, id string, kind artifactKind) bool {
	var confirmed sql.NullString
	err := a.db.QueryRowContext(ctx, `SELECT confirmed_at FROM intake_artifacts WHERE repository_id=? AND kind=?`, id, kind).Scan(&confirmed)
	return err == nil && confirmed.Valid
}
func (a *application) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), 500)
	}
}

const pageTemplate = `{{define "repositories"}}<!doctype html><html><head><script src="https://unpkg.com/htmx.org@2.0.4"></script><script>document.addEventListener("htmx:configRequest",function(e){var m=document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/);if(m)e.detail.headers["X-XSRF-TOKEN"]=decodeURIComponent(m[1])})</script><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/core@1.4.0/dist/css/tabler.min.css"></head><body class="p-4"><main class="container-xl"><h1 class="mb-3">Repositories</h1>{{range .}}<form class="mb-2" method="post" action="/repositories/{{.ID}}"><button class="btn btn-primary">{{.FullName}}</button></form>{{end}}</main></body></html>{{end}}
{{define "workspace"}}<!doctype html><html><head><script src="https://unpkg.com/htmx.org@2.0.4"></script><script src="https://unpkg.com/htmx-ext-sse@2.2.2/sse.js"></script><script>document.addEventListener("htmx:configRequest",function(e){var m=document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/);if(m)e.detail.headers["X-XSRF-TOKEN"]=decodeURIComponent(m[1])})</script><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/core@1.4.0/dist/css/tabler.min.css"></head><body class="p-4"><main class="container-xl"><aside id="pm-status" class="card mb-3" role="status" aria-live="polite"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}}{{if .LastReply.Text}} · Tokens: {{.LastReply.Tokens}} · Cost: {{printf "%.4f" .LastReply.CostUSD}}{{end}}</div></aside><section id="conversation" class="card mb-3"><div class="card-body">{{range .Messages}}<article class="mb-2"><strong>{{.Role}}:</strong> {{.Text}}</article>{{end}}{{range .PendingTurns}}{{template "stream-start" .}}{{end}}</div></section><form class="card card-body mb-3" method="post" action="/repositories/{{.RepositoryID}}/conversation" hx-post="/repositories/{{.RepositoryID}}/conversation" hx-target="#conversation .card-body" hx-swap="beforeend" hx-disabled-elt="#pm-send"><label class="form-label" for="pm-message">Message the PM</label><input id="pm-message" class="form-control mb-2" name="message"><button id="pm-send" class="btn btn-primary"{{if .HasPending}} disabled{{end}}>Send</button></form><section class="card mb-3" aria-labelledby="intake-title"><div class="card-body"><h2 id="intake-title" class="card-title">Tracked-work intake</h2><p>State: {{if .Intake.State}}{{.Intake.State}}{{else}}not started{{end}}</p>{{if not .Intake.State}}<form method="post" action="/repositories/{{.RepositoryID}}/intake/start"><button class="btn btn-outline-primary">Start isolated intake</button></form>{{else if eq .Intake.State "draft"}}<form class="d-inline" method="post" action="/repositories/{{.RepositoryID}}/intake/complete-discovery"><button class="btn btn-outline-primary">Complete discovery</button></form><form class="d-inline" method="post" action="/repositories/{{.RepositoryID}}/intake/abandon"><button class="btn btn-outline-danger">Abandon intake</button></form><form class="card card-body mt-3" method="post" action="/repositories/{{.RepositoryID}}/intake/adr"><label>Decision <input class="form-control" name="decision" required></label><label>Alternative <input class="form-control" name="alternative" required></label><label>Trade-off <input class="form-control" name="tradeoff" required></label><label><input type="checkbox" name="consequential" value="true" required> Consequential</label><label><input type="checkbox" name="hard_to_reverse" value="true" required> Hard to reverse</label><button class="btn btn-outline-primary mt-2">Propose ADR</button></form>{{else if eq .Intake.State "ready"}}<form class="d-inline" method="post" action="/repositories/{{.RepositoryID}}/intake/synthesize"><button class="btn btn-outline-primary">Synthesize drafts</button></form>{{else if eq .Intake.State "confirmed"}}<form method="post" action="/repositories/{{.RepositoryID}}/intake/publish"><button class="btn btn-primary">Publish confirmed tickets to GitHub</button></form>{{else if eq .Intake.State "promoting"}}<form method="post" action="/repositories/{{.RepositoryID}}/intake/publish"><button class="btn btn-primary">Retry intake promotion</button></form>{{end}}{{range .Artifacts}}<article class="card card-sm mt-3"><div class="card-body"><h3 class="h4">{{.Kind}}{{if .Confirmed}} <span class="badge bg-green">confirmed</span>{{end}}</h3><pre class="mb-2 text-wrap">{{.Body}}</pre>{{if .URL}}<a href="{{.URL}}">Published GitHub issue</a>{{end}}{{if and (or (eq .Kind "spec") (eq .Kind "tickets")) (not .Confirmed)}}<form method="post" action="/repositories/{{$.RepositoryID}}/intake/{{.Kind}}/confirm"><button class="btn btn-outline-primary">Confirm {{.Kind}}</button></form>{{end}}</div></article>{{end}}</div></section><form method="post" action="/notifications/test"><button class="btn btn-outline-secondary">Test Telegram notification</button></form></main></body></html>{{end}}
{{define "turn"}}<article class="card card-body mb-2"><strong>pm:</strong> {{.Reply.Text}}</article><aside id="pm-status" class="card mb-3" role="status" aria-live="polite" hx-swap-oob="true"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}} · Tokens: {{.Reply.Tokens}} · Cost: {{printf "%.4f" .Reply.CostUSD}}</div></aside>{{end}}
{{define "stream-start"}}<div id="pending-turn-{{.TurnID}}" class="card card-body mb-2" hx-ext="sse" sse-connect="/repositories/{{.RepositoryID}}/conversation/{{.TurnID}}/stream" sse-swap="chunk,done,error" sse-close="done" hx-swap="innerHTML"><strong>pm:</strong> Thinking…</div>{{if .Sending}}<button id="pm-send" class="btn btn-primary" disabled hx-swap-oob="outerHTML">Sending…</button>{{end}}{{end}}
{{define "streamed-turn"}}<article id="pending-turn-{{.TurnID}}" class="card card-body mb-2" hx-swap-oob="outerHTML"><strong>pm:</strong> {{.Reply.Text}}</article><button id="pm-send" class="btn btn-primary" hx-swap-oob="outerHTML">Send</button><aside id="pm-status" class="card mb-3" role="status" aria-live="polite" hx-swap-oob="true"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}} · Tokens: {{.Reply.Tokens}} · Cost: {{printf "%.4f" .Reply.CostUSD}}</div></aside>{{end}}`
