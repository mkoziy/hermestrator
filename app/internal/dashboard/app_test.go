package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

type fakeGitHub struct{ repos []Repository }

func (f fakeGitHub) Repositories(context.Context) ([]Repository, error) { return f.repos, nil }

type fakeModel struct{}

func (fakeModel) Reply(_ context.Context, _ Conversation, prompt string) (Reply, error) {
	return Reply{Text: "What outcome would make " + prompt + " successful?", Tokens: 21, CostUSD: 0.0042}, nil
}

func (fakeModel) Status(context.Context, string) (Status, error) {
	return Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "12s", RecentActivity: "awaiting operator"}, nil
}

type streamingFakeModel struct{ fakeModel }

func (streamingFakeModel) Stream(_ context.Context, _ Conversation, _ string, emit func(string) error) (Reply, error) {
	if err := emit("A focused "); err != nil {
		return Reply{}, err
	}
	if err := emit("next step"); err != nil {
		return Reply{}, err
	}
	return Reply{Text: "A focused next step", Tokens: 12, CostUSD: 0.0012}, nil
}

type noSnapshotModel struct{ fakeModel }

func (noSnapshotModel) Status(context.Context, string) (Status, error) {
	return Status{Phase: "discovery", ModelRole: "discovery", Elapsed: "0s", RecentActivity: "awaiting discovery"}, nil
}

type splitSecretStreamingModel struct{ fakeModel }

func (splitSecretStreamingModel) Stream(_ context.Context, _ Conversation, _ string, emit func(string) error) (Reply, error) {
	for _, chunk := range []string{"The token is ", "123456789", ":AAabcdefghijklmnopqrstuvwxyz123456789", " done"} {
		if err := emit(chunk); err != nil {
			return Reply{}, err
		}
	}
	return Reply{Text: "The token is 123456789:AAabcdefghijklmnopqrstuvwxyz123456789 done"}, nil
}

type blockingStreamingModel struct {
	fakeModel
	started sync.Once
	ready   chan struct{}
	release chan struct{}
}

func (m *blockingStreamingModel) Stream(_ context.Context, _ Conversation, _ string, emit func(string) error) (Reply, error) {
	m.started.Do(func() { close(m.ready) })
	<-m.release
	if err := emit("resumed reply"); err != nil {
		return Reply{}, err
	}
	return Reply{Text: "resumed reply"}, nil
}

type fakeTelegram struct{ notifications []Notification }

func (f *fakeTelegram) Notify(_ context.Context, notification Notification) error {
	f.notifications = append(f.notifications, notification)
	return nil
}

type failingTelegram struct{ err error }

func (f failingTelegram) Notify(context.Context, Notification) error { return f.err }

type fakePublisher struct {
	publications []Publication
	issues       []PublishedIssue
}

func (f *fakePublisher) Publish(_ context.Context, _ Repository, publications []Publication) ([]PublishedIssue, error) {
	f.publications = append(f.publications, publications...)
	return f.issues, nil
}

type fakeIntake struct {
	started, promoted, cleaned []string
	promoteErr                 error
}

func (f *fakeIntake) Start(_ context.Context, _ Repository) (string, error) {
	f.started = append(f.started, "/tmp/intake-42")
	return "/tmp/intake-42", nil
}

func (f *fakeIntake) Promote(_ context.Context, path string, issue PublishedIssue) (string, error) {
	f.promoted = append(f.promoted, path)
	if f.promoteErr != nil {
		return "", f.promoteErr
	}
	return "/tmp/issues/" + strconv.Itoa(issue.Number), nil
}

func (f *fakeIntake) Cleanup(_ context.Context, path string) error {
	f.cleaned = append(f.cleaned, path)
	return nil
}

func TestDashboardRootRedirectsAuthorizedOperatorToRepositoryPicker(t *testing.T) {
	app := mustApp(t, Dependencies{
		GitHub:       fakeGitHub{},
		Model:        fakeModel{},
		Store:        t.TempDir() + "/pm.db",
		AllowedUsers: map[string]bool{"michael": true},
	})

	response := request(t, app, http.MethodGet, "/", "", "michael")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("root status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	if got := response.Header().Get("Location"); got != "/repositories" {
		t.Fatalf("root location = %q, want %q", got, "/repositories")
	}
}

