package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultStdoutLimit = 8 << 20
	defaultStderrLimit = 64 << 10
	// waitDelay bounds how long Run waits for inherited output pipes after the
	// context ends, so a wrapper executable's grandchild cannot hang a poll.
	waitDelay = 2 * time.Second
)

// ExecCommandRunner runs commands as a literal argv with no shell, capturing
// bounded stdout and stderr.
type ExecCommandRunner struct {
	StdoutLimit int
	StderrLimit int
}

// NewExecCommandRunner returns a runner with the default output caps.
func NewExecCommandRunner() *ExecCommandRunner {
	return &ExecCommandRunner{}
}

// Run executes name with args, inheriting noc's environment and working
// directory, with no stdin. The child is killed when ctx ends.
func (r *ExecCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	stdoutLimit, stderrLimit := r.StdoutLimit, r.StderrLimit
	if stdoutLimit <= 0 {
		stdoutLimit = defaultStdoutLimit
	}
	if stderrLimit <= 0 {
		stderrLimit = defaultStderrLimit
	}
	stdout := &cappedBuffer{limit: stdoutLimit}
	stderr := &cappedBuffer{limit: stderrLimit}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	runErr := cmd.Run()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("run provider command %q: %w", name, ctxErr)
	}
	if runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if stderr.truncated {
			detail += " (truncated)"
		}
		if strings.TrimSpace(detail) != "" {
			return nil, fmt.Errorf("run provider command %q: %w: %s", name, runErr, detail)
		}
		return nil, fmt.Errorf("run provider command %q: %w", name, runErr)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("provider command %q exceeded the %d-byte output limit", name, stdoutLimit)
	}
	return bytes.Clone(stdout.Bytes()), nil
}

// cappedBuffer keeps the first limit bytes, discards the rest, and always
// reports the full write length so the child never sees a short write.
type cappedBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return 0, errors.New("buffer limit must be positive")
	}
	room := b.limit - b.buf.Len()
	if room <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *cappedBuffer) String() string { return b.buf.String() }
