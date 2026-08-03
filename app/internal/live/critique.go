package live

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/firebase/genkit/go/ai"
	aix "github.com/firebase/genkit/go/ai/exp"
	"github.com/firebase/genkit/go/genkit"
	genkitx "github.com/firebase/genkit/go/genkit/exp"
	"github.com/mkoziy/hermestrator/internal/dashboard"
	"github.com/openai/openai-go"
)

// MaxCritiqueRounds caps plan regeneration so critique does not loop
// indefinitely. Once exceeded the run is surfaced as blocked.
const MaxCritiqueRounds = 3

// CritiqueResult captures the final outcome of plan critique.
type CritiqueResult struct {
	Approved           bool
	Findings           string
	RegenerationRounds int
	Blocked            bool
}

// CritiqueModelFunc calls a model to evaluate a plan. previousFindings
// accumulates material findings from earlier critique rounds so the model
// can consider them when re-evaluating a regenerated plan. An empty
// response with no error means the plan is approved.
//
// Tests inject a fake; production uses NewCritiqueModelFunc.
type CritiqueModelFunc func(ctx context.Context, planContent string, previousFindings string) (string, error)

// Critiquer evaluates a generated plan, triggers Planner regeneration on
// material findings, and caps rounds at MaxCritiqueRounds. It never edits
// the plan file by hand — regeneration always goes through the Planner.
type Critiquer struct {
	Planner   *Planner
	ModelFunc CritiqueModelFunc
}

// CritiquePlan reads the plan at dashboard.PlanFileName inside
// workspacePath, calls the critique model, and regenerates via the Planner
// up to MaxCritiqueRounds when material findings are returned. If all
// rounds are exhausted the result is marked Blocked.
//
// scope and executorKind are forwarded to Planner.GeneratePlan on
// regeneration; executorKind does not control planning (the Planner
// always uses codex or pi as directed by its profile).
func (c *Critiquer) CritiquePlan(ctx context.Context, workspacePath string, executorKind dashboard.ExecutorKind, scope string) (*CritiqueResult, error) {
	planPath := filepath.Join(workspacePath, dashboard.PlanFileName)
	if c.ModelFunc == nil {
		return nil, fmt.Errorf("critique model function is not configured")
	}

	var accumulatedFindings string
	for round := 1; round <= MaxCritiqueRounds; round++ {
		content, err := os.ReadFile(planPath)
		if err != nil {
			return nil, fmt.Errorf("read plan for critique round %d: %w", round, err)
		}

		response, err := c.ModelFunc(ctx, string(content), accumulatedFindings)
		if err != nil {
			return nil, fmt.Errorf("critique model call round %d: %w", round, err)
		}

		findings, approved := parseCritiqueResponse(response)
		if approved {
			return &CritiqueResult{
				Approved:           true,
				Findings:           accumulatedFindings,
				RegenerationRounds: round - 1,
			}, nil
		}

		if findings == "" {
			findings = response
		}
		accumulatedFindings = joinFindings(accumulatedFindings, findings)

		// Final round exhausted — surface as blocked instead of
		// regenerating again.
		if round == MaxCritiqueRounds {
			return &CritiqueResult{
				Approved:           false,
				Findings:           accumulatedFindings,
				RegenerationRounds: MaxCritiqueRounds,
				Blocked:            true,
			}, nil
		}

		// Material findings — regenerate the plan through the Planner.
		// The Planner writes the new plan to PlanFileName inside the
		// workspace; we never hand-edit the file.
		if c.Planner == nil {
			return &CritiqueResult{
				Approved: false,
				Findings: accumulatedFindings,
				Blocked:  true,
			}, nil
		}
		if _, err = c.Planner.GeneratePlan(ctx, workspacePath, executorKind, scope); err != nil {
			return nil, fmt.Errorf("regenerate plan after critique round %d: %w", round, err)
		}
	}

	// Should not be reachable; all paths return inside the loop.
	return &CritiqueResult{
		Approved:           false,
		Findings:           accumulatedFindings,
		RegenerationRounds: MaxCritiqueRounds,
		Blocked:            true,
	}, nil
}

// parseCritiqueResponse examines the model response for an explicit
// approval signal. Returns (findings, approved).
func parseCritiqueResponse(response string) (string, bool) {
	lower := strings.ToLower(response)

	// Explicit approval keywords.
	if strings.HasPrefix(strings.TrimSpace(lower), "approved") {
		return "", true
	}
	if strings.Contains(lower, "no material findings") ||
		strings.Contains(lower, "plan is approved") ||
		strings.Contains(lower, "plan looks good") {
		return "", true
	}

	return response, false
}

// joinFindings appends new findings to the accumulated set, separated by a
// visible delimiter so the model can distinguish prior rounds.
func joinFindings(existing, additional string) string {
	if existing == "" {
		return additional
	}
	if additional == "" {
		return existing
	}
	return existing + "\n\n--- previous round findings above ---\n\n" + additional
}

// critiqueSystemPrompt is the system message sent to the model when
// evaluating a plan for critique.
const critiqueSystemPrompt = `You are a plan reviewer. Evaluate the following plan for:
- Premise: does it rest on correct, verified assumptions?
- Logic: are the tasks ordered correctly with clear dependencies?
- Blind spots: what important concerns are missing?
- Effort: is the scope realistic for each task?
- Execution risk: what could go wrong during implementation?

If the plan is acceptable, respond with "Approved" on the first line.
If you find material issues, describe each finding concisely.`