func TestRepositoryPickerUsesBrowserNavigationForSelection(t *testing.T) {
	app := mustApp(t, Dependencies{
		GitHub:       fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        fakeModel{},
		Store:        t.TempDir() + "/pm.db",
		AllowedUsers: map[string]bool{"michael": true},
	})

	picker := request(t, app, http.MethodGet, "/repositories", "", "michael")
	if strings.Contains(picker.Body.String(), `hx-post="/repositories/42"`) {
		t.Fatalf("repository picker intercepts browser navigation: %q", picker.Body.String())
	}
	if !strings.Contains(picker.Body.String(), `action="/repositories/42"`) {
		t.Fatalf("repository picker form action missing: %q", picker.Body.String())
	}
}

func TestOperatorCanOpenFreshRepositoryWorkspace(t *testing.T) {
	app := mustApp(t, Dependencies{
		GitHub:       fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        noSnapshotModel{},
		Store:        t.TempDir() + "/pm.db",
		AllowedUsers: map[string]bool{"michael": true},
	})

	request(t, app, http.MethodGet, "/repositories", "", "michael")
	selected := request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	if selected.Code != http.StatusSeeOther {
		t.Fatalf("select status = %d, want %d", selected.Code, http.StatusSeeOther)
	}

	workspace := request(t, app, http.MethodGet, "/repositories/42", "", "michael")
	if workspace.Code != http.StatusOK {
		t.Fatalf("workspace status = %d, want %d", workspace.Code, http.StatusOK)
	}
	for _, want := range []string{"discovery", "awaiting discovery", "0s"} {
		if !strings.Contains(workspace.Body.String(), want) {
			t.Fatalf("workspace missing %q: %q", want, workspace.Body.String())
		}
	}
}

func TestOperatorCanSelectRepositoryAndContinueConversationAfterRestart(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	telegram := &fakeTelegram{}
	deps := Dependencies{
		GitHub:       fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        fakeModel{},
		Telegram:     telegram,
		Store:        database,
		AllowedUsers: map[string]bool{"michael": true},
	}
	app := mustApp(t, deps)

	repos := request(t, app, http.MethodGet, "/repositories", "", "michael")
	if !strings.Contains(repos.Body.String(), "mkoziy/hermestrator") {
		t.Fatalf("repository picker = %q", repos.Body.String())
	}

	selectRepo := request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	if selectRepo.Code != http.StatusSeeOther {
		t.Fatalf("select status = %d", selectRepo.Code)
	}

	turn := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"a dashboard"}}.Encode(), "michael")
	if turn.Code != http.StatusOK || !strings.Contains(turn.Body.String(), "What outcome would make a dashboard successful?") {
		t.Fatalf("turn = %d %q", turn.Code, turn.Body.String())
	}
	if !strings.Contains(turn.Body.String(), "discovery") || !strings.Contains(turn.Body.String(), "0.0042") {
		t.Fatalf("telemetry absent: %q", turn.Body.String())
	}
	if len(telegram.notifications) != 0 {
		t.Fatalf("ordinary turn notified Telegram: %#v", telegram.notifications)
	}

	restarted := mustApp(t, deps)
	page := request(t, restarted, http.MethodGet, "/repositories/42", "", "michael")
	if !strings.Contains(page.Body.String(), "a dashboard") || !strings.Contains(page.Body.String(), "What outcome") {
		t.Fatalf("durable conversation missing: %q", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "Tokens: 21") || !strings.Contains(page.Body.String(), "Cost: 0.0042") {
		t.Fatalf("durable telemetry missing: %q", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "Elapsed: 12s") || !strings.Contains(page.Body.String(), "awaiting operator") {
		t.Fatalf("durable agent status missing: %q", page.Body.String())
	}
}

func TestIntakeRequiresConfirmationBeforePublishingAndSurvivesRestart(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	publisher := &fakePublisher{issues: []PublishedIssue{{Number: 73, URL: "https://github.com/mkoziy/hermestrator/issues/73"}}}
	intake := &fakeIntake{}
	deps := Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Publisher: publisher, Intake: intake, Store: database, AllowedUsers: map[string]bool{"michael": true}}
	app := mustApp(t, deps)
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42", "", "michael")

	if response := request(t, app, http.MethodPost, "/repositories/42/intake/start", "", "michael"); response.Code != http.StatusSeeOther {
		t.Fatalf("start intake status = %d", response.Code)
	}
	_ = request(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"operators can create projects"}}.Encode(), "michael")
	if response := request(t, app, http.MethodPost, "/repositories/42/intake/synthesize", "", "michael"); response.Code != http.StatusSeeOther {
		t.Fatalf("synthesize status = %d", response.Code)
	}
	if response := request(t, app, http.MethodPost, "/repositories/42/intake/publish", "", "michael"); response.Code != http.StatusConflict {
		t.Fatalf("unconfirmed publish status = %d", response.Code)
	}
	for _, artifact := range []string{"spec", "tickets"} {
		if response := request(t, app, http.MethodPost, "/repositories/42/intake/"+artifact+"/confirm", "", "michael"); response.Code != http.StatusSeeOther {
			t.Fatalf("confirm %s status = %d", artifact, response.Code)
		}
	}

	restarted := mustApp(t, deps)
	workspace := request(t, restarted, http.MethodGet, "/repositories/42", "", "michael")
	for _, want := range []string{"Tracked-work intake", "confirmed", "Glossary updates", "operators can create projects"} {
		if !strings.Contains(workspace.Body.String(), want) {
			t.Fatalf("workspace missing %q: %q", want, workspace.Body.String())
		}
	}
	if response := request(t, restarted, http.MethodPost, "/repositories/42/intake/publish", "", "michael"); response.Code != http.StatusSeeOther {
		t.Fatalf("publish status = %d", response.Code)
	}
	if len(publisher.publications) != 1 || !strings.HasPrefix(publisher.publications[0].Title, "feat: ") {
		t.Fatalf("published issues = %#v", publisher.publications)
	}
	if len(intake.promoted) != 1 || intake.promoted[0] != "/tmp/intake-42" {
		t.Fatalf("intake promotion = %#v", intake.promoted)
	}
}

