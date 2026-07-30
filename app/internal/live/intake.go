package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mkoziy/hermestrator/internal/dashboard"
)

var issueURL = regexp.MustCompile(`/issues/(\d+)$`)
var blockerLine = regexp.MustCompile(`(?m)^Blocked by:\s*.+$`)

// GHPublisher is the intentionally narrow GitHub mutation boundary for the
// discovery slice. The application calls it only after both dashboard
// confirmations have been stored.
type GHPublisher struct {
	Command func(context.Context, string, ...string) *exec.Cmd
}

func (p GHPublisher) Publish(ctx context.Context, repo dashboard.Repository, publications []dashboard.Publication) ([]dashboard.PublishedIssue, error) {
	if strings.TrimSpace(repo.FullName) == "" {
		return nil, fmt.Errorf("GitHub repository name is required")
	}
	command := p.Command
	if command == nil {
		command = exec.CommandContext
	}
	issues := make([]dashboard.PublishedIssue, 0, len(publications))
	for _, publication := range publications {
		if strings.TrimSpace(publication.Title) == "" || strings.TrimSpace(publication.Body) == "" {
			return nil, fmt.Errorf("GitHub issue title and body are required")
		}
		body, err := publicationBody(publication, issues)
		if err != nil {
			return nil, err
		}
		output, err := command(ctx, "gh", "issue", "create", "--repo", repo.FullName, "--title", publication.Title, "--body", body).Output()
		if err != nil {
			return issues, fmt.Errorf("create GitHub issue: %w", err)
		}
		url := strings.TrimSpace(string(output))
		match := issueURL.FindStringSubmatch(url)
		if len(match) != 2 {
			return nil, fmt.Errorf("parse created GitHub issue URL %q", url)
		}
		number, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("parse created GitHub issue number: %w", err)
		}
		issues = append(issues, dashboard.PublishedIssue{Number: number, URL: url})
	}
	return issues, nil
}

func publicationBody(publication dashboard.Publication, published []dashboard.PublishedIssue) (string, error) {
	if len(publication.BlockedBy) == 0 {
		return publication.Body, nil
	}
	blockers := make([]string, 0, len(publication.BlockedBy))
	for _, ticket := range publication.BlockedBy {
		if ticket < 1 || ticket > len(published) {
			return "", fmt.Errorf("ticket dependency %d has not been published", ticket)
		}
		blockers = append(blockers, "#"+strconv.Itoa(published[ticket-1].Number))
	}
	body := strings.TrimSpace(blockerLine.ReplaceAllString(publication.Body, ""))
	return body + "\n\nBlocked by: " + strings.Join(blockers, ", "), nil
}

// CloneIntake gives discovery a filesystem view without granting it access to
// a registered repository's working tree. Its only destructive operation is
// limited to a verified child of BaseDir.
type CloneIntake struct {
	BaseDir      string
	WorkspaceDir string
	Command      func(context.Context, string, ...string) *exec.Cmd
}

func (i CloneIntake) Start(ctx context.Context, repo dashboard.Repository) (string, error) {
	if i.BaseDir == "" {
		return "", fmt.Errorf("intake base directory is required")
	}
	if strings.Count(repo.FullName, "/") != 1 || strings.Contains(repo.FullName, "..") {
		return "", fmt.Errorf("invalid GitHub repository name")
	}
	if err := os.MkdirAll(i.BaseDir, 0o750); err != nil {
		return "", fmt.Errorf("create intake base directory: %w", err)
	}
	path, err := os.MkdirTemp(i.BaseDir, "intake-")
	if err != nil {
		return "", fmt.Errorf("create intake directory: %w", err)
	}
	command := i.Command
	if command == nil {
		command = exec.CommandContext
	}
	if output, err := command(ctx, "gh", "repo", "clone", repo.FullName, path, "--", "--depth=1").CombinedOutput(); err != nil {
		if cleanupErr := i.Cleanup(ctx, path); cleanupErr != nil {
			return "", fmt.Errorf("clone intake: %w; clean failed clone: %v", err, cleanupErr)
		}
		return "", fmt.Errorf("clone intake: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return path, nil
}

func (i CloneIntake) Promote(_ context.Context, path string, issue dashboard.PublishedIssue) (string, error) {
	if i.WorkspaceDir == "" {
		return "", fmt.Errorf("issue workspace directory is required")
	}
	if err := i.validateChild(path); err != nil {
		return "", err
	}
	target := filepath.Join(i.WorkspaceDir, "issues", strconv.Itoa(issue.Number))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", fmt.Errorf("create issue workspace parent: %w", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		if err == nil {
			return "", fmt.Errorf("issue workspace %q already exists", target)
		}
		return "", fmt.Errorf("inspect issue workspace: %w", err)
	}
	if err := os.Rename(path, target); err != nil {
		return "", fmt.Errorf("promote intake workspace: %w", err)
	}
	return target, nil
}

func (i CloneIntake) Cleanup(_ context.Context, path string) error {
	if err := i.validateChild(path); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove intake workspace: %w", err)
	}
	return nil
}

// UpdateContext is the one allowed write during discovery. It records settled
// vocabulary in the isolated clone; implementation source files remain out of
// reach until a later, issue-scoped executor phase.
func (i CloneIntake) UpdateContext(_ context.Context, path, glossary string) error {
	if err := i.validateChild(path); err != nil {
		return err
	}
	contextPath, err := i.regularChild(path, "CONTEXT.md")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	current, err := os.ReadFile(contextPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read intake context: %w", err)
	}
	content := strings.TrimRight(string(current), "\n") + "\n\n" + strings.TrimSpace(glossary) + "\n"
	if err := os.WriteFile(contextPath, []byte(content), 0o640); err != nil {
		return fmt.Errorf("write intake context: %w", err)
	}
	return nil
}

// Inspect reads a deliberately small, conventional set of repository files.
// It returns evidence, not instructions, and never runs project code.
func (i CloneIntake) Inspect(_ context.Context, path string) (string, error) {
	if err := i.validateChild(path); err != nil {
		return "", err
	}
	const maxFileBytes = 16 << 10
	var evidence strings.Builder
	for _, name := range []string{"CONTEXT.md", "README.md", "go.mod", "package.json"} {
		file, err := i.regularChild(path, name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		body, err := os.ReadFile(file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", name, err)
		}
		if len(body) > maxFileBytes {
			body = body[:maxFileBytes]
		}
		fmt.Fprintf(&evidence, "\n## %s\n%s\n", name, body)
	}
	return evidence.String(), nil
}

func (i CloneIntake) validateChild(path string) error {
	base, err := filepath.EvalSymlinks(i.BaseDir)
	if err != nil {
		return fmt.Errorf("resolve intake base directory: %w", err)
	}
	candidate, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve intake workspace: %w", err)
	}
	relative, err := filepath.Rel(base, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refuse operation outside isolated intake directory")
	}
	return nil
}

func (i CloneIntake) regularChild(path, name string) (string, error) {
	file := filepath.Join(path, name)
	info, err := os.Lstat(file)
	if err != nil {
		return file, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("refuse non-regular intake file %q", name)
	}
	return file, nil
}
