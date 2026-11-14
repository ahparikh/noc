package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"noc.local/noc/internal/buildinfo"
	"noc.local/noc/internal/provider"
)

// TestMain lets the test binary stand in for `codex app-server proxy`: the
// proxy client tests point the executable at os.Args[0] and select a behavior
// through the environment. No real codex is ever executed.
func TestMain(m *testing.M) {
	switch os.Getenv("NOC_TEST_FAKE_PROXY") {
	case "serve":
		_ = standardDaemon().serve(readWriter{os.Stdin, os.Stdout})
		os.Exit(0)
	case "refuse":
		fmt.Fprintln(os.Stderr, "fake proxy refused")
		os.Exit(2)
	case "hang":
		// Serve, then ignore stdin closing so the exit grace must kill us.
		d := standardDaemon()
		_ = d.serve(readWriter{os.Stdin, os.Stdout})
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type readWriter struct {
	r *os.File
	w *os.File
}

func (rw readWriter) Read(p []byte) (int, error)  { return rw.r.Read(p) }
func (rw readWriter) Write(p []byte) (int, error) { return rw.w.Write(p) }

// replay runs fn over a connection whose server side is the given JSONL lines
// and returns the client's messages, decoded.
func replay(t *testing.T, lines string, fn func(context.Context, *rpcConnection) error) ([]map[string]json.RawMessage, error) {
	t.Helper()
	var out bytes.Buffer
	conn := newRPCConnection(newJSONLineMessageTransport(strings.NewReader(lines), &out))
	err := fn(context.Background(), conn)
	var sent []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("client sent invalid JSON %q: %v", line, err)
		}
		sent = append(sent, m)
	}
	return sent, err
}

func discoverFn(agents *[]provider.NativeAgent) func(context.Context, *rpcConnection) error {
	return func(ctx context.Context, c *rpcConnection) error {
		var err error
		*agents, err = discoverFromConnection(ctx, c)
		return err
	}
}

func methodOf(m map[string]json.RawMessage) string {
	var s string
	_ = json.Unmarshal(m["method"], &s)
	return s
}

const initOK = `{"id":1,"result":{}}` + "\n"

func threadLine(id int, thread string) string {
	return fmt.Sprintf(`{"id":%d,"result":{"thread":%s}}`+"\n", id, thread)
}