// NewCritiqueModelFunc returns a CritiqueModelFunc backed by a Genkit
// agent, matching the discoveryAgent shape. g must already be initialised
// with the desired model plugin. modelName is the Genkit model identifier
// (e.g. "openrouter/anthropic/claude-sonnet-4.5").
func NewCritiqueModelFunc(g *genkit.Genkit, modelName string, critiqueMaxTokens int) CritiqueModelFunc {
	if critiqueMaxTokens <= 0 {
		critiqueMaxTokens = 1024
	}
	return func(ctx context.Context, planContent string, previousFindings string) (string, error) {
		var responseText string
		for result, err := range genkit.GenerateStream(ctx, g,
			ai.WithModelName(modelName),
			ai.WithMessages(
				ai.NewSystemTextMessage(critiqueSystemPrompt),
				ai.NewUserTextMessage(buildCritiquePrompt(planContent, previousFindings)),
			),
			ai.WithConfig(&openai.ChatCompletionNewParams{
				MaxCompletionTokens: openai.Int(int64(critiqueMaxTokens)),
			}),
		) {
			if err != nil {
				return "", fmt.Errorf("critique model stream: %w", err)
			}
			if result.Done {
				responseText = result.Response.Message.Text()
				break
			}
		}
		if responseText == "" {
			return "", fmt.Errorf("critique model returned empty response")
		}
		return redactSecrets(responseText), nil
	}
}

// buildCritiquePrompt builds the user message for a critique round, including
// the plan content and any accumulated findings from prior rounds.
func buildCritiquePrompt(planContent string, previousFindings string) string {
	if previousFindings == "" {
		return fmt.Sprintf("Evaluate this plan:\n\n%s", planContent)
	}
	return fmt.Sprintf(
		"The previous plan was critiqued and the following findings were identified. A new plan has been generated. Evaluate it:\n\n"+
			"Previous findings:\n%s\n\n"+
			"New plan:\n%s",
		previousFindings, planContent,
	)
}

// CritiqueState is durable agent-owned state for a critique session. It
// matches the pattern of PMState in adapters.go so the Genkit session store
// can persist critique progress.
type CritiqueState struct {
	PlanContent string `json:"planContent"`
	Round       int    `json:"round"`
	Findings    string `json:"findings"`
	Approved    bool   `json:"approved"`
	Blocked     bool   `json:"blocked"`
}

// CritiqueAgent is a Genkit agent that evaluates a plan, matching the shape
// of discoveryAgent in adapters.go. It is used when the operator wants a
// multi-turn critique session rather than the automated
// Critiquer.CritiquePlan loop.
type CritiqueAgent struct {
	agent *aix.Agent[CritiqueState]
	store *SQLiteSessionStore[CritiqueState]
}

// NewCritiqueAgent creates a Genkit agent for plan critique backed by the
// given session store. It mirrors the OpenRouterModel / discoveryAgent
// pattern in adapters.go.
func NewCritiqueAgent(g *genkit.Genkit, modelName string, store *SQLiteSessionStore[CritiqueState], critiqueMaxTokens int) (*CritiqueAgent, error) {
	if critiqueMaxTokens <= 0 {
		critiqueMaxTokens = 1024
	}
	agent := genkitx.DefineCustomAgent(g, "plan-critique",
		critiqueAgentFunc(g, modelName, critiqueMaxTokens),
		aix.WithSessionStore(store),
	)
	return &CritiqueAgent{agent: agent, store: store}, nil
}

// critiqueAgentFunc returns an aix.AgentFunc that evaluates a plan via
// Genkit's model streaming. It matches the shape of discoveryAgent in
// adapters.go.
func critiqueAgentFunc(g *genkit.Genkit, modelName string, maxTokens int) aix.AgentFunc[CritiqueState] {
	return func(ctx context.Context, responder aix.Responder, session *aix.SessionRunner[CritiqueState]) (*aix.AgentResult, error) {
		var message *ai.Message
		err := session.Run(ctx, func(ctx context.Context, input *aix.AgentInput) (*aix.TurnResult, error) {
			session.UpdateCustom(func(state CritiqueState) CritiqueState {
				state.Round++
				return state
			})

			messages := append(
				[]*ai.Message{ai.NewSystemTextMessage(critiqueSystemPrompt)},
				session.Messages()...,
			)
			messages = append(messages, input.Message)

			var reason aix.AgentFinishReason
			for result, err := range genkit.GenerateStream(ctx, g,
				ai.WithModelName(modelName),
				ai.WithMessages(messages...),
				ai.WithConfig(&openai.ChatCompletionNewParams{
					MaxCompletionTokens: openai.Int(int64(maxTokens)),
				}),
			) {
				if err != nil {
					return nil, fmt.Errorf("critique generate: %w", err)
				}
				if result.Done {
					message = ai.NewModelTextMessage(redactSecrets(result.Response.Message.Text()))
					reason = aix.AgentFinishReason(result.Response.FinishReason)
					responder.SendArtifact(&aix.Artifact{
						Name:  "critique-response.md",
						Parts: []*ai.Part{ai.NewTextPart(message.Text())},
					})
					break
				}
				responder.SendModelChunk(result.Chunk)
			}

			session.AddMessages(input.Message, message)

			_, approved := parseCritiqueResponse(message.Text())
			session.UpdateCustom(func(state CritiqueState) CritiqueState {
				state.PlanContent = input.Message.Text()
				state.Findings = joinFindings(state.Findings, message.Text())
				state.Approved = approved
				state.Blocked = state.Round >= MaxCritiqueRounds && !approved
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

// Close releases the underlying Genkit session store.
func (a *CritiqueAgent) Close() error {
	if a.store != nil {
		return a.store.Close()
	}
	return nil
}
