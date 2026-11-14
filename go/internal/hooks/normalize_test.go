package hooks

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustNormalize(t *testing.T, provider apiv1.Provider, payload string, at time.Time, env Environment) apiv1.Observation {
	t.Helper()
	obs, err := Normalize(provider, "HOST", []byte(payload), at, env)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return obs
}

func TestNormalizeClaudePromptIsBoundedAndCapturesFrontend(t *testing.T) {
	payload := `{"session_id":"11111111-2222-3333-4444-555555555555","transcript_path":"/tmp/x.jsonl","cwd":"/tmp","hook_event_name":"UserPromptSubmit","prompt_id":"p-1","permission_mode":"default","prompt":"  Fix   the\n\nflaky test  ","unknown_field":"drop me"}`
	at := mustTime(t, "2026-10-27T16:31:34.720315Z")
	obs := mustNormalize(t, apiv1.ProviderClaude, payload, at, Environment{ProcessID: 4242})
	encoded, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	// Golden envelope from the specification.
	want := `{"schema_version":1,"event_id":"hook-f8bc2ad3042289e9a6b615282f816831","provider":"claude","host_id":"HOST","native_session_id":"11111111-2222-3333-4444-555555555555","event_type":"turn.prompted","source":"hook","received_at":"2026-10-27T16:31:34.720315Z","data":{"native_event":"UserPromptSubmit","native_event_id":"p-1","cwd":"/tmp","initial_request":{"text":"Fix the flaky test","source":"hook_first_observed_user_prompt","observed_at":"2026-10-27T16:31:34.720315Z"},"attention_type":"none","permission_mode":"default","process_id":4242}}`
	if maskID(t, obs, encoded) != maskIDString(want) {
		t.Fatalf("envelope mismatch\n got: %s\nwant: %s", encoded, want)
	}
	for _, leaked := range []string{"transcript_path", "/tmp/x.jsonl", "unknown_field", "drop me"} {
		if strings.Contains(string(encoded), leaked) {
			t.Errorf("envelope leaks %q", leaked)
		}
	}

	// Same prompt_id one second apart: same event ID.
	later := mustNormalize(t, apiv1.ProviderClaude, payload, at.Add(time.Second), Environment{ProcessID: 4242})
	if later.EventID != obs.EventID {
		t.Errorf("event IDs differ with native event ID: %s vs %s", later.EventID, obs.EventID)
	}

	// Two bindings.
	withEnv := mustNormalize(t, apiv1.ProviderClaude, payload, at, Environment{
		ZellijSession: "work", ZellijPaneID: "4", ClaudeRemoteControl: true,
	})
	bindings := withEnv.Data.FrontendBindings
	if len(bindings) != 2 || bindings[0].Kind != "zellij" || bindings[0].Session != "work" || bindings[0].PaneID != "4" ||
		bindings[1].Kind != "claude_remote_control" || bindings[1].State != "observed" || !bindings[1].ObservedAt.Equal(at) {
		t.Fatalf("bindings = %+v", bindings)
	}
	// Codex never gets a remote-control binding.
	codex := mustNormalize(t, apiv1.ProviderCodex, payload, at, Environment{ClaudeRemoteControl: true})
	if len(codex.Data.FrontendBindings) != 0 {
		t.Fatalf("codex bindings = %+v", codex.Data.FrontendBindings)
	}
}

