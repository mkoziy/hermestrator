// Package live contains production adapters for the dashboard's small ports.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	aix "github.com/firebase/genkit/go/ai/exp"
	"github.com/firebase/genkit/go/genkit"
	genkitx "github.com/firebase/genkit/go/genkit/exp"
	"github.com/firebase/genkit/go/plugins/compat_oai"
	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// GitHub lists repositories visible to the automation identity, never to the
// operator's OAuth identity.
type GitHub struct {
	Token    string
	Client   *http.Client
	ReposURL string
}

func (g GitHub) Repositories(ctx context.Context) ([]dashboard.Repository, error) {
	if g.Token == "" {
		return nil, fmt.Errorf("GitHub automation token is not configured")
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	next := g.ReposURL
	if next == "" {
		next = "https://api.github.com/user/repos?per_page=100&sort=full_name"
	}
	repos := []dashboard.Repository{}
	for next != "" {
		values, pageNext, err := g.repositoriesPage(ctx, client, next)
		if err != nil {
			return nil, err
		}
		repos = append(repos, values...)
		next = pageNext
	}
	return repos, nil
}

func (g GitHub) repositoriesPage(ctx context.Context, client *http.Client, pageURL string) ([]dashboard.Repository, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create GitHub request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("list GitHub repositories: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("list GitHub repositories: %s", resp.Status)
	}
	var values []struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&values); err != nil {
		return nil, "", fmt.Errorf("decode GitHub repositories: %w", err)
	}
	repos := make([]dashboard.Repository, 0, len(values))
	for _, value := range values {
		repos = append(repos, dashboard.Repository{ID: fmt.Sprint(value.ID), FullName: value.FullName})
	}
	return repos, githubNextPage(resp.Header.Get("Link")), nil
}

func githubNextPage(header string) string {
	for _, link := range strings.Split(header, ",") {
		parts := strings.Split(link, ";")
		if len(parts) < 2 || !strings.Contains(parts[1], `rel="next"`) {
			continue
		}
		value := strings.TrimSpace(parts[0])
		if len(value) < 3 || value[0] != '<' || value[len(value)-1] != '>' {
			continue
		}
		if _, err := url.ParseRequestURI(value[1 : len(value)-1]); err == nil {
			return value[1 : len(value)-1]
		}
	}
	return ""
}

// OpenRouterModel makes Genkit model calls through OpenRouter's OpenAI-compatible API.
type OpenRouterModel struct {
	agent *aix.Agent[PMState]
	store *SQLiteSessionStore[PMState]
}

// PMState is durable agent-owned state for a repository discovery session.
type PMState struct {
	Phase        string    `json:"phase"`
	ModelRole    string    `json:"modelRole"`
	StartedAt    time.Time `json:"startedAt"`
	LastActivity string    `json:"lastActivity"`
	Tokens       int       `json:"tokens"`
	CostUSD      float64   `json:"costUSD"`
}

func NewOpenRouterModel(ctx context.Context, apiKey, model, storePath string) (*OpenRouterModel, error) {
	if apiKey == "" || model == "" {
		return nil, fmt.Errorf("OpenRouter API key and discovery model are required")
	}
	g := genkit.Init(ctx,
		genkit.WithExperimental(),
		genkit.WithPlugins(&compat_oai.OpenAICompatible{Provider: "openrouter", APIKey: apiKey, BaseURL: "https://openrouter.ai/api/v1"}),
		genkit.WithDefaultModel("openrouter/"+model),
	)
	store, err := NewSQLiteSessionStore[PMState](storePath)
	if err != nil {
		return nil, err
	}
	agent := genkitx.DefineCustomAgent(g, "pm-discovery", discoveryAgent(g, model), aix.WithSessionStore(store))
	return &OpenRouterModel{agent: agent, store: store}, nil
}

