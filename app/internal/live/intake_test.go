package live

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

func TestGHPublisherCreatesEnglishIssueAndReturnsURL(t *testing.T) {
	publisher := GHPublisher{Command: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "printf", "%s", "https://github.com/mkoziy/hermestrator/issues/73\n")
	}}
	issues, err := publisher.Publish(context.Background(), dashboard.Repository{FullName: "mkoziy/hermestrator"}, []dashboard.Publication{{Title: "feat: intake", Body: "English ticket body."}})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 73 {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestGHPublisherPublishesBlockersBeforeDependentTickets(t *testing.T) {
	var commands [][]string
	publisher := GHPublisher{Command: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		commands = append(commands, args)
		if len(commands) == 1 {
			return exec.CommandContext(ctx, "printf", "%s", "https://github.com/mkoziy/hermestrator/issues/73\n")
		}
		return exec.CommandContext(ctx, "printf", "%s", "https://github.com/mkoziy/hermestrator/issues/74\n")
	}}
	_, err := publisher.Publish(context.Background(), dashboard.Repository{FullName: "mkoziy/hermestrator"}, []dashboard.Publication{{Title: "feat: first", Body: "first"}, {Title: "feat: second", Body: "second", BlockedBy: []int{1}}})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(commands) != 2 || !containsArgumentPair(commands[1], "--body", "second\n\nBlocked by: #73") {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestGHPublisherReusesIssueWithPublicationKey(t *testing.T) {
	var commands [][]string
	publisher := GHPublisher{Command: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		commands = append(commands, args)
		return exec.CommandContext(ctx, "printf", "%s", `[{"number":73,"url":"https://github.com/mkoziy/hermestrator/issues/73"}]`)
	}}
	issues, err := publisher.Publish(context.Background(), dashboard.Repository{FullName: "mkoziy/hermestrator"}, []dashboard.Publication{{Title: "feat: intake", Body: "English ticket body.", Key: "42-1"}})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 73 {
		t.Fatalf("issues = %#v", issues)
	}
	if len(commands) != 1 || len(commands[0]) < 2 || commands[0][0] != "search" || commands[0][1] != "issues" {
		t.Fatalf("commands = %#v", commands)
	}
}

func containsArgumentPair(args []string, key, value string) bool {
	for index := range args {
		if args[index] == key && index+1 < len(args) && args[index+1] == value {
			return true
		}
	}
	return false
}

func TestCloneIntakeCleanupRefusesOutsideItsBaseDirectory(t *testing.T) {
	base := t.TempDir()
	intake := CloneIntake{BaseDir: base}
	inside := filepath.Join(base, "intake-safe")
	if err := os.Mkdir(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := intake.Cleanup(context.Background(), inside); err != nil {
		t.Fatalf("clean isolated intake: %v", err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("isolated intake still exists: %v", err)
	}
	if err := intake.Cleanup(context.Background(), t.TempDir()); err == nil {
		t.Fatal("cleanup outside the intake base unexpectedly succeeded")
	}
}

func TestCloneIntakeUpdatesOnlyContextDocument(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "intake-context")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "main.go"), []byte("package main\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	intake := CloneIntake{BaseDir: base}
	if err := intake.UpdateContext(context.Background(), path, "# Glossary updates\n\n- **Draft** — unpublished."); err != nil {
		t.Fatalf("update context: %v", err)
	}
	contextBody, err := os.ReadFile(filepath.Join(path, "CONTEXT.md"))
	if err != nil || string(contextBody) == "" {
		t.Fatalf("context = %q err=%v", contextBody, err)
	}
	code, err := os.ReadFile(filepath.Join(path, "main.go"))
	if err != nil || string(code) != "package main\n" {
		t.Fatalf("production source changed to %q err=%v", code, err)
	}
}

func TestCloneIntakeRefusesSymlinkedContextDocument(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "intake-context")
	outside := filepath.Join(t.TempDir(), "outside-context")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("unchanged\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(path, "CONTEXT.md")); err != nil {
		t.Fatal(err)
	}

	intake := CloneIntake{BaseDir: base}
	if err := intake.UpdateContext(context.Background(), path, "# Glossary updates"); err == nil {
		t.Fatal("update through a symlink unexpectedly succeeded")
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "unchanged\n" {
		t.Fatalf("outside file = %q err=%v", content, err)
	}
}

func TestCloneIntakeInspectionRefusesSymlinkedEvidence(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "intake-inspect")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "sensitive"), filepath.Join(path, "README.md")); err != nil {
		t.Fatal(err)
	}

	intake := CloneIntake{BaseDir: base}
	if _, err := intake.Inspect(context.Background(), path); err == nil {
		t.Fatal("inspection through a symlink unexpectedly succeeded")
	}
}

func TestCloneIntakePromotionIsRetrySafeAfterTheCloneWasMoved(t *testing.T) {
	base := t.TempDir()
	workspace := t.TempDir()
	path := filepath.Join(base, "intake-promote")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	intake := CloneIntake{BaseDir: base, WorkspaceDir: workspace}
	issue := dashboard.PublishedIssue{Number: 73, URL: "https://github.com/mkoziy/hermestrator/issues/73"}

	promoted, err := intake.Promote(context.Background(), path, issue)
	if err != nil {
		t.Fatalf("initial promotion: %v", err)
	}
	retried, err := intake.Promote(context.Background(), path, issue)
	if err != nil {
		t.Fatalf("retry promotion: %v", err)
	}
	if retried != promoted {
		t.Fatalf("retry promoted path = %q, want %q", retried, promoted)
	}
}
