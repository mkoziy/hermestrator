// Package live contains production adapters for the dashboard's small ports.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/compat_oai"
	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// GitHub lists repositories visible to the automation identity, never to the
// operator's OAuth identity.
type GitHub struct {
	Token  string
	Client *http.Client
}

func (g GitHub) Repositories(ctx context.Context) ([]dashboard.Repository, error) {
	if g.Token == "" {
		return nil, fmt.Errorf("GitHub automation token is not configured")
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/repos?per_page=100&sort=full_name", nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list GitHub repositories: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list GitHub repositories: %s", resp.Status)
	}
	var values []struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&values); err != nil {
		return nil, fmt.Errorf("decode GitHub repositories: %w", err)
	}
	repos := make([]dashboard.Repository, 0, len(values))
	for _, value := range values {
		repos = append(repos, dashboard.Repository{ID: fmt.Sprint(value.ID), FullName: value.FullName})
	}
	return repos, nil
}

// OpenRouterModel makes Genkit model calls through OpenRouter's OpenAI-compatible API.
type OpenRouterModel struct {
	genkit *genkit.Genkit
}

func NewOpenRouterModel(ctx context.Context, apiKey, model string) (*OpenRouterModel, error) {
	if apiKey == "" || model == "" {
		return nil, fmt.Errorf("OpenRouter API key and discovery model are required")
	}
	g := genkit.Init(ctx,
		genkit.WithPlugins(&compat_oai.OpenAICompatible{Provider: "openrouter", APIKey: apiKey, BaseURL: "https://openrouter.ai/api/v1"}),
		genkit.WithDefaultModel("openrouter/"+model),
	)
	return &OpenRouterModel{genkit: g}, nil
}

func (m *OpenRouterModel) Reply(ctx context.Context, conversation dashboard.Conversation, prompt string) (dashboard.Reply, error) {
	return m.Stream(ctx, conversation, prompt, func(string) error { return nil })
}

func (m *OpenRouterModel) Stream(ctx context.Context, conversation dashboard.Conversation, prompt string, emit func(string) error) (dashboard.Reply, error) {
	messages := make([]*ai.Message, 0, len(conversation.Messages)+1)
	for _, message := range conversation.Messages {
		role := ai.RoleUser
		if message.Role == "pm" {
			role = ai.RoleModel
		}
		messages = append(messages, ai.NewMessage(role, nil, ai.NewTextPart(message.Text)))
	}
	if len(conversation.Messages) == 0 || conversation.Messages[len(conversation.Messages)-1].Text != prompt {
		messages = append(messages, ai.NewUserMessage(ai.NewTextPart(prompt)))
	}
	response, err := genkit.Generate(ctx, m.genkit, ai.WithMessages(messages...), ai.WithStreaming(func(_ context.Context, chunk *ai.ModelResponseChunk) error {
		if chunk.Text() == "" {
			return nil
		}
		return emit(chunk.Text())
	}))
	if err != nil {
		return dashboard.Reply{}, fmt.Errorf("genkit OpenRouter generation: %w", err)
	}
	reply := dashboard.Reply{Text: response.Text()}
	if response.Usage != nil {
		reply.Tokens = response.Usage.InputTokens + response.Usage.OutputTokens
		if cost, ok := response.Usage.Custom["cost"]; ok {
			reply.CostUSD = cost
		}
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
			users[user] = true
		}
	}
	return users
}
