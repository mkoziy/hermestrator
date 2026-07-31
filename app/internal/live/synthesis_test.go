package live

import (
	"context"
	"reflect"
	"testing"

	"github.com/firebase/genkit/go/genkit"
	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestGenkitSynthesizerDeepEqualWithDashboard(t *testing.T) {
	g := genkit.Init(context.Background(), genkit.WithExperimental())
	s := NewGenkitSynthesizer(g)
	ctx := context.Background()

	conv := dashboard.Conversation{
		RepositoryID: "42",
		Messages: []dashboard.Message{
			{Role: "operator", Text: "decision A"},
			{Role: "pm", Text: "acknowledged"},
			{Role: "operator", Text: "decision B"},
			{Role: "pm", Text: "acknowledged"},
		},
		PendingTurns: []dashboard.PendingTurn{},
	}
	gotGrill, err := s.GrillWithDocs(ctx, conv)
	if err != nil {
		t.Fatalf("GrillWithDocs: %v", err)
	}
	wantGrill := dashboard.GrillWithDocs(conv)
	if !reflect.DeepEqual(gotGrill, wantGrill) {
		t.Fatalf("GrillWithDocs:\n got  %#v\n want %#v", gotGrill, wantGrill)
	}

	repo := dashboard.Repository{ID: "1", FullName: "acme/project"}
	resolved := []string{"- decision A", "- decision B"}

	gotSpec, err := s.ToSpec(ctx, repo, resolved)
	if err != nil {
		t.Fatalf("ToSpec: %v", err)
	}
	wantSpec := dashboard.ToSpec(repo, resolved)
	if gotSpec != wantSpec {
		t.Fatalf("ToSpec:\n got  %q\n want %q", gotSpec, wantSpec)
	}

	gotTickets, err := s.ToTickets(ctx, repo, resolved)
	if err != nil {
		t.Fatalf("ToTickets: %v", err)
	}
	wantTickets := dashboard.ToTickets(repo, resolved)
	if gotTickets != wantTickets {
		t.Fatalf("ToTickets:\n got  %q\n want %q", gotTickets, wantTickets)
	}

	decision := "decision: use SQLite; alternative: use Postgres; trade-off: no extra infra; reversal cost: database migration required"
	gotAssessment, gotProposal, err := s.AssessADR(ctx, decision)
	if err != nil {
		t.Fatalf("AssessADR: %v", err)
	}
	wantAssessment, wantProposal := dashboard.AssessADR(decision)
	if gotAssessment != wantAssessment {
		t.Fatalf("AssessADR assessment mismatch")
	}
	if gotProposal != wantProposal {
		t.Fatalf("AssessADR proposal mismatch")
	}
}

func TestGenkitSynthesizerEmptyConversation(t *testing.T) {
	g := genkit.Init(context.Background(), genkit.WithExperimental())
	s := NewGenkitSynthesizer(g)

	conv := dashboard.Conversation{
		RepositoryID: "42",
		Messages:     []dashboard.Message{},
		PendingTurns: []dashboard.PendingTurn{},
	}
	got, err := s.GrillWithDocs(context.Background(), conv)
	if err != nil {
		t.Fatalf("GrillWithDocs empty: %v", err)
	}
	want := dashboard.GrillWithDocs(conv)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GrillWithDocs empty:\n got  %#v\n want %#v", got, want)
	}
	if len(got) != 1 || got[0] != "- No operator decisions have been recorded yet." {
		t.Fatalf("expected default message, got %#v", got)
	}
}

func TestGenkitSynthesizerAssessADREligible(t *testing.T) {
	g := genkit.Init(context.Background(), genkit.WithExperimental())
	s := NewGenkitSynthesizer(g)

	decision := "decision: use SQLite; alternative: use Postgres; trade-off: no extra infra; reversal cost: database migration required"
	assessment, proposal, err := s.AssessADR(context.Background(), decision)
	if err != nil {
		t.Fatalf("AssessADR eligible: %v", err)
	}
	if assessment == "" || proposal == "" {
		t.Fatalf("expected non-empty assessment and proposal")
	}
}

func TestGenkitSynthesizerAssessADRIneligible(t *testing.T) {
	g := genkit.Init(context.Background(), genkit.WithExperimental())
	s := NewGenkitSynthesizer(g)

	decision := "decision: use tabs; alternative: use spaces"
	assessment, proposal, err := s.AssessADR(context.Background(), decision)
	if err != nil {
		t.Fatalf("AssessADR ineligible: %v", err)
	}
	if assessment == "" {
		t.Fatal("expected non-empty assessment")
	}
	if proposal != "" {
		t.Fatalf("expected empty proposal for ineligible decision, got %q", proposal)
	}
}

func TestDecodeToolResultErrorSurfacesAsError(t *testing.T) {
	// A channel cannot be JSON-marshalled; the marshal step must fail.
	ch := make(chan int)
	_, err := decodeToolResult[[]string](ch)
	if err == nil {
		t.Fatal("expected error from marshal failure, got nil")
	}

	// A plain string unmarshals into a scalar, not a struct; the unmarshal step must fail.
	_, err = decodeToolResult[specInput]("not valid for struct")
	if err == nil {
		t.Fatal("expected error from unmarshal failure, got nil")
	}
}
