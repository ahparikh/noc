package reconcile

import (
	"context"
	"testing"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

func TestBackgroundClaudeSessionsDropHookPaneBindings(t *testing.T) {
	h := newHarness(t)
	obs := h.clock.Add(-time.Minute)
	binding := []apiv1.FrontendBinding{{Kind: "zellij", Session: "s", PaneID: "3", State: "bound", ObservedAt: obs}}

	bg := hookRow("bg", "bg-1", 0, apiv1.LiveStateLive)
	bg.FrontendBindings = binding
	inter := hookRow("inter", "int-1", 0, apiv1.LiveStateLive)
	inter.FrontendBindings = binding
	_ = h.store.Upsert(context.Background(), bg)
	_ = h.store.Upsert(context.Background(), inter)

	nb, ni := norm("bg-1"), norm("int-1")
	nb.SourceKind, ni.SourceKind = "background", "interactive"
	h.ad.set(nb, ni)
	h.run()

	if got := h.get("bg").FrontendBindings; got != nil {
		t.Errorf("background bindings = %+v", got)
	}
	if got := h.get("inter").FrontendBindings; len(got) != 1 {
		t.Errorf("interactive bindings = %+v", got)
	}
}