func TestAbandonIntakeCleansOnlyUnpublishedDraft(t *testing.T) {
	intake := &fakeIntake{}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Intake: intake, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/start", "", "michael")
	response := request(t, app, http.MethodPost, "/repositories/42/intake/abandon", "", "michael")
	if response.Code != http.StatusSeeOther || len(intake.cleaned) != 1 || intake.cleaned[0] != "/tmp/intake-42" {
		t.Fatalf("abandon = %d cleaned=%#v", response.Code, intake.cleaned)
	}
}

func TestAbandonedIntakeCannotConfirmOrPublishStaleArtifacts(t *testing.T) {
	publisher := &fakePublisher{issues: []PublishedIssue{{Number: 73, URL: "https://github.com/mkoziy/hermestrator/issues/73"}}}
	intake := &fakeIntake{}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Publisher: publisher, Intake: intake, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/start", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"operators can create projects"}}.Encode(), "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/synthesize", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/abandon", "", "michael")

	for _, path := range []string{"/repositories/42/intake/spec/confirm", "/repositories/42/intake/tickets/confirm", "/repositories/42/intake/publish"} {
		if response := request(t, app, http.MethodPost, path, "", "michael"); response.Code != http.StatusConflict {
			t.Fatalf("%s status = %d", path, response.Code)
		}
	}
	if len(publisher.publications) != 0 {
		t.Fatalf("stale artifacts were published: %#v", publisher.publications)
	}
}

func TestIntakeRequiresACompletedDiscoveryExchangeBeforeSynthesis(t *testing.T) {
	intake := &fakeIntake{}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Intake: intake, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/start", "", "michael")

	if response := request(t, app, http.MethodPost, "/repositories/42/intake/synthesize", "", "michael"); response.Code != http.StatusConflict {
		t.Fatalf("synthesize without discovery status = %d", response.Code)
	}
}

