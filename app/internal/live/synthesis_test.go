package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
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

// ---------------------------------------------------------------------------
// HTTP-seam integration test: full synthesizeIntake path through
// GenkitSynthesizer (Task 5). This is the only test that exercises the
// production RunRaw/decode path end to end via the HTTP handler.
// ---------------------------------------------------------------------------

type fakeGitHub struct{ repos []dashboard.Repository }

func (f fakeGitHub) Repositories(context.Context) ([]dashboard.Repository, error) {
	return f.repos, nil
}

type fakeModel struct{}

func (fakeModel) Reply(_ context.Context, _ dashboard.Conversation, prompt string) (dashboard.Reply, error) {
	return dashboard.Reply{Text: "What outcome would make " + prompt + " successful?"}, nil
}

func (fakeModel) Status(context.Context, string) (dashboard.Status, error) {
	return dashboard.Status{Phase: "discovery", ModelRole: "discovery"}, nil
}

type fakeIntake struct{ started []string }

func (f *fakeIntake) Start(_ context.Context, _ dashboard.Repository) (string, error) {
	f.started = append(f.started, "/tmp/intake-42")
	return "/tmp/intake-42", nil
}

func (f *fakeIntake) Promote(_ context.Context, _ string, _ dashboard.PublishedIssue) (string, error) {
	return "", nil
}

func (f *fakeIntake) Cleanup(_ context.Context, _ string) error { return nil }

func TestGenkitSynthesizerEndToEndViaHTTPSynthesize(t *testing.T) {
	g := genkit.Init(context.Background(), genkit.WithExperimental())
	synth := NewGenkitSynthesizer(g)

	app, err := dashboard.New(dashboard.Dependencies{
		GitHub:       fakeGitHub{repos: []dashboard.Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        fakeModel{},
		Intake:       &fakeIntake{},
		Synthesizer:  synth,
		Store:        t.TempDir() + "/pm.db",
		AllowedUsers: map[string]bool{"michael": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()

	req := func(method, target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("X-PM-User", "michael")
		if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}

	// Set up: list repos and select one.
	_ = req(http.MethodGet, "/repositories", "")
	_ = req(http.MethodPost, "/repositories/42", "")

	// Start an intake.
	resp := req(http.MethodPost, "/repositories/42/intake/start", "")
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("start intake: %d", resp.Code)
	}

	// Complete discovery.
	resp = req(http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"operators can create projects"}}.Encode())
	if resp.Code != http.StatusSeeOther && resp.Code != http.StatusOK {
		t.Fatalf("converse: %d %q", resp.Code, resp.Body.String())
	}
	resp = req(http.MethodPost, "/repositories/42/intake/complete-discovery", "")
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("complete discovery: %d", resp.Code)
	}

	// Synthesize — this is the call that exercises GenkitSynthesizer.RunRaw.
	resp = req(http.MethodPost, "/repositories/42/intake/synthesize", "")
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("synthesize through GenkitSynthesizer: %d %q", resp.Code, resp.Body.String())
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
