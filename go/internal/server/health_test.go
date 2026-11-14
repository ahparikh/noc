package server

import (
	"testing"
	"time"

	"noc.local/noc/internal/provider"
	apiv1 "noc.local/noc/pkg/api/v1"
)

func TestProviderHealthPreservesFirstDiscoveryResult(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr := newProviderHealthTracker([]provider.ProbeResult{{
		Provider: apiv1.ProviderClaude, Available: true, Version: "2.0", Features: map[string]bool{"discover_live": true}, ObservedAt: t0,
	}})
	h := tr.snapshot()[0]
	if h.Status != "available" || h.StartupStatus != "" || h.StartupObservedAt != nil || h.DiscoverySource != "claude agents --json" || h.Version != "2.0" {
		t.Fatalf("initial = %+v", h)
	}

	t1, t2 := t0.Add(time.Second), t0.Add(2*time.Second)
	tr.update(apiv1.ProviderClaude, "available", "discovery active; observed 2 live sessions", t1)
	tr.update(apiv1.ProviderClaude, "degraded", "boom", t2)
	h = tr.snapshot()[0]
	if h.Status != "degraded" || h.Detail != "boom" || !h.ObservedAt.Equal(t2) {
		t.Fatalf("current = %+v", h)
	}
	if h.StartupStatus != "available" || h.StartupDetail != "discovery active; observed 2 live sessions" || h.StartupObservedAt == nil || !h.StartupObservedAt.Equal(t1) || h.Version != "2.0" {
		t.Fatalf("startup fields = %+v", h)
	}
}

func TestProviderHealthUsesUnavailableProbeAsStartupResult(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr := newProviderHealthTracker([]provider.ProbeResult{{
		Provider: apiv1.ProviderCodex, Available: false, Diagnostic: "codex not found", ObservedAt: t0,
	}})
	h := tr.snapshot()[0]
	if h.Status != "unavailable" || h.StartupStatus != "unavailable" || h.StartupDetail != "codex not found" || h.StartupObservedAt == nil || !h.StartupObservedAt.Equal(t0) ||
		h.DiscoverySource != "Codex app-server loaded-thread snapshot" {
		t.Fatalf("health = %+v", h)
	}
	tr.update(apiv1.ProviderCodex, "available", "x", t0.Add(time.Minute))
	h = tr.snapshot()[0]
	if h.StartupStatus != "unavailable" || h.StartupDetail != "codex not found" {
		t.Fatalf("startup mutated: %+v", h)
	}
}

func TestProviderHealthInitialStatusMapping(t *testing.T) {
	cases := []struct {
		probe provider.ProbeResult
		want  string
	}{
		{provider.ProbeResult{Provider: "x", Diagnostic: "provider discovery disabled"}, "disabled"},
		{provider.ProbeResult{Provider: "x", Diagnostic: provider.ErrNotImplemented.Error()}, "not_implemented"},
		{provider.ProbeResult{Provider: "x", Available: true}, "available"},
		{provider.ProbeResult{Provider: "x", Diagnostic: "nope"}, "unavailable"},
	}
	for _, c := range cases {
		h := newProviderHealthTracker([]provider.ProbeResult{c.probe}).snapshot()[0]
		if h.Status != c.want || h.StartupStatus != c.want || h.DiscoverySource != "provider adapter" {
			t.Errorf("%+v => %+v, want %s", c.probe, h, c.want)
		}
	}
}

func TestProviderHealthAvailableWithoutDiscoverLive(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newProviderHealthTracker([]provider.ProbeResult{{Provider: apiv1.ProviderCodex, Available: true, ObservedAt: t0}}).snapshot()[0]
	if h.Status != "available" || h.Detail != "" || h.StartupStatus != "available" || h.StartupObservedAt == nil {
		t.Fatalf("health = %+v", h)
	}
}

func TestProviderHealthSnapshotOrderAndUnknownProvider(t *testing.T) {
	tr := newProviderHealthTracker([]provider.ProbeResult{{Provider: "b"}, {Provider: "a"}})
	tr.update("zzz", "available", "", time.Now())
	snap := tr.snapshot()
	if len(snap) != 2 || snap[0].Provider != "b" || snap[1].Provider != "a" {
		t.Fatalf("snapshot = %+v", snap)
	}
	snap[0].Status = "mutated"
	if tr.snapshot()[0].Status == "mutated" {
		t.Fatal("snapshot aliases tracker state")
	}
}