func TestNormalizeStopProducesProvenancedSummary(t *testing.T) {
	payload := `{"session_id":"thread-0001","turn_id":"turn-7","hook_event_name":"Stop","cwd":"/work/repo","model":"gpt-test","permission_mode":"default","transcript_path":null,"last_assistant_message":"Done.\n\n  All tests pass.","stop_hook_active":false}`
	at := mustTime(t, "2026-10-27T16:33:59.993892Z")
	obs := mustNormalize(t, apiv1.ProviderCodex, payload, at, Environment{ZellijSession: "work", ZellijPaneID: "4", ProcessID: 4242})
	encoded, _ := json.Marshal(obs)
	want := `{"schema_version":1,"event_id":"hook-ea7cf4b2bebae8c97b1ebb231920d336","provider":"codex","host_id":"HOST","native_session_id":"thread-0001","event_type":"turn.stopped","source":"hook","received_at":"2026-10-27T16:33:59.993892Z","data":{"native_event":"Stop","native_event_id":"turn-7","cwd":"/work/repo","model":"gpt-test","summary":{"text":"Done. All tests pass.","source":"hook_last_assistant_message","observed_at":"2026-10-27T16:33:59.993892Z"},"attention_type":"none","permission_mode":"default","frontend_bindings":[{"kind":"zellij","session":"work","pane_id":"4","state":"observed","observed_at":"2026-10-27T16:33:59.993892Z"}],"process_id":4242}}`
	if maskID(t, obs, encoded) != maskIDString(want) {
		t.Fatalf("envelope mismatch\n got: %s\nwant: %s", encoded, want)
	}

	// Compaction fixture and truncation at 1200.
	compact := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"Stop","last_assistant_message":"Fixed the parser.\n\nTests pass."}`, at, Environment{})
	if compact.Data.Summary == nil || compact.Data.Summary.Text != "Fixed the parser. Tests pass." {
		t.Fatalf("summary = %+v", compact.Data.Summary)
	}
	long := strings.Repeat("é", 1300)
	trunc := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"Stop","last_assistant_message":"`+long+`"}`, at, Environment{})
	if s := trunc.Data.Summary; s == nil || len([]rune(s.Text)) != 1200 || !s.Truncated {
		t.Fatalf("truncation = %+v", s)
	}
	exact := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"Stop","last_assistant_message":"`+strings.Repeat("a", 1200)+`"}`, at, Environment{})
	if exact.Data.Summary.Truncated {
		t.Fatal("exactly 1200 runes must not be truncated")
	}
	// Prompts are bounded to 2048 and only captured on UserPromptSubmit.
	prompt := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"`+strings.Repeat("p", 3000)+`"}`, at, Environment{})
	if r := prompt.Data.InitialRequest; r == nil || len([]rune(r.Text)) != 2048 || !r.Truncated {
		t.Fatalf("initial request = %+v", r)
	}
	other := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"Stop","prompt":"hi"}`, at, Environment{})
	if other.Data.InitialRequest != nil {
		t.Fatal("only UserPromptSubmit captures a prompt")
	}
}

func TestNormalizeCodexInterruptStopsCurrentTurn(t *testing.T) {
	at := mustTime(t, "2026-10-27T16:33:59Z")
	obs := mustNormalize(t, apiv1.ProviderCodex,
		`{"session_id":"t","hook_event_name":"Interrupt","last_assistant_message":"ignored"}`, at, Environment{})
	if obs.EventType != EventTurnStopped || obs.Data.NativeEvent != "Interrupt" || obs.Data.Summary != nil {
		t.Fatalf("interrupt = %+v", obs)
	}
}

func TestNormalizeExcludesClaudeCloud(t *testing.T) {
	at := time.Now()
	payload := []byte(`{"session_id":"s","hook_event_name":"Stop"}`)
	if _, err := Normalize(apiv1.ProviderClaude, "HOST", payload, at, Environment{ClaudeRemote: true}); !errors.Is(err, ErrCloudSession) {
		t.Fatalf("claude cloud err = %v", err)
	}
	// Cloud exclusion precedes parsing.
	if _, err := Normalize(apiv1.ProviderClaude, "HOST", []byte("garbage"), at, Environment{ClaudeRemote: true}); !errors.Is(err, ErrCloudSession) {
		t.Fatalf("cloud before parse err = %v", err)
	}
	if _, err := Normalize(apiv1.ProviderCodex, "HOST", payload, at, Environment{ClaudeRemote: true}); err != nil {
		t.Fatalf("codex must not be excluded: %v", err)
	}
	if _, err := Normalize(apiv1.ProviderClaude, "HOST", payload, at, Environment{ClaudeRemoteControl: true}); err != nil {
		t.Fatalf("remote control must not be excluded: %v", err)
	}
}

func TestLifecycleEventIDSeparatesLaterResume(t *testing.T) {
	payload := `{"session_id":"s","hook_event_name":"SessionStart","cwd":"/w"}`
	at := mustTime(t, "2026-10-27T16:00:00.100Z")
	a := mustNormalize(t, apiv1.ProviderClaude, payload, at, Environment{})
	b := mustNormalize(t, apiv1.ProviderClaude, payload, at.Add(100*time.Millisecond), Environment{})
	c := mustNormalize(t, apiv1.ProviderClaude, payload, at.Add(2*time.Second), Environment{})
	if a.EventID != b.EventID {
		t.Errorf("+100ms IDs differ: %s %s", a.EventID, b.EventID)
	}
	if a.EventID == c.EventID {
		t.Errorf("+2s ID must differ")
	}
	// A bucket boundary between two deliveries yields different IDs.
	x := mustNormalize(t, apiv1.ProviderClaude, payload, mustTime(t, "2026-10-27T16:00:00.950Z"), Environment{})
	y := mustNormalize(t, apiv1.ProviderClaude, payload, mustTime(t, "2026-10-27T16:00:01.050Z"), Environment{})
	if x.EventID == y.EventID {
		t.Errorf("straddling a second boundary must differ")
	}
}

func TestEventIDNativePrecedenceAndEnvironmentSensitivity(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	id := func(payload string, env Environment) string {
		return mustNormalize(t, apiv1.ProviderClaude, payload, at, env).EventID
	}
	prompt := id(`{"session_id":"s","hook_event_name":"Stop","prompt_id":"p","turn_id":"t","tool_use_id":"u"}`, Environment{})
	if mustNormalize(t, apiv1.ProviderClaude, `{"session_id":"s","hook_event_name":"Stop","prompt_id":"p","turn_id":"t"}`, at, Environment{}).Data.NativeEventID != "p" {
		t.Fatal("prompt_id must win")
	}
	if got := mustNormalize(t, apiv1.ProviderClaude, `{"session_id":"s","hook_event_name":"Stop","turn_id":" t ","tool_use_id":"u"}`, at, Environment{}).Data.NativeEventID; got != "t" {
		t.Fatalf("turn_id precedence: %q", got)
	}
	if got := mustNormalize(t, apiv1.ProviderClaude, `{"session_id":"s","hook_event_name":"Stop","tool_use_id":"u"}`, at, Environment{}).Data.NativeEventID; got != "u" {
		t.Fatalf("tool_use_id: %q", got)
	}
	if prompt == id(`{"session_id":"s","hook_event_name":"Stop","prompt_id":"p","turn_id":"t","tool_use_id":"u"}`, Environment{ProcessID: 9}) {
		t.Error("process_id must affect the ID")
	}
	if prompt == id(`{"session_id":"s","hook_event_name":"Stop","prompt_id":"p","turn_id":"t","tool_use_id":"u"}`, Environment{ZellijPaneID: "1"}) {
		t.Error("bindings must affect the ID")
	}
	// With a native ID the receive time is irrelevant.
	later := mustNormalize(t, apiv1.ProviderClaude, `{"session_id":"s","hook_event_name":"Stop","prompt_id":"p","turn_id":"t","tool_use_id":"u"}`, at.Add(time.Hour), Environment{})
	if later.EventID != prompt {
		t.Error("native event ID must remove the time component")
	}
}

func TestReadObservationRejectsOversizedPayload(t *testing.T) {
	at := time.Now()
	envelope := `{"session_id":"s","hook_event_name":"Stop","cwd":"`
	pad := func(total int) string {
		fill := total - len(envelope) - len(`"}`)
		return envelope + strings.Repeat("x", fill) + `"}`
	}
	exact := pad(MaxPayloadBytes)
	if len(exact) != 262144 {
		t.Fatalf("fixture length %d", len(exact))
	}
	if _, err := ReadObservation(strings.NewReader(exact), apiv1.ProviderClaude, "HOST", at, Environment{}); err != nil {
		t.Fatalf("exactly 262144 bytes must be accepted: %v", err)
	}
	over := pad(MaxPayloadBytes + 1)
	_, err := ReadObservation(strings.NewReader(over), apiv1.ProviderClaude, "HOST", at, Environment{})
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversize err = %v", err)
	}
	if err.Error() != "hook payload exceeds the size limit" {
		t.Fatalf("message = %q", err)
	}
	_, err = ReadObservation(errReader{}, apiv1.ProviderClaude, "HOST", at, Environment{})
	if err == nil || !strings.HasPrefix(err.Error(), "read hook payload: ") {
		t.Fatalf("read error = %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestNotificationAttentionIgnoresIdleNudges(t *testing.T) {
	cases := map[string]apiv1.AttentionType{
		"permission_prompt":  apiv1.AttentionApprovalRequired,
		"Approval_Needed":    apiv1.AttentionApprovalRequired,
		"elicitation_dialog": apiv1.AttentionInputRequired,
		"input_needed":       apiv1.AttentionInputRequired,
		"auth_required":      apiv1.AttentionAuthenticationRequired,
		"oauth_expired":      apiv1.AttentionAuthenticationRequired,
		"auth_success":       apiv1.AttentionNone,
		"idle_prompt":        apiv1.AttentionNone,
		"":                   apiv1.AttentionNone,
	}
	for kind, want := range cases {
		if got := notificationAttention(kind); got != want {
			t.Errorf("notificationAttention(%q) = %q, want %q", kind, got, want)
		}
	}
	at := time.Now()
	obs := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"Notification","notification_type":"idle_prompt","message":"Claude is waiting"}`, at, Environment{})
	if obs.Data.AttentionType != apiv1.AttentionNone || obs.EventType != EventNotification {
		t.Fatalf("idle nudge = %+v", obs.Data)
	}
}

func TestNormalizeNotificationGolden(t *testing.T) {
	payload := `{"session_id":"sess-0001","hook_event_name":"Notification","cwd":"/work/repo","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash","transcript_path":"/x"}`
	at := mustTime(t, "2026-10-27T16:34:00.10318Z")
	obs := mustNormalize(t, apiv1.ProviderClaude, payload, at, Environment{
		ZellijSession: "work", ZellijPaneID: "4", ClaudeRemoteControl: true, ProcessID: 4242,
	})
	encoded, _ := json.Marshal(obs)
	want := `{"schema_version":1,"event_id":"hook-862c29f8eba0658e415b32a2fc4ef20f","provider":"claude","host_id":"HOST","native_session_id":"sess-0001","event_type":"notification.received","source":"hook","received_at":"2026-10-27T16:34:00.10318Z","data":{"native_event":"Notification","cwd":"/work/repo","attention_type":"approval_required","attention_detail":"Claude needs your permission to use Bash","frontend_bindings":[{"kind":"zellij","session":"work","pane_id":"4","state":"observed","observed_at":"2026-10-27T16:34:00.10318Z"},{"kind":"claude_remote_control","state":"observed","observed_at":"2026-10-27T16:34:00.10318Z"}],"process_id":4242}}`
	if maskID(t, obs, encoded) != maskIDString(want) {
		t.Fatalf("envelope mismatch\n got: %s\nwant: %s", encoded, want)
	}
}

func TestNormalizeEventMappings(t *testing.T) {
	at := mustTime(t, "2026-10-27T16:00:00Z")
	type want struct {
		eventType string
		attention apiv1.AttentionType
	}
	cases := map[string]want{
		"SessionStart":      {EventSessionStarted, apiv1.AttentionNone},
		"SessionEnd":        {EventSessionEnded, apiv1.AttentionNone},
		"UserPromptSubmit":  {EventTurnPrompted, apiv1.AttentionNone},
		"Stop":              {EventTurnStopped, apiv1.AttentionNone},
		"Interrupt":         {EventTurnStopped, apiv1.AttentionNone},
		"StopFailure":       {EventTurnFailed, apiv1.AttentionError},
		"CwdChanged":        {EventWorkspaceChanged, apiv1.AttentionNone},
		"PostModelSwitch":   {EventModelChanged, apiv1.AttentionNone},
		"PermissionRequest": {EventApprovalRequired, apiv1.AttentionApprovalRequired},
		"Notification":      {EventNotification, apiv1.AttentionNone},
		"Bogus":             {EventHookObserved, apiv1.AttentionNone},
		"stop":              {EventHookObserved, apiv1.AttentionNone}, // case-sensitive
	}
	for native, w := range cases {
		obs := mustNormalize(t, apiv1.ProviderClaude, `{"session_id":"s","hook_event_name":"`+native+`"}`, at, Environment{})
		if obs.EventType != w.eventType || obs.Data.AttentionType != w.attention || obs.Data.NativeEvent != native {
			t.Errorf("%s -> %s/%s, want %s/%s", native, obs.EventType, obs.Data.AttentionType, w.eventType, w.attention)
		}
	}

	failed := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"StopFailure","error":"rate_limit","message":"other","last_assistant_message":"partial"}`, at, Environment{})
	if failed.Data.AttentionDetail != "rate_limit" || failed.Data.Summary == nil || failed.Data.Summary.Text != "partial" {
		t.Fatalf("StopFailure = %+v", failed.Data)
	}
	fallback := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"StopFailure","error":"  ","message":"fell\n back"}`, at, Environment{})
	if fallback.Data.AttentionDetail != "fell back" {
		t.Fatalf("detail fallback = %q", fallback.Data.AttentionDetail)
	}

	cwd := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"CwdChanged","cwd":"/old","old_cwd":"/older","new_cwd":"/work/repo/sub"}`, at, Environment{})
	if cwd.Data.CWD != "/work/repo/sub" {
		t.Fatalf("cwd = %q", cwd.Data.CWD)
	}
	noNew := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"CwdChanged","cwd":"/old","new_cwd":" "}`, at, Environment{})
	if noNew.Data.CWD != "/old" {
		t.Fatalf("cwd fallback = %q", noNew.Data.CWD)
	}
	elsewhere := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"Stop","cwd":"/old","new_cwd":"/new"}`, at, Environment{})
	if elsewhere.Data.CWD != "/old" {
		t.Fatalf("new_cwd only applies to CwdChanged, got %q", elsewhere.Data.CWD)
	}

	model := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"PostModelSwitch","model":"old-model","to_model":"new-model"}`, at, Environment{})
	if model.EventType != EventModelChanged || model.Data.Model != "new-model" {
		t.Fatalf("model = %+v", model.Data)
	}
	stopModel := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"Stop","model":"m","to_model":"n"}`, at, Environment{})
	if stopModel.Data.Model != "n" {
		t.Fatalf("to_model overrides on any event, got %q", stopModel.Data.Model)
	}

	perm := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"PermissionRequest","message":"Run   rm -rf?\n","tool_name":"Bash"}`, at, Environment{})
	if perm.Data.AttentionDetail != "Run rm -rf?" {
		t.Fatalf("permission detail = %q", perm.Data.AttentionDetail)
	}

	end := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`, at, Environment{})
	if end.EventType != EventSessionEnded || end.Data.Reason != "prompt_input_exit" {
		t.Fatalf("session end = %+v", end.Data)
	}

	named := mustNormalize(t, apiv1.ProviderClaude,
		`{"session_id":"s","hook_event_name":"SessionStart","session_title":"  My   title ","source_kind":"background","permission_mode":"plan"}`, at, Environment{})
	if named.Data.Name != "My   title" || named.Data.SourceKind != "background" || named.Data.PermissionMode != "plan" {
		t.Fatalf("metadata = %+v", named.Data)
	}
}