func TestIntakeDoesNotCreateAnotherCloneWhileOneIsActive(t *testing.T) {
	intake := &fakeIntake{}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Intake: intake, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/start", "", "michael")

	response := request(t, app, http.MethodPost, "/repositories/42/intake/start", "", "michael")
	if response.Code != http.StatusConflict || len(intake.started) != 1 {
		t.Fatalf("second start = %d started=%#v", response.Code, intake.started)
	}
}

func TestPublishedIntakeRetriesPromotionWithoutPublishingAnotherIssue(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	publisher := &fakePublisher{issues: []PublishedIssue{{Number: 73, URL: "https://github.com/mkoziy/hermestrator/issues/73"}}}
	intake := &fakeIntake{promoteErr: errors.New("workspace unavailable")}
	deps := Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Publisher: publisher, Intake: intake, Store: database, AllowedUsers: map[string]bool{"michael": true}}
	app := mustApp(t, deps)
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/start", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"operators can create projects"}}.Encode(), "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/synthesize", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/spec/confirm", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/tickets/confirm", "", "michael")

	if response := request(t, app, http.MethodPost, "/repositories/42/intake/publish", "", "michael"); response.Code != http.StatusInternalServerError {
		t.Fatalf("failed promotion = %d", response.Code)
	}
	intake.promoteErr = nil
	if response := request(t, app, http.MethodPost, "/repositories/42/intake/publish", "", "michael"); response.Code != http.StatusSeeOther {
		t.Fatalf("retried promotion = %d", response.Code)
	}
	if len(publisher.publications) != 1 {
		t.Fatalf("published %d times", len(publisher.publications))
	}
}

func TestDraftIntakeRendersADRProposalForm(t *testing.T) {
	intake := &fakeIntake{}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Intake: intake, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42", "", "michael")
	_ = request(t, app, http.MethodPost, "/repositories/42/intake/start", "", "michael")

	workspace := request(t, app, http.MethodGet, "/repositories/42", "", "michael")
	if !strings.Contains(workspace.Body.String(), "Propose ADR") {
		t.Fatalf("ADR form missing: %q", workspace.Body.String())
	}
}

func TestTicketSetPublicationsPreserveBlockingEdges(t *testing.T) {
	publications, err := ticketSetPublications("# Ticket set\n\n## Ticket 1: establish the seam\n\nBlocked by: none\n\n## Ticket 2: finish the workflow\n\nBlocked by: Ticket 1")
	if err != nil {
		t.Fatalf("parse ticket set: %v", err)
	}
	if len(publications) != 2 || publications[0].Title != "feat: establish the seam" || len(publications[1].BlockedBy) != 1 || publications[1].BlockedBy[0] != 1 {
		t.Fatalf("publications = %#v", publications)
	}
}

func TestNonHTMXConversationPostRedirectsToWorkspace(t *testing.T) {
	app := mustApp(t, Dependencies{
		GitHub:       fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        fakeModel{},
		Store:        t.TempDir() + "/pm.db",
		AllowedUsers: map[string]bool{"michael": true},
	})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")

	response := request(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"a dashboard"}}.Encode(), "michael")
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/repositories/42" {
		t.Fatalf("response = %d location=%q", response.Code, response.Header().Get("Location"))
	}
}

func TestProtectedRoutesRejectUnapprovedOperator(t *testing.T) {
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	response := request(t, app, http.MethodGet, "/repositories", "", "stranger")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestProtectedRoutesMatchGitHubLoginsCaseInsensitively(t *testing.T) {
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	response := request(t, app, http.MethodGet, "/repositories", "", "Michael")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestRepositoryRegistrationRejectsUnknownRepository(t *testing.T) {
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	response := request(t, app, http.MethodPost, "/repositories/not-listed", "", "michael")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestConversationRedactsSecretsBeforePersistenceAndRendering(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: fakeModel{}, Store: database, AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	token := "123456789:AAabcdefghijklmnopqrstuvwxyz123456789"
	response := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {token}}.Encode(), "michael")
	if strings.Contains(response.Body.String(), token) || !strings.Contains(response.Body.String(), "[redacted]") {
		t.Fatalf("secret rendered in %q", response.Body.String())
	}
}

func TestTestNotificationIsReadOnlyAndLinksToDashboard(t *testing.T) {
	telegram := &fakeTelegram{}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Telegram: telegram, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}, DashboardURL: "https://pm.example"})
	response := request(t, app, http.MethodPost, "/notifications/test", "", "michael")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if len(telegram.notifications) != 1 || telegram.notifications[0] != (Notification{Text: "PM dashboard test notification.", URL: "https://pm.example/repositories"}) {
		t.Fatalf("notifications = %#v", telegram.notifications)
	}
}

