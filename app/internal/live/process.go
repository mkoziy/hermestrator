package live

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// LineEvent is a single line of output from a running process.
type LineEvent struct {
	Stream string    // "stdout" or "stderr"
	Text   string    // line content without trailing newline
	Time   time.Time // when the line was received
}

// lineResult is an internal type for passing lines or errors through the
// concurrent reader channel.
type lineResult struct {
	event LineEvent
	err   error
}

// RunResult captures the final state of a completed or cancelled process run.
type RunResult struct {
	ExitCode  int           // -1 if the process was signalled before exit
	Duration  time.Duration // wall-clock time from start to completion
	Lines     []LineEvent   // all captured lines in arrival order
	Cancelled bool          // true when the run was stopped by context cancellation
}

// ProcessRunner executes a subprocess with streaming output and process-group
// cancellation. It uses the same injectable-Command convention as GHPublisher
// and CloneIntake so tests can supply coreutils commands.
type ProcessRunner struct {
	// Command returns a prepared *exec.Cmd. Tests inject coreutils here;
	// production uses exec.CommandContext. The runner adjusts
	// cmd.SysProcAttr before starting the child.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
	// Dir is the working directory for the subprocess. When empty, the
	// subprocess inherits the parent's working directory.
	Dir string
}

// Run starts name with args, streams stdout and stderr line-by-line to onLine,
// and blocks until the process exits or ctx is cancelled. When ctx is
// cancelled the runner sends SIGKILL to the entire process group.
//
// onLine may be nil; when non-nil it is called for every line as it arrives.
// If onLine returns an error, the process is killed and that error is returned.
func (r *ProcessRunner) Run(ctx context.Context, onLine func(LineEvent) error, name string, args ...string) (*RunResult, error) {
	command := r.Command
	if command == nil {
		command = exec.CommandContext
	}

	cmd := command(ctx, name, args...)

	// Set the working directory if specified and exists.
	if r.Dir != "" {
		if _, err := os.Stat(r.Dir); err == nil {
			cmd.Dir = r.Dir
		}
	}

	// Put the child in its own process group so a single signal kills the
	// entire tree, not just the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("process runner stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("process runner stderr pipe: %w", err)
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("process runner start: %w", err)
	}

	// Read stdout and stderr concurrently into a buffered channel.
	const chanBuf = 128
	linesCh := make(chan lineResult, chanBuf)

	// WaitGroup tracks when both readers have finished.
	var readersWG sync.WaitGroup
	readersWG.Add(2)
	go readLines(stdout, "stdout", linesCh, &readersWG)
	go readLines(stderr, "stderr", linesCh, &readersWG)

	// Context watcher: kill the whole process group on cancellation.
	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-ctxDone:
		}
	}()

	// Collect lines until both readers finish and the channel is drained.
	var lines []LineEvent
	var readErr error
	eofCount := 0

	for eofCount < 2 {
		lr, ok := <-linesCh
		if !ok {
			break
		}
		if lr.err != nil {
			if lr.err != io.EOF {
				readErr = lr.err
			}
			eofCount++
			continue
		}
		lines = append(lines, lr.event)
		if onLine != nil && readErr == nil {
			if err := onLine(lr.event); err != nil {
				readErr = err
				// Kill the process group so we don't leak it.
				if cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
			}
		}
	}

	// Wait for the process to finish.
	waitErr := cmd.Wait()
	close(ctxDone)

	duration := time.Since(start)
	cancelled := isContextCancelled(ctx) || (waitErr != nil && isSignalExit(waitErr))

	result := &RunResult{
		ExitCode:  exitCode(waitErr),
		Duration:  duration,
		Lines:     lines,
		Cancelled: cancelled,
	}

	if readErr != nil {
		return result, readErr
	}

	return result, nil
}

// readLines reads from r line-by-line and sends each line into ch. On
// EOF it sends an error of io.EOF. The WaitGroup is decremented when the
// reader finishes.
func readLines(r io.Reader, stream string, ch chan<- lineResult, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		ch <- lineResult{event: LineEvent{Stream: stream, Text: scanner.Text(), Time: time.Now()}}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	ch <- lineResult{err: err}
}

// exitCode extracts the process exit code from a *exec.ExitError. Returns -1
// for signal termination and 0 for a nil error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

// isSignalExit reports whether err indicates the process was terminated by a
// signal rather than a normal exit.
func isSignalExit(err error) bool {
	if err == nil {
		return false
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode() == -1
	}
	return false
}

// isContextCancelled returns true when ctx is done, indicating the run was
// deliberately cancelled by the caller (not just a child signal).
func isContextCancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