func TestNormalizeRejectsInvalidPayloads(t *testing.T) {
	at := time.Now()
	cases := []struct {
		name, payload, want string
	}{
		{"not json", `not json`, "hook payload is not valid JSON"},
		{"wrong type", `{"session_id":"s","hook_event_name":"Bogus","cwd":5}`, "hook payload is not valid JSON"},
		{"array", `[1,2]`, "hook payload is not valid JSON"},
		{"string", `"x"`, "hook payload is not valid JSON"},
		{"null", `null`, "hook payload has no session_id"},
		{"no session", `{"hook_event_name":"Stop"}`, "hook payload has no session_id"},
		{"blank session", `{"session_id":"  ","hook_event_name":"Stop"}`, "hook payload has no session_id"},
		{"no event", `{"session_id":"s"}`, "hook payload has no hook_event_name"},
	}
	for _, tc := range cases {
		_, err := Normalize(apiv1.ProviderClaude, "HOST", []byte(tc.payload), at, Environment{})
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// Case-insensitive keys and null values.
	obs := mustNormalize(t, apiv1.ProviderClaude, `{"Session_ID":"s","HOOK_EVENT_NAME":"Stop","cwd":null}`, at, Environment{})
	if obs.NativeSessionID != "s" || obs.Data.CWD != "" {
		t.Fatalf("case-insensitive = %+v", obs)
	}
	// Preconditions precede parsing.
	if _, err := Normalize("gemini", "HOST", []byte("x"), at, Environment{}); err == nil || err.Error() != `unsupported hook provider "gemini"` {
		t.Fatalf("provider err = %v", err)
	}
	if _, err := Normalize(apiv1.ProviderClaude, "", []byte("x"), at, Environment{}); err == nil || err.Error() != "hook host ID is required" {
		t.Fatalf("host err = %v", err)
	}
}

func TestRejectedObservationShape(t *testing.T) {
	at := mustTime(t, "2026-10-27T16:31:35.484141Z")
	obs := RejectedObservation(apiv1.ProviderCodex, "HOST", at, errors.New("hook payload is not valid JSON"))
	encoded, _ := json.Marshal(obs)
	want := `{"schema_version":1,"event_id":"hook-88c40d13f273582187f6dd2390ad4681","provider":"codex","host_id":"HOST","event_type":"hook.rejected","source":"hook","received_at":"2026-10-27T16:31:35.484141Z","data":{"native_event":"unknown"},"error":"hook payload is not valid JSON"}`
	if maskID(t, obs, encoded) != maskIDString(want) {
		t.Fatalf("rejected mismatch\n got: %s\nwant: %s", encoded, want)
	}
	other := RejectedObservation(apiv1.ProviderCodex, "HOST", at.Add(time.Nanosecond), errors.New("hook payload is not valid JSON"))
	if other.EventID == obs.EventID {
		t.Error("distinct invocations must not dedupe")
	}
	nilErr := RejectedObservation(apiv1.ProviderClaude, "HOST", at, nil)
	if nilErr.Error != "hook payload was rejected" {
		t.Fatalf("nil error text = %q", nilErr.Error)
	}
	long := RejectedObservation(apiv1.ProviderClaude, "HOST", at, errors.New("a\n\n"+strings.Repeat("b", 600)))
	if len([]rune(long.Error)) != 512 || !strings.HasPrefix(long.Error, "a bbb") {
		t.Fatalf("long error = %d runes", len([]rune(long.Error)))
	}
}

func TestBoundedAndCompactText(t *testing.T) {
	if got := bounded("  héllo wörld  ", 5); got != "héllo" {
		t.Errorf("bounded = %q", got)
	}
	if got := bounded("  abc  ", 10); got != "abc" {
		t.Errorf("bounded trim = %q", got)
	}
	if got := compactText("\n a \t b  c\n"); got != "a b c" {
		t.Errorf("compactText = %q", got)
	}
	if got := bounded(strings.Repeat("x", 600), maxMetadataRunes); len(got) != 512 {
		t.Errorf("metadata bound = %d", len(got))
	}
}

func TestEnvironmentFromOS(t *testing.T) {
	t.Setenv("ZELLIJ_SESSION_NAME", "sess")
	t.Setenv("ZELLIJ_PANE_ID", "7")
	t.Setenv("CLAUDE_CODE_REMOTE", "TRUE")
	t.Setenv("CLAUDE_CODE_BRIDGE_SESSION_ID", "secret-value")
	env := EnvironmentFromOS()
	if env.ZellijSession != "sess" || env.ZellijPaneID != "7" || env.ClaudeRemote || !env.ClaudeRemoteControl {
		t.Fatalf("env = %+v", env)
	}
	t.Setenv("CLAUDE_CODE_REMOTE", "true")
	t.Setenv("CLAUDE_CODE_BRIDGE_SESSION_ID", "")
	env = EnvironmentFromOS()
	if !env.ClaudeRemote || env.ClaudeRemoteControl {
		t.Fatalf("env = %+v", env)
	}
}

// The specification's golden event IDs were computed with the real (sanitized
// in the document) host and process IDs, so goldens compare with the event ID
// masked; the ID itself is checked for shape.
var eventIDPattern = regexp.MustCompile(`"event_id":"hook-[0-9a-f]{32}"`)

func maskIDString(s string) string {
	return eventIDPattern.ReplaceAllString(s, `"event_id":"ID"`)
}

func maskID(t *testing.T, obs apiv1.Observation, encoded []byte) string {
	t.Helper()
	if !eventIDPattern.Match(encoded) {
		t.Fatalf("event ID has the wrong shape: %s", obs.EventID)
	}
	return maskIDString(string(encoded))
}