func TestWorkspaceUsesTablerComponentsForConversationAndTestNotification(t *testing.T) {
	app := mustApp(t, Dependencies{
		GitHub:       fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        streamingFakeModel{},
		Store:        t.TempDir() + "/pm.db",
		AllowedUsers: map[string]bool{"michael": true},
	})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")

	workspace := request(t, app, http.MethodGet, "/repositories/42", "", "michael")
	for _, want := range []string{
		`class="card mb-3"`,
		`class="btn btn-primary"`,
		`action="/notifications/test"`,
		`Test Telegram notification`,
	} {
		if !strings.Contains(workspace.Body.String(), want) {
			t.Fatalf("workspace missing %q: %q", want, workspace.Body.String())
		}
	}

	started := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"style it"}}.Encode(), "michael")
	if !strings.Contains(started.Body.String(), `class="card card-body mb-2"`) {
		t.Fatalf("stream start is not a Tabler component: %q", started.Body.String())
	}
	stream := request(t, app, http.MethodGet, streamURL(t, started.Body.String()), "", "michael")
	if !strings.Contains(stream.Body.String(), `event: chunk`) || !strings.Contains(stream.Body.String(), `<strong>pm:</strong> A focused next step`) || !strings.Contains(stream.Body.String(), `class="card card-body mb-2"`) {
		t.Fatalf("streamed response is not a Tabler component: %q", stream.Body.String())
	}
}

func TestTestNotificationKeepsTelegramFailureOutOfBrowser(t *testing.T) {
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: fakeModel{}, Telegram: failingTelegram{err: errors.New("bot token secret-value rejected")}, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}, DashboardURL: "https://pm.example"})
	response := request(t, app, http.MethodPost, "/notifications/test", "", "michael")
	if response.Code != http.StatusBadGateway || response.Body.String() != "Telegram unavailable\n" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}

func TestConversationStreamsHTMXFragmentsAndPersistsTheCompletedTurn(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	deps := Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: streamingFakeModel{}, Store: database, AllowedUsers: map[string]bool{"michael": true}}
	app := mustApp(t, deps)
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")

	response := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"ship it"}}.Encode(), "michael")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "sse-connect") {
		t.Fatalf("SSE bridge missing: %q", response.Body.String())
	}
	streamURL := streamURL(t, response.Body.String())
	stream := request(t, app, http.MethodGet, streamURL, "", "michael")
	if stream.Code != http.StatusOK || stream.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(stream.Body.String(), "event: chunk") || !strings.Contains(stream.Body.String(), "A focused next step") || !strings.Contains(stream.Body.String(), "event: done") {
		t.Fatalf("SSE events missing: %d %q", stream.Code, stream.Body.String())
	}
	replay := request(t, app, http.MethodGet, streamURL, "", "michael")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), "event: done") {
		t.Fatalf("SSE replay missing: %d %q", replay.Code, replay.Body.String())
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var replies int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE repository_id='42' AND role='pm'`).Scan(&replies); err != nil {
		t.Fatal(err)
	}
	if replies != 1 {
		t.Fatalf("replayed SSE generated %d replies", replies)
	}

	restarted := mustApp(t, deps)
	page := request(t, restarted, http.MethodGet, "/repositories/42", "", "michael")
	if !strings.Contains(page.Body.String(), "A focused next step") {
		t.Fatalf("completed stream was not durable: %q", page.Body.String())
	}
}

func TestConversationNeverPersistsSplitStreamedSecrets(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: splitSecretStreamingModel{}, Store: database, AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")

	response := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"stream safely"}}.Encode(), "michael")
	stream := request(t, app, http.MethodGet, streamURL(t, response.Body.String()), "", "michael")
	token := "123456789:AAabcdefghijklmnopqrstuvwxyz123456789"
	if strings.Contains(stream.Body.String(), token) || !strings.Contains(stream.Body.String(), "[redacted]") {
		t.Fatalf("stream leaked secret: %q", stream.Body.String())
	}

	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var events string
	if err := db.QueryRow(`SELECT group_concat(data, '') FROM turn_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(events, token) {
		t.Fatalf("persisted stream leaked secret: %q", events)
	}
}

