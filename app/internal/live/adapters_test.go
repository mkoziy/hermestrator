package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firebase/genkit/go/ai"
	aix "github.com/firebase/genkit/go/ai/exp"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"
	genkitx "github.com/firebase/genkit/go/genkit/exp"
	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestTelegramPayloadIncludesInlineDashboardLink(t *testing.T) {
	payload, err := telegramPayload("123", dashboard.Notification{Text: "PM dashboard test notification.", URL: "https://pm.example/repositories"})
	if err != nil {
		t.Fatal(err)
	}
	var message telegramRequest
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	if message.ChatID != "123" || message.Text != "PM dashboard test notification. Open dashboard" || len(message.Entities) != 1 || message.Entities[0] != (telegramEntity{Type: "text_link", Offset: 32, Length: 14, URL: "https://pm.example/repositories"}) {
		t.Fatalf("message = %#v", message)
	}
}

func TestTelegramNotifySendsInlineDashboardLink(t *testing.T) {
	requests := make(chan telegramRequest, 1)
	decodeErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var message telegramRequest
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			decodeErrors <- err
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- message
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	err := (Telegram{BotToken: "bot-token", ChatID: "123", Client: server.Client(), Endpoint: server.URL}).Notify(context.Background(), dashboard.Notification{Text: "PM dashboard test notification.", URL: "https://pm.example/repositories"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-decodeErrors:
		t.Fatal(err)
	case message := <-requests:
		if message.Text != "PM dashboard test notification. Open dashboard" || len(message.Entities) != 1 || message.Entities[0].URL != "https://pm.example/repositories" {
			t.Fatalf("message = %#v", message)
		}
	default:
		t.Fatal("Telegram request was not captured")
	}
}

func TestTelegramNotifyRejectsUnreachableDashboardURL(t *testing.T) {
	err := (Telegram{BotToken: "bot-token", ChatID: "123"}).Notify(context.Background(), dashboard.Notification{Text: "PM dashboard test notification.", URL: "http://localhost:8080/repositories"})
	if err == nil || !strings.Contains(err.Error(), "reachable HTTPS") {
		t.Fatalf("error = %v", err)
	}
}

func TestGitHubRepositoriesFollowsPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer automation-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/first":
			w.Header().Set("Link", "<"+serverURL(r)+"/second>; rel=\"next\"")
			_, _ = w.Write([]byte(`[{"id":1,"full_name":"acme/first"}]`))
		case "/second":
			_, _ = w.Write([]byte(`[{"id":2,"full_name":"acme/second"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repos, err := (GitHub{Token: "automation-token", ReposURL: server.URL + "/first"}).Repositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].FullName != "acme/first" || repos[1].FullName != "acme/second" {
		t.Fatalf("repositories = %#v", repos)
	}
}

func TestIsMissingSnapshot(t *testing.T) {
	if !isMissingSnapshot(core.NewError(core.NOT_FOUND, "no snapshot found")) {
		t.Fatal("NOT_FOUND snapshot error was not recognized")
	}
	if isMissingSnapshot(core.NewError(core.INTERNAL, "store unavailable")) {
		t.Fatal("non-NOT_FOUND error was recognized as a missing snapshot")
	}
}

func TestReplyFromOutputRejectsFailedAgentTurn(t *testing.T) {
	_, err := replyFromOutput(&aix.AgentOutput[PMState]{
		FinishReason: aix.AgentFinishReasonFailed,
		Error:        core.NewError(core.INTERNAL, "model unavailable"),
	})
	if err == nil || err.Error() != "genkit PM turn failed: model unavailable" {
		t.Fatalf("failed agent turn error = %v", err)
	}
}

func TestReplyFromOutputRejectsEmptyAgentTurn(t *testing.T) {
	_, err := replyFromOutput(&aix.AgentOutput[PMState]{Message: ai.NewModelTextMessage("")})
	if err == nil || err.Error() != "genkit PM turn completed without a response" {
		t.Fatalf("empty agent turn error = %v", err)
	}
}

func serverURL(r *http.Request) string { return "http://" + r.Host }

func TestGitHubNextPage(t *testing.T) {
	if got := githubNextPage(`<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=3>; rel="last"`); got != "https://api.github.com/user/repos?page=2" {
		t.Fatalf("next page = %q", got)
	}
}

func TestOpenRouterStatusDefaultsBeforeFirstTurn(t *testing.T) {
	model, err := NewOpenRouterModel(context.Background(), "test-key", "openai/gpt-4.1-mini", t.TempDir()+"/pm.db", CloneIntake{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = model.Close() })

	status, err := model.Status(context.Background(), "42")
	if err != nil {
		t.Fatalf("status before first turn: %v", err)
	}
	want := dashboard.Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "0s", RecentActivity: "awaiting discovery"}
	if status != want {
		t.Fatalf("status = %#v, want %#v", status, want)
	}
}

func TestDiscoveryToolCallCapsTurn(t *testing.T) {
	state := &discoveryTurnState{clonePath: t.TempDir()}
	ctx := context.WithValue(context.Background(), discoveryTurnStateKey{}, state)
	for call := 1; call <= MaxDiscoveryToolCalls; call++ {
		_, message := discoveryToolCall(ctx)
		if message != "" {
			t.Fatalf("call %d: message=%q", call, message)
		}
	}
	_, message := discoveryToolCall(ctx)
	if message != "tool budget exhausted" {
		t.Fatalf("over-budget call: message=%q", message)
	}
}

func TestDiscoveryToolResultRedactsSecrets(t *testing.T) {
	if got := redactSecrets("token ghp_abcdefghijklmnopqrstuvwxyz1234567890"); strings.Contains(got, "ghp_") {
		t.Fatalf("secret was not redacted: %q", got)
	}
}

func TestDiscoveryToolsCapCallsAndRedactModelFacingResults(t *testing.T) {
	base := t.TempDir()
	clone := filepath.Join(base, "intake")
	if err := os.Mkdir(clone, 0o750); err != nil {
		t.Fatal(err)
	}
	secret := "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	if err := os.WriteFile(filepath.Join(clone, secret+".txt"), []byte(secret+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "secrets.txt"), []byte(secret+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteSessionStore[PMState](t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	g := genkit.Init(context.Background(), genkit.WithExperimental())
	genkit.DefineModel(g, "openrouter/test-discovery", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true, SystemRole: true, Tools: true}}, func(_ context.Context, request *ai.ModelRequest, _ ai.ModelStreamCallback) (*ai.ModelResponse, error) {
		responses := toolResponses(request)
		for _, response := range responses {
			if output := fmt.Sprint(response.Output); strings.Contains(output, secret) {
				return nil, fmt.Errorf("tool result exposed a secret: %q", output)
			}
		}
		if len(responses) == MaxDiscoveryToolCalls+1 {
			budgetExhausted := 0
			for _, response := range responses {
				if fmt.Sprint(response.Output) == "tool budget exhausted" {
					budgetExhausted++
				}
			}
			if budgetExhausted != 1 {
				return nil, fmt.Errorf("budget exhausted responses = %d, want 1", budgetExhausted)
			}
			return &ai.ModelResponse{Request: request, Message: ai.NewModelTextMessage("completed"), FinishReason: ai.FinishReasonStop}, nil
		}
		if len(responses) != 0 {
			return nil, fmt.Errorf("tool responses = %d, want %d", len(responses), MaxDiscoveryToolCalls+1)
		}
		requests := make([]*ai.Part, 0, MaxDiscoveryToolCalls+1)
		for call := 0; call <= MaxDiscoveryToolCalls; call++ {
			name, input := "pm_discovery_read", map[string]any{"RelativePath": "secrets.txt"}
			switch call {
			case 0:
				name, input = "pm_discovery_context", map[string]any{}
			case 1:
				name, input = "pm_discovery_glob", map[string]any{"Pattern": "*.txt"}
			case 2:
				name, input = "pm_discovery_grep", map[string]any{"Pattern": "ghp_"}
			}
			requests = append(requests, ai.NewToolRequestPart(&ai.ToolRequest{Name: name, Input: input, Ref: fmt.Sprintf("call-%d", call+1)}))
		}
		return &ai.ModelResponse{Request: request, Message: ai.NewModelMessage(requests...), FinishReason: ai.FinishReasonStop}, nil
	})
	contextTool, globTool, grepTool, readTool := defineDiscoveryTools(g, CloneIntake{BaseDir: base})
	agent := genkitx.DefineCustomAgent(g, "discovery-tools-test", discoveryAgent(g, "test-discovery", contextTool, globTool, grepTool, readTool), aix.WithSessionStore(store))
	model := OpenRouterModel{agent: agent, store: store, genkit: g}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reply, err := model.Stream(ctx, dashboard.Conversation{RepositoryID: "42", ClonePath: clone}, "inspect repository", func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "completed" {
		t.Fatalf("reply = %#v", reply)
	}
}

func toolResponses(request *ai.ModelRequest) []*ai.ToolResponse {
	var responses []*ai.ToolResponse
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part.IsToolResponse() {
				responses = append(responses, part.ToolResponse)
			}
		}
	}
	return responses
}

func TestOpenRouterStreamFinishesAfterFirstAgentTurn(t *testing.T) {
	store, err := NewSQLiteSessionStore[PMState](t.TempDir() + "/pm.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	g := genkit.Init(context.Background(), genkit.WithExperimental())
	agent := genkitx.DefineCustomAgent(g, "one-turn", func(ctx context.Context, _ aix.Responder, session *aix.SessionRunner[PMState]) (*aix.AgentResult, error) {
		var reply *ai.Message
		err := session.Run(ctx, func(_ context.Context, input *aix.AgentInput) (*aix.TurnResult, error) {
			reply = ai.NewModelTextMessage("completed")
			session.AddMessages(input.Message, reply)
			return &aix.TurnResult{FinishReason: aix.AgentFinishReasonStop}, nil
		})
		return &aix.AgentResult{Message: reply}, err
	}, aix.WithSessionStore(store))
	model := OpenRouterModel{agent: agent, store: store}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	reply, err := model.Stream(ctx, dashboard.Conversation{RepositoryID: "42"}, "hello", func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "completed" {
		t.Fatalf("reply = %#v", reply)
	}
}