func TestDiscoverFromConnectionFiltersAndSanitizes(t *testing.T) {
	data, err := os.ReadFile("testdata/discovery.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var agents []provider.NativeAgent
	sent, err := replay(t, string(data), discoverFn(&agents))
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, a := range agents {
		ids = append(ids, a.NativeSessionID)
		for _, banned := range []string{"transcript", "/private/", "path"} {
			if strings.Contains(string(a.Raw), banned) {
				t.Fatalf("%s evidence leaks %q: %s", a.NativeSessionID, banned, a.Raw)
			}
		}
	}
	if got := strings.Join(ids, ","); got != "cli-active,vscode-idle,app-approval,error-live" {
		t.Fatalf("accepted = %s", got)
	}
	if want := `{"status":"active","created_at":1789400000,"updated_at":1789400010,"model":"gpt-6-astra","reasoning_effort":"high","preview":"Build a live agent registry"}`; string(agents[0].Raw) != want {
		t.Fatalf("raw = %s", agents[0].Raw)
	}
	if agents[0].Name != "CLI task" || agents[0].CWD != "/workspace/cli" || agents[0].SourceKind != "cli" || agents[0].NativeShortID != "" {
		t.Fatalf("agent = %+v", agents[0])
	}
	if agents[1].CWD != "/workspace/vscode/./" || !strings.Contains(string(agents[1].Raw), `"preview":"first request"`) ||
		strings.Contains(string(agents[1].Raw), "preview_truncated") {
		t.Fatalf("vscode = %+v raw %s", agents[1], agents[1].Raw)
	}
	if !strings.Contains(string(agents[2].Raw), `"active_flags":["waitingOnApproval","waitingOnUserInput"]`) || agents[2].SourceKind != "appServer" {
		t.Fatalf("approval = %+v raw %s", agents[2], agents[2].Raw)
	}
	if agents[3].Name != "" || agents[3].SourceKind != "cli" {
		t.Fatalf("error-live = %+v", agents[3])
	}

	// Message sequence: initialize, initialized, list, one reject, 8 reads.
	var methods []string
	var rejected bool
	for _, m := range sent {
		if method := methodOf(m); method != "" {
			methods = append(methods, method)
			switch method {
			case "thread/list", "thread/start", "thread/resume", "thread/name/set", "turn/start":
				t.Fatalf("forbidden method %s", method)
			}
			continue
		}
		var id string
		_ = json.Unmarshal(m["id"], &id)
		var e rpcErrorBody
		_ = json.Unmarshal(m["error"], &e)
		if id != "server-1" || e.Code != -32601 || e.Message != "noc does not implement server-initiated requests" {
			t.Fatalf("unexpected reply %v", m)
		}
		rejected = true
	}
	want := "initialize,initialized,thread/loaded/list" + strings.Repeat(",thread/read", 8)
	if strings.Join(methods, ",") != want || !rejected {
		t.Fatalf("methods = %v rejected=%v", methods, rejected)
	}
	for _, m := range sent {
		switch methodOf(m) {
		case "thread/read":
			if !strings.HasSuffix(string(m["params"]), `,"includeTurns":false}`) {
				t.Fatalf("params = %s", m["params"])
			}
		case "initialized":
			if string(m["params"]) != "{}" || m["id"] != nil {
				t.Fatalf("initialized = %v", m)
			}
		case "initialize":
			wantParams := fmt.Sprintf(`{"clientInfo":{"name":"noc","title":"noc agent registry","version":%q},"capabilities":{"experimentalApi":false,"optOutNotificationMethods":["item/agentMessage/delta","item/commandExecution/outputDelta","item/reasoning/summaryTextDelta","item/reasoning/textDelta"]}}`, buildinfo.Version)
			if string(m["params"]) != wantParams || string(m["id"]) != "1" || m["jsonrpc"] != nil {
				t.Fatalf("initialize = %s", m["params"])
			}
		}
	}

	norm, err := New().Normalize(agents[0])
	if err != nil || norm.Name != "CLI task" || norm.Model != "gpt-6-astra" {
		t.Fatalf("normalize = %+v, %v", norm, err)
	}
}

func TestNativeAgentFromThreadBoundsPreviewMetadata(t *testing.T) {
	thread := threadRecord{
		ID:              " t ",
		CWD:             " /w ",
		Source:          json.RawMessage(`"cli"`),
		Model:           strings.Repeat("é", 300),
		ReasoningEffort: strings.Repeat("界", 70),
		Preview:         strings.Repeat("😀", 2100),
	}
	thread.Status.Type = "idle"
	agent, ok, err := nativeAgentFromThread(thread)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	var ev evidence
	if err := json.Unmarshal(agent.Raw, &ev); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(ev.Model)); n != 256 {
		t.Fatalf("model runes = %d", n)
	}
	if n := len([]rune(ev.ReasoningEffort)); n != 64 {
		t.Fatalf("reasoning runes = %d", n)
	}
	if n := len([]rune(ev.Preview)); n != 2048 || !ev.PreviewTruncated {
		t.Fatalf("preview runes = %d truncated=%v", n, ev.PreviewTruncated)
	}
	if agent.NativeSessionID != "t" || agent.CWD != "/w" {
		t.Fatalf("agent = %+v", agent)
	}

	// Exactly at the limit is not truncated; whitespace is compacted first.
	thread.Preview = "  first\n\n  request  "
	thread.Model = strings.Repeat("a", 256)
	agent, _, _ = nativeAgentFromThread(thread)
	ev = evidence{}
	_ = json.Unmarshal(agent.Raw, &ev)
	if ev.Preview != "first request" || ev.PreviewTruncated || len(ev.Model) != 256 {
		t.Fatalf("ev = %+v", ev)
	}
	// Whitespace padding does not count toward the preview limit.
	thread.Preview = strings.Repeat(" ", 3000) + strings.Repeat("x", 2048) + strings.Repeat("\n", 3000)
	agent, _, _ = nativeAgentFromThread(thread)
	ev = evidence{}
	_ = json.Unmarshal(agent.Raw, &ev)
	if len(ev.Preview) != 2048 || ev.PreviewTruncated {
		t.Fatalf("padded preview = %d truncated=%v", len(ev.Preview), ev.PreviewTruncated)
	}

	for name, mutate := range map[string]func(*threadRecord){
		"empty id":   func(r *threadRecord) { r.ID = "  " },
		"notLoaded":  func(r *threadRecord) { r.Status.Type = "notLoaded" },
		"exec":       func(r *threadRecord) { r.Source = json.RawMessage(`"exec"`) },
		"structured": func(r *threadRecord) { r.Source = json.RawMessage(`{"subAgent":"review"}`) },
		"number":     func(r *threadRecord) { r.Source = json.RawMessage(`7`) },
		"null":       func(r *threadRecord) { r.Source = json.RawMessage(`null`) },
		"missing":    func(r *threadRecord) { r.Source = nil },
	} {
		r := thread
		mutate(&r)
		if _, ok, err := nativeAgentFromThread(r); ok || err != nil {
			t.Errorf("%s: ok=%v err=%v", name, ok, err)
		}
	}
}