func TestWorkspaceReconnectsToPendingTurnAfterReload(t *testing.T) {
	model := &blockingStreamingModel{ready: make(chan struct{}), release: make(chan struct{})}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: model, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")
	start := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"resume me"}}.Encode(), "michael")
	stream := streamURL(t, start.Body.String())
	<-model.ready

	page := request(t, app, http.MethodGet, "/repositories/42", "", "michael")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `sse-connect="`+stream+`"`) || !strings.Contains(page.Body.String(), `id="pm-send" class="btn btn-primary" disabled`) {
		t.Fatalf("pending turn was not reconnected: %d %q", page.Code, page.Body.String())
	}
	close(model.release)
	completed := request(t, app, http.MethodGet, stream, "", "michael")
	if !strings.Contains(completed.Body.String(), "event: done") {
		t.Fatalf("resumed turn did not complete: %q", completed.Body.String())
	}
}

func TestConversationAllowsOnlyOneStreamingTurnAtATime(t *testing.T) {
	model := &blockingStreamingModel{ready: make(chan struct{}), release: make(chan struct{})}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}}, Model: model, Store: t.TempDir() + "/pm.db", AllowedUsers: map[string]bool{"michael": true}})
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")

	started := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"first"}}.Encode(), "michael")
	if started.Code != http.StatusOK || !strings.Contains(started.Body.String(), `id="pm-send" class="btn btn-primary" disabled`) {
		t.Fatalf("stream start should disable sending: %d %q", started.Code, started.Body.String())
	}
	<-model.ready

	second := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"second"}}.Encode(), "michael")
	if second.Code != http.StatusConflict || second.Body.String() != "A PM response is already in progress.\n" {
		t.Fatalf("second streaming turn = %d %q", second.Code, second.Body.String())
	}

	close(model.release)
	stream := request(t, app, http.MethodGet, streamURL(t, started.Body.String()), "", "michael")
	if !strings.Contains(stream.Body.String(), `id="pending-turn-`) || !strings.Contains(stream.Body.String(), `hx-swap-oob="outerHTML"`) || !strings.Contains(stream.Body.String(), `id="pm-send" class="btn btn-primary" hx-swap-oob="outerHTML"`) {
		t.Fatalf("stream completion should replace its pending card and restore sending: %q", stream.Body.String())
	}
}

func TestStartupCancelsLegacyDuplicatePendingTurns(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	legacy, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE repositories (id TEXT PRIMARY KEY, full_name TEXT NOT NULL);
		CREATE TABLE messages (repository_id TEXT NOT NULL, role TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);
		CREATE TABLE pending_turns (id TEXT PRIMARY KEY, repository_id TEXT NOT NULL, prompt TEXT NOT NULL, started_at TEXT, completed_at TEXT);
		INSERT INTO repositories(id,full_name) VALUES('42','mkoziy/hermestrator');
		INSERT INTO pending_turns(id,repository_id,prompt,started_at) VALUES
			('oldest','42','first','2026-07-29T12:00:00Z'),
			('middle','42','second','2026-07-29T12:01:00Z'),
			('newest','42','third','2026-07-29T12:02:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	model := &blockingStreamingModel{ready: make(chan struct{}), release: make(chan struct{})}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: model, Store: database, AllowedUsers: map[string]bool{"michael": true}})
	defer func() { close(model.release) }()
	<-model.ready

	page := request(t, app, http.MethodGet, "/repositories/42", "", "michael")
	if page.Code != http.StatusOK || strings.Count(page.Body.String(), "sse-connect=") != 1 || !strings.Contains(page.Body.String(), `id="pm-send" class="btn btn-primary" disabled`) {
		t.Fatalf("workspace did not retain one active turn: %d %q", page.Code, page.Body.String())
	}

	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var active, canceled int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_turns WHERE state IN ('pending','running')`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_turns WHERE state='canceled' AND terminal_reason='canceled after duplicate submission recovery'`).Scan(&canceled); err != nil {
		t.Fatal(err)
	}
	if active != 1 || canceled != 2 {
		t.Fatalf("pending turn recovery = %d active, %d canceled; want 1 active, 2 canceled", active, canceled)
	}
}

