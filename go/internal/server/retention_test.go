package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakePruner struct {
	mu    sync.Mutex
	calls []time.Time
	ch    chan time.Time
	err   error
}

func (f *fakePruner) PruneRetired(_ context.Context, before time.Time) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, before)
	f.mu.Unlock()
	f.ch <- before
	return 1, f.err
}

func TestRetentionLoopSweepsImmediatelyAndOnTick(t *testing.T) {
	p := &fakePruner{ch: make(chan time.Time, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	retire := 14 * 24 * time.Hour
	begin := time.Now().UTC()
	go func() { runRetentionLoop(ctx, p, retire, 20*time.Millisecond, logger); close(done) }()

	var first time.Time
	select {
	case first = <-p.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no immediate sweep")
	}
	if first.Before(begin.Add(-retire)) || first.After(time.Now().UTC().Add(-retire)) {
		t.Fatalf("cutoff %v not now-retireAfter", first)
	}
	select {
	case <-p.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no tick sweep")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop")
	}
}

func TestRetentionLoopContinuesAfterError(t *testing.T) {
	p := &fakePruner{ch: make(chan time.Time, 16), err: errors.New("db busy")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runRetentionLoop(ctx, p, time.Hour, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 0; i < 2; i++ {
		select {
		case <-p.ch:
		case <-time.After(2 * time.Second):
			t.Fatal("loop stopped after error")
		}
	}
}
