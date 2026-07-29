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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

var secretPattern = regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9_-]{12,}|gh[pousr]_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,}|\d{6,}:[a-z0-9_-]{20,}|[a-z0-9_-]{20,}\.[a-z0-9_-]{20,}\.[a-z0-9_-]{20,})\b`)

type Repository struct{ ID, FullName string }
type Message struct {
	Role, Text string
	CreatedAt  time.Time
}
type Conversation struct {
	RepositoryID string
	Messages     []Message
	PendingTurns []PendingTurn
	LastReply    Reply
	Status       Status
}
type PendingTurn struct{ TurnID, RepositoryID string }
type Status struct{ Phase, ModelRole, Elapsed, RecentActivity string }
type Reply struct {
	Text    string
	Tokens  int
	CostUSD float64
}
type Notification struct{ Text, URL string }

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
	closed    bool
}

type Handler interface {
	http.Handler
	Close() error
}

func New(deps Dependencies) (Handler, error) {
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
		CREATE TABLE IF NOT EXISTS pending_turns (id TEXT PRIMARY KEY, repository_id TEXT NOT NULL, prompt TEXT NOT NULL, started_at TEXT, completed_at TEXT);
		CREATE TABLE IF NOT EXISTS turn_events (id INTEGER PRIMARY KEY AUTOINCREMENT, turn_id TEXT NOT NULL, event TEXT NOT NULL, data TEXT NOT NULL);`); err != nil {
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
	if _, err = db.Exec(`UPDATE pending_turns SET started_at=NULL WHERE completed_at IS NULL`); err != nil {
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
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s columns: %w", table, err)
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
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
	a.render(w, "workspace", c)
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
	now := time.Now().UTC()
	if _, err = a.db.ExecContext(r.Context(), `INSERT INTO messages(repository_id,role,text,created_at) VALUES(?,?,?,?)`, id, "operator", prompt, now.Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "could not save message", 500)
		return
	}
	c.Messages = append(c.Messages, Message{Role: "operator", Text: prompt, CreatedAt: now})
	if _, streaming := a.deps.Model.(StreamingModel); streaming && r.Header.Get("HX-Request") == "true" {
		turnID := uuid.NewString()
		if _, err = a.db.ExecContext(r.Context(), `INSERT INTO pending_turns(id,repository_id,prompt) VALUES(?,?,?)`, turnID, id, prompt); err != nil {
			http.Error(w, "could not start response stream", http.StatusInternalServerError)
			return
		}
		a.startTurn(id, turnID, prompt)
		a.render(w, "stream-start", struct{ RepositoryID, TurnID string }{id, turnID})
		return
	}
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

func (a *application) resumePendingTurns() error {
	rows, err := a.db.Query(`SELECT id,repository_id,prompt FROM pending_turns WHERE completed_at IS NULL`)
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
	result, err := a.db.Exec(`UPDATE pending_turns SET started_at=? WHERE id=? AND started_at IS NULL AND completed_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), turnID)
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
	var completedAt sql.NullString
	err := a.db.QueryRowContext(r.Context(), `SELECT repository_id,completed_at FROM pending_turns WHERE id=?`, turnID).Scan(&repositoryID, &completedAt)
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
		if err := a.db.QueryRowContext(r.Context(), `SELECT completed_at FROM pending_turns WHERE id=?`, turnID).Scan(&completedAt); err != nil || completedAt.Valid {
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
		var safe streamTextBuffer
		var reply Reply
		reply, err = a.reply(ctx, c, prompt, func(chunk string) error {
			text.WriteString(chunk)
			return safe.Append(chunk, func(visible string) error {
				return a.recordTurnEvent(ctx, turnID, "chunk", `<article class="card card-body py-2 mb-2"><strong>pm:</strong> `+template.HTMLEscapeString(visible)+`</article>`)
			})
		})
		if flushErr := safe.Flush(func(visible string) error {
			return a.recordTurnEvent(ctx, turnID, "chunk", `<article class="card card-body py-2 mb-2"><strong>pm:</strong> `+template.HTMLEscapeString(visible)+`</article>`)
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
			}{reply, status})
			if err == nil {
				err = a.recordTurnEvent(ctx, turnID, "done", fragment)
			}
		}
	}
	if err != nil {
		log.Printf("complete PM response for repository %q turn %q: %s", repositoryID, turnID, redactSecrets(err.Error()))
		_ = a.recordTurnEvent(ctx, turnID, "error", `<p class="text-danger">The PM response could not be completed.</p>`)
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE pending_turns SET completed_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), turnID)
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
	status := Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "0s", RecentActivity: "discovery turn completed"}
	if model, ok := a.deps.Model.(StatusModel); ok {
		return model.Status(ctx, id)
	}
	return status, nil
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

func redactSecrets(value string) string { return secretPattern.ReplaceAllString(value, "[redacted]") }

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
func (a *application) conversation(ctx context.Context, id string) (Conversation, error) {
	var exists string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM repositories WHERE id=?`, id).Scan(&exists); err != nil {
		return Conversation{}, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT role,text,tokens,cost_usd,created_at FROM messages WHERE repository_id=? ORDER BY rowid`, id)
	if err != nil {
		return Conversation{}, err
	}
	defer func() { _ = rows.Close() }()
	c := Conversation{RepositoryID: id}
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
	pending, err := a.db.QueryContext(ctx, `SELECT id FROM pending_turns WHERE repository_id=? AND completed_at IS NULL ORDER BY rowid`, id)
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
	return c, pending.Err()
}
func (a *application) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), 500)
	}
}

const pageTemplate = `{{define "repositories"}}<!doctype html><html><head><script src="https://unpkg.com/htmx.org@2.0.4"></script><script>document.addEventListener("htmx:configRequest",function(e){var m=document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/);if(m)e.detail.headers["X-XSRF-TOKEN"]=decodeURIComponent(m[1])})</script><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/core@1.4.0/dist/css/tabler.min.css"></head><body class="p-4"><main class="container-xl"><h1 class="mb-3">Repositories</h1>{{range .}}<form class="mb-2" method="post" action="/repositories/{{.ID}}"><button class="btn btn-primary">{{.FullName}}</button></form>{{end}}</main></body></html>{{end}}
{{define "workspace"}}<!doctype html><html><head><script src="https://unpkg.com/htmx.org@2.0.4"></script><script src="https://unpkg.com/htmx-ext-sse@2.2.2/sse.js"></script><script>document.addEventListener("htmx:configRequest",function(e){var m=document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]*)/);if(m)e.detail.headers["X-XSRF-TOKEN"]=decodeURIComponent(m[1])})</script><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/core@1.4.0/dist/css/tabler.min.css"></head><body class="p-4"><main class="container-xl"><aside id="pm-status" class="card mb-3" role="status" aria-live="polite"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}}{{if .LastReply.Text}} · Tokens: {{.LastReply.Tokens}} · Cost: {{printf "%.4f" .LastReply.CostUSD}}{{end}}</div></aside><section id="conversation" class="card mb-3"><div class="card-body">{{range .Messages}}<article class="mb-2"><strong>{{.Role}}:</strong> {{.Text}}</article>{{end}}{{range .PendingTurns}}{{template "stream-start" .}}{{end}}</div></section><form class="card card-body mb-3" method="post" action="/repositories/{{.RepositoryID}}/conversation" hx-post="/repositories/{{.RepositoryID}}/conversation" hx-target="#conversation .card-body" hx-swap="beforeend"><label class="form-label" for="pm-message">Message the PM</label><input id="pm-message" class="form-control mb-2" name="message"><button class="btn btn-primary">Send</button></form><form method="post" action="/notifications/test"><button class="btn btn-outline-secondary">Test Telegram notification</button></form></main></body></html>{{end}}
{{define "turn"}}<article class="card card-body mb-2"><strong>pm:</strong> {{.Reply.Text}}</article><aside id="pm-status" class="card mb-3" role="status" aria-live="polite" hx-swap-oob="true"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}} · Tokens: {{.Reply.Tokens}} · Cost: {{printf "%.4f" .Reply.CostUSD}}</div></aside>{{end}}
{{define "stream-start"}}<div id="pending-turn" class="card card-body mb-2" hx-ext="sse" sse-connect="/repositories/{{.RepositoryID}}/conversation/{{.TurnID}}/stream" sse-swap="chunk,done,error" sse-close="done"><strong>pm:</strong> Thinking…</div>{{end}}
{{define "streamed-turn"}}<article class="card card-body mb-2"><strong>pm:</strong> {{.Reply.Text}}</article><aside id="pm-status" class="card mb-3" role="status" aria-live="polite" hx-swap-oob="true"><div class="card-body">Phase: {{.Status.Phase}} · Model role: {{.Status.ModelRole}} · Elapsed: {{.Status.Elapsed}} · Recent activity: {{.Status.RecentActivity}} · Tokens: {{.Reply.Tokens}} · Cost: {{printf "%.4f" .Reply.CostUSD}}</div></aside>{{end}}`