func TestStartupResumesInterruptedRunningTurn(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE repositories (id TEXT PRIMARY KEY, full_name TEXT NOT NULL);
		CREATE TABLE messages (repository_id TEXT NOT NULL, role TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);
		CREATE TABLE pending_turns (id TEXT PRIMARY KEY, repository_id TEXT NOT NULL, prompt TEXT NOT NULL, started_at TEXT, completed_at TEXT, state TEXT NOT NULL DEFAULT 'pending', terminal_reason TEXT);
		INSERT INTO repositories(id,full_name) VALUES('42','mkoziy/hermestrator');
		INSERT INTO pending_turns(id,repository_id,prompt,started_at,state) VALUES('interrupted','42','first','2026-07-29T12:00:00Z','running');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	model := &blockingStreamingModel{ready: make(chan struct{}), release: make(chan struct{})}
	app := mustApp(t, Dependencies{GitHub: fakeGitHub{}, Model: model, Store: database, AllowedUsers: map[string]bool{"michael": true}})
	<-model.ready
	page := request(t, app, http.MethodGet, "/repositories/42", "", "michael")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "sse-connect=") || !strings.Contains(page.Body.String(), `id="pm-send" class="btn btn-primary" disabled`) {
		t.Fatalf("interrupted turn was not resumed: %d %q", page.Code, page.Body.String())
	}
	close(model.release)
	stream := request(t, app, http.MethodGet, streamURL(t, page.Body.String()), "", "michael")
	if !strings.Contains(stream.Body.String(), "resumed reply") {
		t.Fatalf("resumed turn did not complete: %q", stream.Body.String())
	}

	db, err = sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var state, reason string
	if err := db.QueryRow(`SELECT state,terminal_reason FROM pending_turns WHERE id='interrupted'`).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != string(turnCompleted) || reason != "" {
		t.Fatalf("interrupted turn = (%q,%q)", state, reason)
	}
}

func streamURL(t *testing.T, body string) string {
	t.Helper()
	const prefix = `sse-connect="`
	start := strings.Index(body, prefix)
	if start == -1 {
		t.Fatalf("no stream URL in %q", body)
	}
	value := body[start+len(prefix):]
	end := strings.Index(value, `"`)
	if end == -1 {
		t.Fatalf("unterminated stream URL in %q", body)
	}
	return value[:end]
}

func TestExistingConversationDatabaseMigratesTelemetryColumns(t *testing.T) {
	database := t.TempDir() + "/pm.db"
	legacy, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE messages (repository_id TEXT NOT NULL, role TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	deps := Dependencies{
		GitHub:       fakeGitHub{repos: []Repository{{ID: "42", FullName: "mkoziy/hermestrator"}}},
		Model:        fakeModel{},
		Store:        database,
		AllowedUsers: map[string]bool{"michael": true},
	}
	app := mustApp(t, deps)
	_ = request(t, app, http.MethodGet, "/repositories", "", "michael")

	response := requestHTMX(t, app, http.MethodPost, "/repositories/42/conversation", url.Values{"message": {"migrate"}}.Encode(), "michael")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Tokens: 21") {
		t.Fatalf("migrated conversation = %d %q", response.Code, response.Body.String())
	}
}

func mustApp(t *testing.T, deps Dependencies) http.Handler {
	t.Helper()
	app, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	return app
}
func request(t *testing.T, app http.Handler, method, target, body, user string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("X-PM-User", user)
	if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func requestHTMX(t *testing.T, app http.Handler, method, target, body, user string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("X-PM-User", user)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}
