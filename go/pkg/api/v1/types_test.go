package apiv1

import "testing"

func TestDeriveDisplayState(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		want  DisplayState
	}{
		{"approval wins", Agent{Attention: Attention{Type: AttentionApprovalRequired}, ActivityState: ActivityStateWorking, LiveState: LiveStateNotLive}, DisplayStateNeedsApproval},
		{"input", Agent{Attention: Attention{Type: AttentionInputRequired}}, DisplayStateNeedsInput},
		{"auth", Agent{Attention: Attention{Type: AttentionAuthenticationRequired}}, DisplayStateNeedsAuthentication},
		{"error", Agent{Attention: Attention{Type: AttentionError}}, DisplayStateError},
		{"unknown attention falls through", Agent{Attention: Attention{Type: AttentionUnknown}, LiveState: LiveStateLive}, DisplayStateIdle},
		{"working ignores liveness", Agent{ActivityState: ActivityStateWorking, LiveState: LiveStateNotLive}, DisplayStateWorking},
		{"waiting", Agent{ActivityState: ActivityStateWaiting}, DisplayStateWaiting},
		{"ready running", Agent{ActivityState: ActivityStateIdle, LiveState: LiveStateLive, RuntimeState: RuntimeStateRunning}, DisplayStateReady},
		{"ready loaded", Agent{ActivityState: ActivityStateIdle, LiveState: LiveStateLive, RuntimeState: RuntimeStateLoaded}, DisplayStateReady},
		{"idle resumable", Agent{ActivityState: ActivityStateIdle, LiveState: LiveStateLive, RuntimeState: RuntimeStateResumable}, DisplayStateIdle},
		{"idle starting", Agent{ActivityState: ActivityStateIdle, LiveState: LiveStateLive, RuntimeState: RuntimeStateStarting}, DisplayStateIdle},
		{"idle empty runtime", Agent{ActivityState: ActivityStateIdle, LiveState: LiveStateLive}, DisplayStateIdle},
		{"unknown activity live", Agent{ActivityState: ActivityStateUnknown, LiveState: LiveStateLive}, DisplayStateIdle},
		{"stale", Agent{LiveState: LiveStateCandidateStale}, DisplayStateStale},
		{"not live", Agent{LiveState: LiveStateNotLive}, DisplayStateNotLive},
		{"unknown live state", Agent{LiveState: LiveStateUnknown}, DisplayStateNotLive},
		{"empty", Agent{}, DisplayStateNotLive},
	}
	for _, tc := range cases {
		if got := DeriveDisplayState(tc.agent); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestDeriveBucketGroupsByWhatTheUserIsNeededFor(t *testing.T) {
	running := func(state DisplayState) Agent {
		return Agent{LiveState: LiveStateLive, RuntimeState: RuntimeStateRunning, DisplayState: state}
	}
	cases := []struct {
		agent Agent
		want  Bucket
	}{
		{running(DisplayStateNeedsApproval), BucketBlocked},
		{running(DisplayStateNeedsInput), BucketBlocked},
		{running(DisplayStateNeedsAuthentication), BucketBlocked},
		{running(DisplayStateError), BucketBlocked},
		{running(DisplayStateWorking), BucketWorking},
		{running(DisplayStateWaiting), BucketWorking},
		{running(DisplayStateReady), BucketReview},
		{running(DisplayStateIdle), BucketQuiet},
		{running(DisplayStateStale), BucketQuiet},
		{running(DisplayStateNotLive), BucketQuiet},
		{running(""), BucketQuiet},
		{Agent{LiveState: LiveStateLive, RuntimeState: RuntimeStateLoaded, DisplayState: DisplayStateReady}, BucketReview},
		{Agent{LiveState: LiveStateLive, RuntimeState: RuntimeStateStarting, DisplayState: DisplayStateWorking}, BucketWorking},
		{Agent{LiveState: LiveStateLive, RuntimeState: RuntimeStateStarting, DisplayState: DisplayStateIdle}, BucketQuiet},
		{Agent{LiveState: LiveStateLive, RuntimeState: RuntimeStateResumable, DisplayState: DisplayStateNeedsInput}, BucketQuiet},
		{Agent{LiveState: LiveStateLive, RuntimeState: RuntimeStateUnknown, DisplayState: DisplayStateWorking}, BucketQuiet},
		{Agent{LiveState: LiveStateNotLive, RuntimeState: RuntimeStateRunning, DisplayState: DisplayStateWorking}, BucketQuiet},
	}
	for i, tc := range cases {
		if got := DeriveBucket(tc.agent); got != tc.want {
			t.Errorf("case %d: got %q want %q", i, got, tc.want)
		}
	}
	if BucketBlocked.Rank() != 0 || BucketWorking.Rank() != 1 || BucketReview.Rank() != 2 || BucketQuiet.Rank() != 3 {
		t.Fatalf("bucket ranks are wrong")
	}
	if Bucket("").Rank() != len(Buckets) || Bucket("bogus").Rank() != len(Buckets) {
		t.Fatalf("unknown buckets must rank last")
	}
}

func TestHasProcess(t *testing.T) {
	for _, rt := range []RuntimeState{RuntimeStateRunning, RuntimeStateLoaded, RuntimeStateStarting} {
		if !HasProcess(Agent{LiveState: LiveStateLive, RuntimeState: rt}) {
			t.Errorf("live %s should have a process", rt)
		}
		if HasProcess(Agent{LiveState: LiveStateCandidateStale, RuntimeState: rt}) {
			t.Errorf("stale %s should not have a process", rt)
		}
	}
	for _, rt := range []RuntimeState{RuntimeStateResumable, RuntimeStateStopped, RuntimeStateFailed, RuntimeStateUnknown, ""} {
		if HasProcess(Agent{LiveState: LiveStateLive, RuntimeState: rt}) {
			t.Errorf("live %q should not have a process", rt)
		}
	}
}

func TestHooksSeeItsTerminalAndNeverEngaged(t *testing.T) {
	if HooksSeeItsTerminal(Agent{Provider: ProviderClaude, SourceKind: "background"}) {
		t.Fatal("background claude hooks run in the daemon")
	}
	if !HooksSeeItsTerminal(Agent{Provider: ProviderClaude}) || !HooksSeeItsTerminal(Agent{Provider: ProviderCodex, SourceKind: "background"}) {
		t.Fatal("other combinations see the terminal")
	}
	if !NeverEngaged(Agent{UserName: " ", Name: "\t", InitialRequest: &TextPreview{Text: "  "}, Summary: "x"}) {
		t.Fatal("whitespace-only fields and a summary do not count as engagement")
	}
	if NeverEngaged(Agent{Name: "n"}) || NeverEngaged(Agent{UserName: "u"}) || NeverEngaged(Agent{InitialRequest: &TextPreview{Text: "p"}}) {
		t.Fatal("a name, user name, or prompt counts as engagement")
	}
}
