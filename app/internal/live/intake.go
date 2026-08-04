package live

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		if publication.Key != "" {
			existing, found, findErr := p.findPublished(ctx, repo, publication.Key)
			if findErr != nil {
				return issues, findErr
			}
			if found {
				issues = append(issues, existing)
				continue
			}
			body += "\n\n<!-- hermestrator-publication:" + publication.Key + " -->"
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

func (p GHPublisher) findPublished(ctx context.Context, repo dashboard.Repository, key string) (dashboard.PublishedIssue, bool, error) {
	command := p.Command
	if command == nil {
		command = exec.CommandContext
	}
	output, err := command(ctx, "gh", "search", "issues", "--repo", repo.FullName, "--match", "body", "--json", "number,url", "--limit", "2", "hermestrator-publication:"+key).Output()
	if err != nil {
		return dashboard.PublishedIssue{}, false, fmt.Errorf("find GitHub issue by publication key: %w", err)
	}
	var issues []struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(output, &issues); err != nil {
		return dashboard.PublishedIssue{}, false, fmt.Errorf("decode GitHub publication search: %w", err)
	}
	if len(issues) == 0 {
		return dashboard.PublishedIssue{}, false, nil
	}
	if len(issues) > 1 {
		return dashboard.PublishedIssue{}, false, fmt.Errorf("publication key %q matches multiple GitHub issues", key)
	}
	return dashboard.PublishedIssue{Number: issues[0].Number, URL: issues[0].URL}, true, nil
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
	if issue.Number < 1 {
		return "", fmt.Errorf("published issue number is required")
	}
	target := filepath.Join(i.WorkspaceDir, "issues", strconv.Itoa(issue.Number))
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("issue workspace %q is not a directory", target)
		}
		if _, sourceErr := os.Lstat(path); os.IsNotExist(sourceErr) {
			return target, nil
		} else if sourceErr != nil {
			return "", fmt.Errorf("inspect intake workspace: %w", sourceErr)
		}
		return "", fmt.Errorf("issue workspace %q already exists", target)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect issue workspace: %w", err)
	}
	if err := i.validateChild(path); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", fmt.Errorf("create issue workspace parent: %w", err)
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

const (
	maxDiscoveryReadBytes     = 16 << 10
	maxDiscoveryGlobMatches   = 200
	maxDiscoveryGrepBytes     = 16 << 10
	maxDiscoveryGrepScanBytes = 32 << 20
	discoveryGrepTruncated    = "search truncated\n"
)