func discoveryAgent(g *genkit.Genkit, model string) aix.AgentFunc[PMState] {
	return func(ctx context.Context, responder aix.Responder, session *aix.SessionRunner[PMState]) (*aix.AgentResult, error) {
		var message *ai.Message
		err := session.Run(ctx, func(ctx context.Context, input *aix.AgentInput) (*aix.TurnResult, error) {
			session.UpdateCustom(func(state PMState) PMState {
				if state.StartedAt.IsZero() {
					state.StartedAt = time.Now().UTC()
				}
				state.Phase = "discovery"
				state.ModelRole = "discovery"
				state.LastActivity = "discovery turn in progress"
				return state
			})
			messages := append([]*ai.Message{ai.NewSystemTextMessage("You are a product manager. Ask one focused discovery question at a time.")}, session.Messages()...)
			messages = append(messages, input.Message)
			var reason aix.AgentFinishReason
			for result, err := range genkit.GenerateStream(ctx, g, ai.WithModelName("openrouter/"+model), ai.WithMessages(messages...)) {
				if err != nil {
					return nil, fmt.Errorf("generate discovery response: %w", err)
				}
				if result.Done {
					message = result.Response.Message
					reason = aix.AgentFinishReason(result.Response.FinishReason)
					session.AddMessages(input.Message, message)
					session.UpdateCustom(func(state PMState) PMState {
						if result.Response.Usage != nil {
							state.Tokens = result.Response.Usage.InputTokens + result.Response.Usage.OutputTokens
							state.CostUSD = result.Response.Usage.Custom["cost"]
						}
						return state
					})
					break
				}
				responder.SendModelChunk(result.Chunk)
			}
			session.UpdateCustom(func(state PMState) PMState {
				state.LastActivity = "discovery turn completed"
				return state
			})
			return &aix.TurnResult{FinishReason: reason}, nil
		})
		if err != nil {
			return nil, err
		}
		return &aix.AgentResult{Message: message}, nil
	}
}

func (m *OpenRouterModel) Close() error { return m.store.Close() }

func (m *OpenRouterModel) Status(ctx context.Context, repositoryID string) (dashboard.Status, error) {
	snapshot, err := m.agent.GetLatestSnapshot(ctx, "pm-"+repositoryID)
	if err != nil {
		return dashboard.Status{}, fmt.Errorf("load Genkit PM status: %w", err)
	}
	if snapshot == nil || snapshot.State == nil {
		return dashboard.Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "0s", RecentActivity: "awaiting discovery"}, nil
	}
	state := snapshot.State.Custom
	return dashboard.Status{Phase: state.Phase, ModelRole: state.ModelRole, Elapsed: time.Since(state.StartedAt).Round(time.Second).String(), RecentActivity: state.LastActivity}, nil
}

func (m *OpenRouterModel) Reply(ctx context.Context, conversation dashboard.Conversation, prompt string) (dashboard.Reply, error) {
	return m.Stream(ctx, conversation, prompt, func(string) error { return nil })
}

func (m *OpenRouterModel) Stream(ctx context.Context, conversation dashboard.Conversation, prompt string, emit func(string) error) (dashboard.Reply, error) {
	conn, err := m.agent.Connect(ctx, aix.WithSessionID[PMState]("pm-"+conversation.RepositoryID))
	if err != nil {
		return dashboard.Reply{}, fmt.Errorf("connect Genkit PM agent: %w", err)
	}
	if err := conn.SendText(prompt); err != nil {
		return dashboard.Reply{}, fmt.Errorf("send Genkit PM turn: %w", err)
	}
	for chunk, err := range conn.Receive() {
		if err != nil {
			return dashboard.Reply{}, fmt.Errorf("receive Genkit PM turn: %w", err)
		}
		if chunk.ModelChunk != nil && chunk.ModelChunk.Text() != "" {
			if err := emit(chunk.ModelChunk.Text()); err != nil {
				return dashboard.Reply{}, err
			}
		}
	}
	result, err := conn.Output()
	if err != nil {
		return dashboard.Reply{}, fmt.Errorf("complete Genkit PM turn: %w", err)
	}
	status, err := m.agent.GetLatestSnapshot(ctx, "pm-"+conversation.RepositoryID)
	if err != nil {
		return dashboard.Reply{}, fmt.Errorf("load completed Genkit PM turn: %w", err)
	}
	reply := dashboard.Reply{Text: result.Message.Text()}
	if status != nil && status.State != nil {
		reply.Tokens = status.State.Custom.Tokens
		reply.CostUSD = status.State.Custom.CostUSD
	}
	return reply, nil
}

// Telegram posts read-only notification messages through the Bot API.
type Telegram struct {
	BotToken string
	ChatID   string
	Client   *http.Client
}

func (t Telegram) Notify(ctx context.Context, message string) error {
	if t.BotToken == "" || t.ChatID == "" {
		return fmt.Errorf("Telegram bot token and chat ID are required")
	}
	payload, err := json.Marshal(map[string]string{"chat_id": t.ChatID, "text": message, "disable_web_page_preview": "true"})
	if err != nil {
		return fmt.Errorf("encode Telegram notification: %w", err)
	}
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+t.BotToken+"/sendMessage", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create Telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send Telegram notification: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("send Telegram notification: %s", resp.Status)
	}
	return nil
}

// AllowedUsers parses a comma-separated GitHub login allowlist.
func AllowedUsers(value string) map[string]bool {
	users := map[string]bool{}
	for _, user := range strings.Split(value, ",") {
		if user = strings.TrimSpace(user); user != "" {
			users[strings.ToLower(user)] = true
		}
	}
	return users
}
