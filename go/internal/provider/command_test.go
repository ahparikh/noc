package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{limit: 4}
	n, err := b.Write([]byte("abcdef"))
	if err != nil || n != 6 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if b.String() != "abcd" || !b.truncated {
		t.Fatalf("buffer %q truncated=%v", b.String(), b.truncated)
	}
	n, _ = b.Write([]byte("g"))
	if n != 1 || b.String() != "abcd" {
		t.Fatalf("overflow write n=%d %q", n, b.String())
	}
	bad := &cappedBuffer{}
	if _, err := bad.Write([]byte("x")); err == nil {
		t.Fatal("non-positive limit must fail")
	}
}

func TestExecCommandRunnerClassifiesErrors(t *testing.T) {
	r := NewExecCommandRunner()
	ctx := context.Background()
	out, err := r.Run(ctx, "sh", "-c", "printf hello")
	if err != nil || string(out) != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	_, err = r.Run(ctx, "sh", "-c", "echo no such subcommand >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), `run provider command "sh": exit status 3: no such subcommand`) {
		t.Fatalf("stderr detail: %v", err)
	}
	_, err = r.Run(ctx, "definitely-not-a-binary-noc")
	if err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("missing binary: %v", err)
	}
	small := &ExecCommandRunner{StdoutLimit: 3}
	_, err = small.Run(ctx, "sh", "-c", "printf abcdef")
	if err == nil || err.Error() != `provider command "sh" exceeded the 3-byte output limit` {
		t.Fatalf("overflow: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = r.Run(short, "sh", "-c", "sleep 5")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("timeout was not bounded: %v", time.Since(start))
	}
}
