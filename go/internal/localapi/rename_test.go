package localapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"noc.local/noc/internal/provider"
	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

type fakeRenamer struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeRenamer) SetName(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id+"="+name)
	return f.err
}

func TestRenamePushesNameToProvidersThatSupportIt(t *testing.T) {
	mem := registry.NewMemory()
	ctx := ctx5(t)
	codex := newAgent("c1")
	claude := newAgent("k1")
	claude.Provider = apiv1.ProviderClaude
	_ = mem.Upsert(ctx, codex)
	_ = mem.Upsert(ctx, claude)

	renamer := &fakeRenamer{}
	h := NewHandler(mem, testHealth)
	h.SetRenamers(map[apiv1.Provider]provider.Renamer{apiv1.ProviderCodex: renamer})
	_, client := serve(t, h)

	res, err := client.RenameAgent(ctx, "c1", "  Cache work ")
	if err != nil || res.ProviderSync != apiv1.ProviderNameSynced || res.ProviderSyncError != "" || res.Agent.UserName != "Cache work" {
		t.Fatalf("synced: %+v %v", res, err)
	}
	if len(renamer.calls) != 1 || renamer.calls[0] != "thread-c1=Cache work" {
		t.Fatalf("calls = %v", renamer.calls)
	}

	res, err = client.RenameAgent(ctx, "k1", "Name")
	if err != nil || res.ProviderSync != apiv1.ProviderNameUnsupported {
		t.Fatalf("unsupported (no renamer): %+v %v", res, err)
	}

	res, err = client.RenameAgent(ctx, "c1", "   ")
	if err != nil || res.ProviderSync != apiv1.ProviderNameNotAttempted || len(renamer.calls) != 1 {
		t.Fatalf("not_attempted: %+v %v calls=%v", res, err, renamer.calls)
	}

	renamer.err = fmt.Errorf("wrapped: %w", provider.ErrNotImplemented)
	res, err = client.RenameAgent(ctx, "c1", "Again")
	if err != nil || res.ProviderSync != apiv1.ProviderNameUnsupported || res.ProviderSyncError != "" {
		t.Fatalf("ErrNotImplemented: %+v %v", res, err)
	}

	renamer.err = errors.New("line one\n\n  line   two " + strings.Repeat("é", 400))
	res, err = client.RenameAgent(ctx, "c1", "Failing")
	if err != nil || res.ProviderSync != apiv1.ProviderNameFailed || res.Agent.UserName != "Failing" {
		t.Fatalf("failed: %+v %v", res, err)
	}
	want := "line one line two " + strings.Repeat("é", 282) + "…"
	if res.ProviderSyncError != want {
		t.Fatalf("error header = %q", res.ProviderSyncError)
	}
	stored, _ := mem.Get(ctx, "c1")
	if stored.UserName != "Failing" {
		t.Fatalf("stored name lost: %q", stored.UserName)
	}
}

func TestRenameEmptyObjectBodyClears(t *testing.T) {
	mem := registry.NewMemory()
	ctx := ctx5(t)
	_ = mem.Upsert(ctx, newAgent("a1"))
	_, _ = mem.SetUserName(ctx, "a1", "old")
	socket, _ := serve(t, NewHandler(mem, testHealth))
	resp, body := rawDo(t, socket, "PUT", "/v1/agents/a1/name", `{"name":""}`)
	if resp.StatusCode != 200 || strings.Contains(body, "user_name") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}
