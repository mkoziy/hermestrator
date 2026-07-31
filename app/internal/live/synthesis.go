package live

import (
	"context"
	"encoding/json"
	"fmt"

	aix "github.com/firebase/genkit/go/ai/exp"
	"github.com/firebase/genkit/go/genkit"
	genkitx "github.com/firebase/genkit/go/genkit/exp"
	"github.com/mkoziy/hermestrator/internal/dashboard"
)

// specInput is the bounded input for the pm_to_spec Genkit tool.
type specInput struct {
	Repo     dashboard.Repository `json:"repo"`
	Resolved []string             `json:"resolved"`
}

// ticketsInput is the bounded input for the pm_to_tickets Genkit tool.
type ticketsInput struct {
	Repo     dashboard.Repository `json:"repo"`
	Resolved []string             `json:"resolved"`
}

// adrResult captures the two return values from AssessADR for the pm_assess_adr tool.
type adrResult struct {
	Assessment string `json:"assessment"`
	Proposal   string `json:"proposal"`
}

// GenkitSynthesizer implements dashboard.Synthesizer by wrapping four
// bounded Genkit tools. It is invoked deterministically (no LLM in the
// loop) via Tool.RunRaw.
type GenkitSynthesizer struct {
	grillWithDocs *aix.Tool[dashboard.Conversation, []string]
	toSpec        *aix.Tool[specInput, string]
	toTickets     *aix.Tool[ticketsInput, string]
	assessADR     *aix.Tool[string, adrResult]
}

// NewGenkitSynthesizer registers four pm_* tools against g and returns a
// GenkitSynthesizer that implements dashboard.Synthesizer.
func NewGenkitSynthesizer(g *genkit.Genkit) *GenkitSynthesizer {
	return &GenkitSynthesizer{
		grillWithDocs: genkitx.DefineTool(g, "pm_grill_with_docs",
			"Extracts settled operator decisions from completed discovery turns.",
			func(ctx context.Context, c dashboard.Conversation) ([]string, error) {
				return dashboard.GrillWithDocs(c), nil
			},
		),
		toSpec: genkitx.DefineTool(g, "pm_to_spec",
			"Produces a specification draft from a repository and its resolved decisions.",
			func(ctx context.Context, input specInput) (string, error) {
				return dashboard.ToSpec(input.Repo, input.Resolved), nil
			},
		),
		toTickets: genkitx.DefineTool(g, "pm_to_tickets",
			"Produces a tracer-bullet ticket breakdown from a repository and its resolved decisions.",
			func(ctx context.Context, input ticketsInput) (string, error) {
				return dashboard.ToTickets(input.Repo, input.Resolved), nil
			},
		),
		assessADR: genkitx.DefineTool(g, "pm_assess_adr",
			"Assesses whether a settled decision meets ADR eligibility criteria.",
			func(ctx context.Context, decision string) (adrResult, error) {
				assessment, proposal := dashboard.AssessADR(decision)
				return adrResult{Assessment: assessment, Proposal: proposal}, nil
			},
		),
	}
}

// GrillWithDocs calls pm_grill_with_docs via RunRaw.
func (s *GenkitSynthesizer) GrillWithDocs(ctx context.Context, c dashboard.Conversation) ([]string, error) {
	result, err := s.grillWithDocs.RunRaw(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("pm_grill_with_docs: %w", err)
	}
	return decodeToolResult[[]string](result)
}

// ToSpec calls pm_to_spec via RunRaw.
func (s *GenkitSynthesizer) ToSpec(ctx context.Context, repo dashboard.Repository, resolved []string) (string, error) {
	result, err := s.toSpec.RunRaw(ctx, specInput{Repo: repo, Resolved: resolved})
	if err != nil {
		return "", fmt.Errorf("pm_to_spec: %w", err)
	}
	return decodeToolResult[string](result)
}

// ToTickets calls pm_to_tickets via RunRaw.
func (s *GenkitSynthesizer) ToTickets(ctx context.Context, repo dashboard.Repository, resolved []string) (string, error) {
	result, err := s.toTickets.RunRaw(ctx, ticketsInput{Repo: repo, Resolved: resolved})
	if err != nil {
		return "", fmt.Errorf("pm_to_tickets: %w", err)
	}
	return decodeToolResult[string](result)
}

// AssessADR calls pm_assess_adr via RunRaw.
func (s *GenkitSynthesizer) AssessADR(ctx context.Context, decision string) (string, string, error) {
	result, err := s.assessADR.RunRaw(ctx, decision)
	if err != nil {
		return "", "", fmt.Errorf("pm_assess_adr: %w", err)
	}
	r, err := decodeToolResult[adrResult](result)
	if err != nil {
		return "", "", err
	}
	return r.Assessment, r.Proposal, nil
}

// decodeToolResult round-trips result through JSON to recover the concrete
// type T from RunRaw's JSON-decoded any. RunRaw internally json.Marshal's
// the input and json.Unmarshal's the output, so the returned value is a
// JSON-decoded representation (e.g. []interface{} for []string), not the
// concrete Out type. A bare type assertion would fail at runtime.
func decodeToolResult[T any](result any) (T, error) {
	var zero T
	data, err := json.Marshal(result)
	if err != nil {
		return zero, fmt.Errorf("marshal tool result: %w", err)
	}
	if err := json.Unmarshal(data, &zero); err != nil {
		return zero, fmt.Errorf("unmarshal tool result: %w", err)
	}
	return zero, nil
}
