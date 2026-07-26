// Package dashboard serves the operator-facing PM workspace.
package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Repository struct{ ID, FullName string }
type Message struct {
	Role, Text string
	CreatedAt  time.Time
}
type Conversation struct {
	RepositoryID string
	Messages     []Message
}
type Reply struct {
	Text    string
	Tokens  int
	CostUSD float64
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
type Telegram interface {
	Notify(context.Context, string) error
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
	templates *template.Template
}

func New(deps Dependencies) (http.Handler, error) {
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
		CREATE TABLE IF NOT EXISTS messages (repository_id TEXT NOT NULL, role TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS activity (repository_id TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);`); err != nil {
		_ = db.Close()
		return nil, err
	}
	t, err := template.New("page").Parse(pageTemplate)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	a := &application{deps: deps, db: db, templates: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories", a.repositories)
	mux.HandleFunc("POST /repositories/{id}", a.selectRepository)
	mux.HandleFunc("GET /repositories/{id}", a.workspace)
	mux.HandleFunc("POST /repositories/{id}/conversation", a.converse)
	mux.HandleFunc("POST /notifications/test", a.testNotification)
	return a.authorized(mux), nil
}

func (a *application) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("X-PM-User")
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
	a.render(w, "workspace", c)
}
func (a *application) converse(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	prompt := strings.TrimSpace(r.Form.Get("message"))
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
	if _, err = a.db.ExecContext(r.Context(), `INSERT INTO messages VALUES(?,?,?,?)`, id, "operator", prompt, now.Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "could not save message", 500)
		return
	}
	c.Messages = append(c.Messages, Message{Role: "operator", Text: prompt, CreatedAt: now})
	stream, streaming := a.deps.Model.(StreamingModel)
	var reply Reply
	if streaming {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if _, err := w.Write([]byte(`<div id="pm-stream" hx-swap-oob="innerHTML:#pm-stream"></div>`)); err != nil {
			return
		}
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		var text strings.Builder
		reply, err = stream.Stream(r.Context(), c, prompt, func(chunk string) error {
			text.WriteString(chunk)
			if _, err := fmt.Fprintf(w, `<span id="pm-stream" hx-swap-oob="innerHTML:#pm-stream">%s</span>`, template.HTMLEscapeString(text.String())); err != nil {
				return err
			}
			if flush, ok := w.(http.Flusher); ok {
				flush.Flush()
			}
			return nil
		})
		if err == nil && reply.Text == "" {
			reply.Text = text.String()
		}
	} else {
		reply, err = a.deps.Model.Reply(r.Context(), c, prompt)
	}
	if err != nil {
		if !streaming {
			http.Error(w, "model response unavailable", http.StatusBadGateway)
		}
		return
	}
	if _, err = a.db.ExecContext(r.Context(), `INSERT INTO messages VALUES(?,?,?,?)`, id, "pm", reply.Text, now.Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "could not save response", 500)
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO activity VALUES(?,?,?)`, id, "discovery turn completed", now.Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "could not save activity", http.StatusInternalServerError)
		return
	}
	if streaming {
		a.render(w, "streamed-turn", struct {
			Reply Reply
		}{reply})
		return
	}
	a.render(w, "turn", struct {
		Reply   Reply
		Phase   string
		Elapsed string
	}{reply, "discovery", "0s"})
}
func (a *application) testNotification(w http.ResponseWriter, r *http.Request) {
	if a.deps.Telegram != nil {
		base := strings.TrimRight(a.deps.DashboardURL, "/")
		if base == "" {
			base = "http://localhost:8080"
		}
		if err := a.deps.Telegram.Notify(r.Context(), "PM dashboard test notification: "+base+"/repositories"); err != nil {
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
	rows, err := a.db.QueryContext(ctx, `SELECT role,text,created_at FROM messages WHERE repository_id=? ORDER BY rowid`, id)
	if err != nil {
		return Conversation{}, err
	}
	defer func() { _ = rows.Close() }()
	c := Conversation{RepositoryID: id}
	for rows.Next() {
		var m Message
		var created string
		if err := rows.Scan(&m.Role, &m.Text, &created); err != nil {
			return Conversation{}, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		c.Messages = append(c.Messages, m)
	}
	return c, rows.Err()
}
func (a *application) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), 500)
	}
}

const pageTemplate = `{{define "repositories"}}<!doctype html><html><head><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/core@1.4.0/dist/css/tabler.min.css"></head><body class="p-4"><h1>Repositories</h1>{{range .}}<form method="post" action="/repositories/{{.ID}}"><button class="btn btn-primary">{{.FullName}}</button></form>{{end}}</body></html>{{end}}
{{define "workspace"}}<!doctype html><html><head><script src="https://unpkg.com/htmx.org@2.0.4"></script><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@tabler/core@1.4.0/dist/css/tabler.min.css"></head><body class="p-4"><aside>Phase: discovery · Model role: discovery · Elapsed: 0s · Recent activity: discovery</aside><main id="conversation">{{range .Messages}}<p><strong>{{.Role}}:</strong> {{.Text}}</p>{{end}}<div id="pm-stream"></div></main><form method="post" action="/repositories/{{.RepositoryID}}/conversation" hx-post="/repositories/{{.RepositoryID}}/conversation" hx-target="#conversation" hx-swap="beforeend"><input name="message" aria-label="Message the PM"><button>Send</button></form></body></html>{{end}}
{{define "turn"}}<p><strong>pm:</strong> {{.Reply.Text}}</p><aside>Phase: {{.Phase}} · Model role: discovery · Elapsed: {{.Elapsed}} · Recent activity: discovery · Tokens: {{.Reply.Tokens}} · Cost: {{printf "%.4f" .Reply.CostUSD}}</aside>{{end}}
{{define "streamed-turn"}}<p><strong>pm:</strong> {{.Reply.Text}}</p><aside>Phase: discovery · Model role: discovery · Tokens: {{.Reply.Tokens}} · Cost: {{printf "%.4f" .Reply.CostUSD}}</aside><div id="pm-stream" hx-swap-oob="innerHTML:#pm-stream"></div>{{end}}`