// Read returns at most 16 KiB from one regular file below the intake root.
func (i CloneIntake) Read(ctx context.Context, path, relativePath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := i.validateChild(path); err != nil {
		return "", err
	}
	file, err := i.regularChild(path, relativePath)
	if err != nil {
		return "", err
	}
	handle, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("read intake file %q: %w", relativePath, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(handle, maxDiscoveryReadBytes))
	closeErr := handle.Close()
	if readErr != nil {
		return "", fmt.Errorf("read intake file %q: %w", relativePath, readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close intake file %q: %w", relativePath, closeErr)
	}
	return string(body), nil
}

// Glob returns paths for regular files whose base name matches pattern. The
// pattern is deliberately matched against the base name, so *.md also finds
// files nested below the intake root.
func (i CloneIntake) Glob(ctx context.Context, path, pattern string) (string, error) {
	if err := i.validateChild(path); err != nil {
		return "", err
	}
	if _, err := filepath.Match(pattern, ""); err != nil {
		return "", fmt.Errorf("invalid glob pattern: %w", err)
	}
	matches := make([]string, 0, maxDiscoveryGlobMatches)
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve intake path: %w", err)
	}
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		matched, err := filepath.Match(pattern, filepath.Base(relative))
		if err != nil {
			return fmt.Errorf("invalid glob pattern: %w", err)
		}
		if matched {
			matches = append(matches, filepath.ToSlash(relative))
			if len(matches) == maxDiscoveryGlobMatches {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return "", fmt.Errorf("glob intake: %w", err)
	}
	if len(matches) == 0 {
		return "no matches", nil
	}
	return strings.Join(matches, "\n"), nil
}

// Grep returns matching lines as relative-path:line:text records.
func (i CloneIntake) Grep(ctx context.Context, path, pattern string) (string, error) {
	if err := i.validateChild(path); err != nil {
		return "", err
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("compile grep pattern: %w", err)
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve intake path: %w", err)
	}
	var output strings.Builder
	scanned := int64(0)
	truncated := false
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if scanned+info.Size() > maxDiscoveryGrepScanBytes {
			truncated = true
			return filepath.SkipAll
		}
		scanned += info.Size()
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4<<10), 1<<20)
		lineNumber := 0
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				_ = file.Close()
				return err
			}
			lineNumber++
			line := scanner.Text()
			if compiled.MatchString(line) {
				relative, err := filepath.Rel(root, current)
				if err != nil {
					return err
				}
				record := fmt.Sprintf("%s:%d: %s\n", filepath.ToSlash(relative), lineNumber, line)
				if output.Len()+len(record) > maxDiscoveryGrepBytes-len(discoveryGrepTruncated) {
					truncated = true
					_ = file.Close()
					return filepath.SkipAll
				}
				output.WriteString(record)
			}
		}
		scanErr := scanner.Err()
		if closeErr := file.Close(); closeErr != nil && scanErr == nil {
			return closeErr
		}
		// A generated or minified file may contain a line larger than the
		// scanner's bounded token size. Skip that file so it cannot prevent
		// discovery in the rest of the repository.
		if errors.Is(scanErr, bufio.ErrTooLong) {
			return nil
		}
		return scanErr
	})
	if err != nil && err != filepath.SkipAll {
		return "", fmt.Errorf("grep intake: %w", err)
	}
	if truncated {
		return output.String() + discoveryGrepTruncated, nil
	}
	if output.Len() == 0 {
		return "no matches", nil
	}
	return output.String(), nil
}

func (i CloneIntake) validateChild(path string) error {
	base, err := filepath.Abs(i.BaseDir)
	if err != nil {
		return fmt.Errorf("resolve intake base directory: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve intake workspace: %w", err)
	}
	relative, err := filepath.Rel(base, absolute)
	if err != nil {
		return fmt.Errorf("resolve intake workspace: %w", err)
	}
	_, err = i.regularDescendant(i.BaseDir, relative)
	if err != nil {
		return err
	}
	return nil
}

func (i CloneIntake) regularChild(path, name string) (string, error) {
	file, err := i.regularDescendant(path, name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(file)
	if err != nil {
		return file, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("refuse non-regular intake file %q", name)
	}
	return file, nil
}

// regularDescendant resolves a path below root while refusing traversal and
// symlinked path components. The final component may be absent so callers
// that create files can validate its parent before writing it.
func (i CloneIntake) regularDescendant(root, relativePath string) (string, error) {
	base, err := filepath.EvalSymlinks(i.BaseDir)
	if err != nil {
		return "", fmt.Errorf("resolve intake base directory: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve intake root: %w", err)
	}
	rootRelative, err := filepath.Rel(base, resolvedRoot)
	if err != nil || rootRelative == ".." || strings.HasPrefix(rootRelative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refuse operation outside isolated intake directory")
	}
	if filepath.IsAbs(relativePath) || relativePath == "" {
		return "", fmt.Errorf("refuse invalid intake relative path %q", relativePath)
	}
	clean := filepath.Clean(relativePath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refuse operation outside isolated intake directory")
	}
	candidate := filepath.Join(resolvedRoot, clean)
	parts := strings.Split(clean, string(filepath.Separator))
	current := resolvedRoot
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) && index == len(parts)-1 {
			break
		}
		if statErr != nil {
			return "", fmt.Errorf("inspect intake path: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refuse symlinked intake path %q", part)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("refuse non-directory intake path %q", part)
		}
	}
	return candidate, nil
}
