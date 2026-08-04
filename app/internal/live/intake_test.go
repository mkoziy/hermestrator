package live

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	if err := intake.Cleanup(context.Background(), base); err == nil {
		t.Fatal("cleanup of the intake base unexpectedly succeeded")
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("intake base was removed: %v", err)
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

func TestCloneIntakeRegularDescendantAcceptsNestedRegularFile(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "intake")
	nested := filepath.Join(root, "docs", "architecture.md")
	if err := os.MkdirAll(filepath.Dir(nested), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("architecture"), 0o640); err != nil {
		t.Fatal(err)
	}

	resolved, err := (CloneIntake{BaseDir: base}).regularDescendant(root, filepath.Join("docs", "architecture.md"))
	if err != nil {
		t.Fatalf("regular descendant: %v", err)
	}
	want, err := filepath.EvalSymlinks(nested)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != want {
		t.Fatalf("resolved path = %q, want %q", resolved, want)
	}
}

func TestCloneIntakeRegularDescendantRefusesTraversalAndSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "intake")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "leaf.md")); err != nil {
		t.Fatal(err)
	}

	intake := CloneIntake{BaseDir: base}
	for name, relative := range map[string]string{
		"traversal":            filepath.Join("..", "secret.md"),
		"intermediate symlink": filepath.Join("linked", "secret.md"),
		"leaf symlink":         "leaf.md",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := intake.regularDescendant(root, relative); err == nil {
				t.Fatalf("regular descendant %q unexpectedly succeeded", relative)
			}
		})
	}
}

func TestCloneIntakeGlobMatchesNestedBaseNames(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(root, "docs", "adr"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"README.md", filepath.Join("docs", "adr", "0001.md"), filepath.Join("docs", "adr", "notes.txt")} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "hidden.md"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	intake := CloneIntake{BaseDir: base}
	got, err := intake.Glob(context.Background(), root, "*.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != "README.md\ndocs/adr/0001.md" {
		t.Fatalf("glob = %q", got)
	}
	if got, err := intake.Glob(context.Background(), root, "docs/*.md"); err != nil || got != "no matches" {
		t.Fatalf("base-name limitation = %q, %v", got, err)
	}
}

func TestCloneIntakeGlobCapsMatches(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxDiscoveryGlobMatches+1; index++ {
		if err := os.WriteFile(filepath.Join(root, "match-"+strconv.Itoa(index)+".md"), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	got, err := (CloneIntake{BaseDir: base}).Glob(context.Background(), root, "*.md")
	if err != nil {
		t.Fatal(err)
	}
	if matches := strings.Split(got, "\n"); len(matches) != maxDiscoveryGlobMatches {
		t.Fatalf("glob matches = %d, want %d", len(matches), maxDiscoveryGlobMatches)
	}
}

func TestCloneIntakeGrepMatchesAndCapsOutput(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "notes.md"), []byte("keep this\nignore\nkeep that\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "secret"), []byte("keep hidden\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	intake := CloneIntake{BaseDir: base}
	got, err := intake.Grep(context.Background(), root, `keep`)
	if err != nil || got != "docs/notes.md:1: keep this\ndocs/notes.md:3: keep that\n" {
		t.Fatalf("grep = %q, %v", got, err)
	}
	if got, err := intake.Grep(context.Background(), root, `missing`); err != nil || got != "no matches" {
		t.Fatalf("no matches = %q, %v", got, err)
	}
	if _, err := intake.Grep(context.Background(), root, "["); err == nil {
		t.Fatal("invalid regex accepted")
	}
	large := strings.Repeat("needle\n", 5000)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(large), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err = intake.Grep(context.Background(), root, `needle`)
	if err != nil || len(got) > maxDiscoveryGrepBytes {
		t.Fatalf("capped grep length=%d err=%v", len(got), err)
	}
}

func TestCloneIntakeGrepSkipsFilesWithOversizedLines(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("x", 1<<20+1)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "match.txt"), []byte("needle\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	got, err := (CloneIntake{BaseDir: base}).Grep(context.Background(), root, `needle`)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if got != "match.txt:1: needle\n" {
		t.Fatalf("grep = %q", got)
	}
}

func TestCloneIntakeGrepCapsInputScannedPerCall(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("miss\n", maxDiscoveryGrepInputBytes/5+1)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "later.txt"), []byte("needle\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	got, err := (CloneIntake{BaseDir: base}).Grep(context.Background(), root, `needle`)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if got != "no matches" {
		t.Fatalf("grep after input cap = %q", got)
	}
}

func TestCloneIntakeReadReturnsFileContents(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "intake-read")
	if err := os.MkdirAll(filepath.Join(path, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "docs", "README.md"), []byte("repository guide\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	intake := CloneIntake{BaseDir: base}
	got, err := intake.Read(context.Background(), path, filepath.Join("docs", "README.md"))
	if err != nil || got != "repository guide\n" {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestCloneIntakeReadCapsSizeAndRefusesUnsafeFiles(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "intake-read")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "large.txt"), []byte(strings.Repeat("x", maxDiscoveryReadBytes+100)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(path, "directory"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(path, "linked.txt")); err != nil {
		t.Fatal(err)
	}

	intake := CloneIntake{BaseDir: base}
	got, err := intake.Read(context.Background(), path, "large.txt")
	if err != nil || len(got) != maxDiscoveryReadBytes {
		t.Fatalf("capped read length=%d err=%v", len(got), err)
	}
	for name, relative := range map[string]string{
		"directory": "directory",
		"symlink":   "linked.txt",
		"traversal": filepath.Join("..", "secret.txt"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := intake.Read(context.Background(), path, relative); err == nil {
				t.Fatalf("read %q unexpectedly succeeded", relative)
			}
		})
	}
}

func TestCloneIntakeReadHonorsCancelledContext(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "intake-read")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("repository guide"), 0o640); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (CloneIntake{BaseDir: base}).Read(ctx, path, "README.md"); !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want context cancellation", err)
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