func TestDiscoverFromConnectionSkipsThreadReadRPCError(t *testing.T) {
	lines := initOK +
		`{"id":2,"result":{"data":["a","b","c","d"]}}` + "\n" +
		`{"id":3,"error":{"code":12345,"message":"Thread is no longer loaded"}}` + "\n" +
		`{"id":4,"error":{"code":-1,"message":"THREAD NOT FOUND: b"}}` + "\n" +
		`{"id":5,"error":{"code":-1,"message":"conversation not loaded"}}` + "\n" +
		threadLine(6, `{"id":"d","source":"cli","status":{"type":"idle"}}`)
	var agents []provider.NativeAgent
	_, err := replay(t, lines, discoverFn(&agents))
	if err != nil || len(agents) != 1 || agents[0].NativeSessionID != "d" {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
}

func TestDiscoverFromConnectionReturnsNonLivenessRPCError(t *testing.T) {
	lines := initOK +
		`{"id":2,"result":{"data":["a","b"]}}` + "\n" +
		threadLine(3, `{"id":"a","source":"cli","status":{"type":"idle"}}`) +
		`{"id":4,"error":{"code":-32600,"message":"boom"}}` + "\n"
	var agents []provider.NativeAgent
	_, err := replay(t, lines, discoverFn(&agents))
	if err == nil || err.Error() != "read loaded Codex thread metadata: Codex app-server error -32600: boom" || agents != nil {
		t.Fatalf("agents = %+v, err = %v", agents, err)
	}
	var rpcErr *rpcCallError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 {
		t.Fatalf("not an rpcCallError: %v", err)
	}
}

func TestDiscoverFromConnectionPaginatesLoadedThreads(t *testing.T) {
	lines := initOK +
		`{"id":2,"result":{"data":["t1","t2"],"nextCursor":"C"}}` + "\n" +
		`{"id":3,"result":{"data":["t3"," ","t1"],"nextCursor":""}}` + "\n" +
		threadLine(4, `{"id":"t1","source":"cli","status":{"type":"idle"}}`) +
		threadLine(5, `{"id":"t2","source":"cli","status":{"type":"idle"}}`) +
		threadLine(6, `{"id":"t3","source":"cli","status":{"type":"idle"}}`)
	var agents []provider.NativeAgent
	sent, err := replay(t, lines, discoverFn(&agents))
	if err != nil || len(agents) != 3 {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
	var lists []string
	for _, m := range sent {
		if methodOf(m) == "thread/loaded/list" {
			lists = append(lists, string(m["params"]))
		}
	}
	if len(lists) != 2 || lists[0] != `{"limit":1000}` || lists[1] != `{"limit":1000,"cursor":"C"}` {
		t.Fatalf("list params = %v", lists)
	}

	repeated := initOK +
		`{"id":2,"result":{"data":["t1"],"nextCursor":"C"}}` + "\n" +
		`{"id":3,"result":{"data":["t2"],"nextCursor":"C"}}` + "\n"
	_, err = replay(t, repeated, discoverFn(&agents))
	if err == nil || err.Error() != "Codex repeated a loaded-thread pagination cursor" {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverFromConnectionRejectsMalformedProtocol(t *testing.T) {
	cases := []struct {
		name  string
		lines string
		want  string
	}{
		{"invalid json", "not json\n", "initialize Codex app-server connection: decode Codex app-server message: "},
		{"closed early", "", "initialize Codex app-server connection: read Codex app-server message: EOF"},
		{"initialize error", `{"id":1,"error":{"code":-1,"message":"nope"}}` + "\n", "initialize Codex app-server connection: Codex app-server error -1: nope"},
		{"foreign ids ignored", `{"id":"1","result":{}}` + "\n" + `{"id":null,"result":{}}` + "\n" + `{"id":1.0,"result":{}}` + "\n" + `{"method":"note"}` + "\n",
			"initialize Codex app-server connection: read Codex app-server message: EOF"},
		{"list type", initOK + `{"id":2,"result":{"data":"nope"}}` + "\n", "list loaded Codex threads: decode thread/loaded/list response: "},
		{"thread type", initOK + `{"id":2,"result":{"data":["a"]}}` + "\n" + threadLine(3, `{"id":5}`),
			"read loaded Codex thread metadata: decode thread/read response: "},
		{"list error", initOK + `{"id":2,"error":{"code":-32601,"message":"no"}}` + "\n", "list loaded Codex threads: Codex app-server error -32601: no"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var agents []provider.NativeAgent
			_, err := replay(t, tc.lines, discoverFn(&agents))
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want prefix %q", err, tc.want)
			}
			if agents != nil {
				t.Fatalf("rows returned with error: %v", agents)
			}
		})
	}

	// A message with a method and "id": null is a server request: rejected.
	lines := `{"id":null,"method":"x"}` + "\n" + initOK + `{"id":2,"result":{"data":[]}}` + "\n"
	var agents []provider.NativeAgent
	sent, err := replay(t, lines, discoverFn(&agents))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range sent {
		if methodOf(m) == "" && string(m["id"]) == "null" && strings.Contains(string(m["error"]), "-32601") {
			found = true
		}
	}
	if !found {
		t.Fatalf("null-id server request not rejected: %v", sent)
	}

	// A done context surfaces as the bare context error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := newRPCConnection(newJSONLineMessageTransport(strings.NewReader(initOK), &bytes.Buffer{}))
	if err := conn.call(ctx, "m", nil, nil); !errors.Is(err, context.Canceled) || err.Error() != "context canceled" {
		t.Fatalf("err = %v", err)
	}
}

func TestSetThreadNameOnConnection(t *testing.T) {
	sent, err := replay(t, initOK+`{"id":2,"result":{}}`+"\n", func(ctx context.Context, c *rpcConnection) error {
		return setThreadNameOnConnection(ctx, c, "t1", "New")
	})
	if err != nil || len(sent) != 3 || methodOf(sent[2]) != "thread/name/set" || string(sent[2]["params"]) != `{"threadId":"t1","name":"New"}` {
		t.Fatalf("sent = %v, err = %v", sent, err)
	}
	_, err = replay(t, initOK+`{"id":2,"error":{"code":-1,"message":"bad"}}`+"\n", func(ctx context.Context, c *rpcConnection) error {
		return setThreadNameOnConnection(ctx, c, "t1", "New")
	})
	if err == nil || err.Error() != "set Codex thread name: Codex app-server error -1: bad" {
		t.Fatalf("err = %v", err)
	}
}

func TestBoundedBuffer(t *testing.T) {
	b := &boundedBuffer{limit: 5}
	n, err := b.Write([]byte("abc"))
	if n != 3 || err != nil || b.String() != "abc" {
		t.Fatal(n, err, b.String())
	}
	if n, _ := b.Write([]byte("defgh")); n != 5 || b.String() != "abcde (truncated)" {
		t.Fatalf("%d %q", n, b.String())
	}
}

func fakeProxy(t *testing.T, mode string) *proxyClient {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOC_TEST_FAKE_PROXY", mode)
	return newProxyClient(exe, 5*time.Second)
}

func TestProxyClientDiscoversAndRenames(t *testing.T) {
	c := fakeProxy(t, "serve")
	agents, err := c.DiscoverLive(context.Background())
	if err != nil || len(agents) != 1 || agents[0].NativeSessionID != "t1" {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
	if err := c.SetThreadName(context.Background(), "t1", "Renamed"); err != nil {
		t.Fatal(err)
	}
}

func TestProxyClientReportsStderrOnFailure(t *testing.T) {
	c := fakeProxy(t, "refuse")
	_, err := c.DiscoverLive(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "open Codex app-server WebSocket: ") ||
		!strings.HasSuffix(err.Error(), ": fake proxy refused") {
		t.Fatalf("err = %v", err)
	}
	missing := newProxyClient("/nonexistent/codex-for-noc-tests", time.Second)
	if _, err := missing.DiscoverLive(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "start Codex app-server proxy: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestProxyClientKillsSlowExitWithoutError(t *testing.T) {
	c := fakeProxy(t, "hang")
	start := time.Now()
	agents, err := c.DiscoverLive(context.Background())
	if err != nil || len(agents) != 1 {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %v", d)
	}
}
