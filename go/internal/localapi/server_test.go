package localapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func rawDo(t *testing.T, socket, method, path, body string) (*http.Response, string) {
	t.Helper()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(method, "http://m"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestClientAndServerOverUnixSocket(t *testing.T) {
	mem := registry.NewMemory()
	var wakes atomic.Int32
	h := NewHandler(mem, testHealth, func() { wakes.Add(1) })
	socket, client := serve(t, h)
	ctx := ctx5(t)

	health, err := client.Health(ctx)
	if err != nil || health.Status != "ok" || health.HostID != "host-1" {
		t.Fatalf("health = %+v, %v", health, err)
	}

	list, err := client.ListAgents(ctx, false)
	if err != nil || list.Agents == nil || len(list.Agents) != 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	_, body := rawDo(t, socket, "GET", "/v1/agents", "")
	if !strings.Contains(body, `"agents":[]`) || !strings.HasSuffix(body, "}\n") {
		t.Fatalf("body = %q", body)
	}

	if err := mem.Upsert(ctx, newAgent("a1")); err != nil {
		t.Fatal(err)
	}
	got, err := client.SetAgentName(ctx, "a1", "  Cache work  ")
	if err != nil || got.UserName != "Cache work" {
		t.Fatalf("set name = %+v, %v", got, err)
	}
	agent, err := client.GetAgent(ctx, "a1")
	if err != nil || agent.UserName != "Cache work" {
		t.Fatalf("get = %+v, %v", agent, err)
	}
	got, err = client.SetAgentName(ctx, "a1", "   ")
	if err != nil || got.UserName != "" {
		t.Fatalf("clear = %+v, %v", got, err)
	}

	if err := client.WakeHooks(ctx); err != nil {
		t.Fatal(err)
	}
	if wakes.Load() != 1 {
		t.Fatalf("wakes = %d", wakes.Load())
	}
}

type frames struct {
	ch chan apiv1.AgentList
}

func startWatch(t *testing.T, client *Client, all bool) (*frames, context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &frames{ch: make(chan apiv1.AgentList, 32)}
	errc := make(chan error, 1)
	go func() {
		errc <- client.WatchAgents(ctx, all, func(l apiv1.AgentList) error { f.ch <- l; return nil })
	}()
	return f, cancel, errc
}

func (f *frames) next(t *testing.T) apiv1.AgentList {
	t.Helper()
	select {
	case l := <-f.ch:
		return l
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for frame")
		return apiv1.AgentList{}
	}
}

func (f *frames) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case l := <-f.ch:
		t.Fatalf("unexpected frame: %+v", l)
	case <-time.After(d):
	}
}

func TestWatchAgentsStreamsSemanticSnapshots(t *testing.T) {
	mem := registry.NewMemory()
	h := NewHandler(mem, testHealth)
	h.SetWatchInterval(10 * time.Millisecond)
	_, client := serve(t, h)
	ctx := context.Background()

	f, cancel, errc := startWatch(t, client, false)
	first := f.next(t)
	if first.Agents == nil || len(first.Agents) != 0 || first.SchemaVersion != 1 {
		t.Fatalf("initial = %+v", first)
	}

	a := newAgent("a1")
	_ = mem.Upsert(ctx, a)
	if l := f.next(t); len(l.Agents) != 1 {
		t.Fatalf("add frame = %+v", l)
	}

	if _, err := mem.SetUserName(ctx, "a1", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if l := f.next(t); l.Agents[0].UserName != "Renamed" {
		t.Fatalf("rename frame = %+v", l)
	}

	// Freshness-only change emits nothing. Upsert keeps the user name overlay.
	a.UserName = ""
	a.LastSeenAt = a.LastSeenAt.Add(time.Minute)
	ts := time.Now()
	a.SnapshotConfirmedAt = &ts
	_ = mem.Upsert(ctx, a)
	f.expectNone(t, 50*time.Millisecond)

	a.LiveState = apiv1.LiveStateNotLive
	_ = mem.Upsert(ctx, a)
	if l := f.next(t); len(l.Agents) != 0 {
		t.Fatalf("removal frame = %+v", l)
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrConnection) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not end")
	}
}

func TestWatchAgentsPreservesCanceledContext(t *testing.T) {
	_, client := serve(t, NewHandler(registry.NewMemory(), testHealth))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.WatchAgents(ctx, false, nil)
	if !errors.Is(err, ErrConnection) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestWatchAgentsEOFAndHandlerError(t *testing.T) {
	// Server that sends one frame then closes.
	path := shortSocketPath(t)
	l, cleanup, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(apiv1.AgentList{SchemaVersion: 1, Agents: []apiv1.Agent{}})
	})}
	go srv.Serve(l)
	defer srv.Close()
	client := NewClient(path)
	defer client.Close()

	var n int
	err = client.WatchAgents(context.Background(), false, func(apiv1.AgentList) error { n++; return nil })
	if !errors.Is(err, ErrConnection) || err.Error() != "connect to noc server: agent watch stream closed" || n != 1 {
		t.Fatalf("err = %v n=%d", err, n)
	}
	sentinel := errors.New("stop")
	err = client.WatchAgents(context.Background(), false, func(apiv1.AgentList) error { return sentinel })
	if err != sentinel {
		t.Fatalf("err = %v", err)
	}
}

func TestWatchEndsOnListError(t *testing.T) {
	r := &flakyReader{Memory: registry.NewMemory()}
	h := NewHandler(r, testHealth)
	h.SetWatchInterval(5 * time.Millisecond)
	_, client := serve(t, h)
	err := client.WatchAgents(ctx5(t), false, nil)
	if !errors.Is(err, ErrConnection) {
		t.Fatalf("err = %v", err)
	}
}

type flakyReader struct {
	*registry.Memory
	calls atomic.Int32
}

func (f *flakyReader) List(ctx context.Context, o registry.ListOptions) ([]apiv1.Agent, error) {
	if f.calls.Add(1) > 1 {
		return nil, errors.New("boom")
	}
	return f.Memory.List(ctx, o)
}

func TestClientAndServerListAgentEvents(t *testing.T) {
	mem := &eventMemory{Memory: registry.NewMemory(), events: []apiv1.AgentEvent{{SchemaVersion: 1, ID: "e1", AgentID: "a1", Provider: apiv1.ProviderCodex}}}
	_ = mem.Upsert(context.Background(), newAgent("a1"))
	_, client := serve(t, NewHandler(mem, testHealth))
	ctx := ctx5(t)

	out, err := client.ListEvents(ctx, "a1", apiv1.ProviderCodex, 7)
	if err != nil || out.AgentID != "a1" || out.Provider != apiv1.ProviderCodex || len(out.Events) != 1 {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if mem.last.AgentID != "a1" || mem.last.Limit != 7 || mem.last.Provider != apiv1.ProviderCodex {
		t.Fatalf("opts = %+v", mem.last)
	}
	out, err = client.ListEvents(ctx, "", "", 0)
	if err != nil || out.AgentID != "" || out.Provider != "" || mem.last.Limit != registry.DefaultEventLimit {
		t.Fatalf("out = %+v, %v, opts=%+v", out, err, mem.last)
	}
	_, err = client.ListEvents(ctx, "missing", "", 0)
	if err == nil || err.Error() != "noc server: agent was not found" || errors.Is(err, ErrConnection) {
		t.Fatalf("err = %v", err)
	}
}

func TestForgetAgentRemovesTheRowOverTheSocket(t *testing.T) {
	mem := registry.NewMemory()
	_ = mem.Upsert(context.Background(), newAgent("a1"))
	_, client := serve(t, NewHandler(mem, testHealth))
	ctx := ctx5(t)
	if err := client.ForgetAgent(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Get(ctx, "a1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("row still present: %v", err)
	}
	if err := client.ForgetAgent(ctx, "a1"); err == nil || err.Error() != "noc server: agent was not found" {
		t.Fatalf("second forget err = %v", err)
	}
}

func TestErrorCatalogOverRawHTTP(t *testing.T) {
	mem := &eventMemory{Memory: registry.NewMemory()}
	_ = mem.Upsert(context.Background(), newAgent("a1"))
	var wakes atomic.Int32
	socket, _ := serve(t, NewHandler(mem, testHealth, func() { wakes.Add(1) }))

	long := strings.Repeat("x", 121)
	cases := []struct {
		method, path, body string
		status             int
		allow, code, msg   string
	}{
		{"POST", "/v1/agents", "", 405, "GET", "method_not_allowed", "method not allowed"},
		{"POST", "/v1/agents/a1", "", 405, "GET", "method_not_allowed", ""},
		{"HEAD", "/v1/health", "", 405, "GET", "", ""},
		{"GET", "/v1/agents/", "", 400, "", "invalid_agent_id", "an agent ID is required"},
		{"GET", "/v1/agents/a/b/c", "", 400, "", "invalid_agent_id", ""},
		{"GET", "/v1/agents/a/bogus", "", 404, "", "endpoint_not_found", "endpoint was not found"},
		{"GET", "/v1/agents/missing", "", 404, "", "agent_not_found", "agent was not found"},
		{"DELETE", "/v1/agents/missing", "", 404, "", "agent_not_found", ""},
		{"GET", "/v1/agents/a1/name", "", 405, "PUT", "method_not_allowed", ""},
		{"GET", "/v1/hooks/wake", "", 405, "POST", "method_not_allowed", ""},
		{"POST", "/v1/agents/watch", "", 405, "GET", "method_not_allowed", ""},
		{"POST", "/v1/events", "", 405, "GET", "method_not_allowed", ""},
		{"PUT", "/v1/agents/a1/name", `{"name":"x","extra":1}`, 400, "", "invalid_agent_name", "request body must contain a name"},
		{"PUT", "/v1/agents/a1/name", ``, 400, "", "invalid_agent_name", "request body must contain a name"},
		{"PUT", "/v1/agents/a1/name", `{"name":"` + long + `"}`, 400, "", "invalid_agent_name", "agent name must be at most 120 characters"},
		{"PUT", "/v1/agents/a1/name", `{"name":"` + strings.Repeat("x", 5000) + `"}`, 400, "", "invalid_agent_name", "request body must contain a name"},
		{"PUT", "/v1/agents/missing/name", `{}`, 404, "", "agent_not_found", ""},
		{"GET", "/v1/events?limit=0", "", 400, "", "invalid_limit", "limit must be between 1 and 200"},
		{"GET", "/v1/events?limit=201", "", 400, "", "invalid_limit", ""},
		{"GET", "/v1/events?limit=abc", "", 400, "", "invalid_limit", ""},
		{"GET", "/v1/events?provider=other", "", 400, "", "invalid_provider", "provider must be claude or codex"},
		{"GET", "/v1/agents/missing/events?provider=other", "", 404, "", "agent_not_found", ""},
	}
	for _, c := range cases {
		resp, body := rawDo(t, socket, c.method, c.path, c.body)
		if resp.StatusCode != c.status || resp.Header.Get("Allow") != c.allow {
			t.Errorf("%s %s: status=%d allow=%q", c.method, c.path, resp.StatusCode, resp.Header.Get("Allow"))
			continue
		}
		if c.method == "HEAD" {
			continue
		}
		var e apiv1.Error
		if err := json.Unmarshal([]byte(body), &e); err != nil || e.Code != c.code || e.SchemaVersion != 1 || (c.msg != "" && e.Message != c.msg) {
			t.Errorf("%s %s: body=%q", c.method, c.path, body)
		}
		if resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: content-type %q", c.method, c.path, resp.Header.Get("Content-Type"))
		}
	}

	// Exactly 120 runes (multi-byte) is accepted; {} and null clear.
	resp, _ := rawDo(t, socket, "PUT", "/v1/agents/a1/name", `{"name":"`+strings.Repeat("é", 120)+`"}`)
	if resp.StatusCode != 200 || resp.Header.Get(apiv1.ProviderNameSyncHeader) != "unsupported" {
		t.Errorf("120 runes: %d %v", resp.StatusCode, resp.Header)
	}
	resp, body := rawDo(t, socket, "PUT", "/v1/agents/a1/name", `null`)
	if resp.StatusCode != 200 || resp.Header.Get(apiv1.ProviderNameSyncHeader) != "not_attempted" || strings.Contains(body, "user_name") {
		t.Errorf("null: %d %v %s", resp.StatusCode, resp.Header, body)
	}

	// Plain-text mux 404.
	resp, body = rawDo(t, socket, "GET", "/v1/nope", "")
	if resp.StatusCode != 404 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || body != "404 page not found\n" {
		t.Errorf("nope: %d %v %q", resp.StatusCode, resp.Header, body)
	}

	// Wake returns 202 with empty body; trailing slash on watch routes to agent id.
	resp, body = rawDo(t, socket, "POST", "/v1/hooks/wake", "")
	if resp.StatusCode != 202 || body != "" || wakes.Load() != 1 {
		t.Errorf("wake: %d %q", resp.StatusCode, body)
	}
	resp, _ = rawDo(t, socket, "GET", "/v1/agents/watch/", "")
	if resp.StatusCode != 404 {
		t.Errorf("watch/: %d", resp.StatusCode)
	}
	// Forget success is 204 with no body.
	resp, body = rawDo(t, socket, "DELETE", "/v1/agents/a1", "")
	if resp.StatusCode != 204 || body != "" {
		t.Errorf("delete: %d %q", resp.StatusCode, body)
	}
}

func TestAllQueryIsExactlyTrue(t *testing.T) {
	mem := registry.NewMemory()
	a := newAgent("a1")
	a.LiveState = apiv1.LiveStateNotLive
	_ = mem.Upsert(context.Background(), a)
	socket, _ := serve(t, NewHandler(mem, testHealth))
	for query, want := range map[string]int{"": 0, "?all=1": 0, "?all=TRUE": 0, "?all=true": 1} {
		_, body := rawDo(t, socket, "GET", "/v1/agents"+query, "")
		var l apiv1.AgentList
		_ = json.Unmarshal([]byte(body), &l)
		if len(l.Agents) != want {
			t.Errorf("%q: %d agents", query, len(l.Agents))
		}
	}
}

func TestUnavailableCapabilities(t *testing.T) {
	socket, _ := serve(t, NewHandler(bareReader{m: registry.NewMemory()}, testHealth))
	cases := []struct {
		method, path, code string
		status             int
	}{
		{"DELETE", "/v1/agents/a1", "agent_forget_unavailable", 501},
		{"PUT", "/v1/agents/a1/name", "agent_naming_unavailable", 501},
		{"GET", "/v1/events", "events_unavailable", 501},
		{"GET", "/v1/agents/a1/events", "events_unavailable", 501},
		{"POST", "/v1/hooks/wake", "hook_ingestion_unavailable", 503},
	}
	for _, c := range cases {
		resp, body := rawDo(t, socket, c.method, c.path, `{"name":"x"}`)
		var e apiv1.Error
		_ = json.Unmarshal([]byte(body), &e)
		if resp.StatusCode != c.status || e.Code != c.code {
			t.Errorf("%s %s: %d %s", c.method, c.path, resp.StatusCode, body)
		}
	}
}

func TestClientConnectionErrors(t *testing.T) {
	client := NewClient(shortSocketPath(t))
	defer client.Close()
	ctx := ctx5(t)
	for name, err := range map[string]error{
		"health": func() error { _, e := client.Health(ctx); return e }(),
		"wake":   client.WakeHooks(ctx),
		"watch":  client.WatchAgents(ctx, false, nil),
	} {
		if !errors.Is(err, ErrConnection) || !strings.HasPrefix(err.Error(), "connect to noc server: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Non-watch errors are not unwrappable to the cause.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Health(canceled); errors.Is(err, context.Canceled) {
		t.Errorf("health cause should not unwrap: %v", err)
	}
}

func TestClientUnexpectedStatus(t *testing.T) {
	path := shortSocketPath(t)
	l, cleanup, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents" {
			_, _ = w.Write([]byte("not json"))
			return
		}
		http.NotFound(w, r)
	})}
	go srv.Serve(l)
	defer srv.Close()
	client := NewClient(path)
	defer client.Close()
	if _, err := client.GetAgent(ctx5(t), "x"); err == nil || err.Error() != "noc server: unexpected HTTP status 404 Not Found" {
		t.Errorf("err = %v", err)
	}
	if err := client.WakeHooks(ctx5(t)); err == nil || err.Error() != "noc server: unexpected HTTP status 404 Not Found" {
		t.Errorf("wake err = %v", err)
	}
	if _, err := client.ListAgents(ctx5(t), false); err == nil || !strings.HasPrefix(err.Error(), "decode noc server response: ") {
		t.Errorf("decode err = %v", err)
	}
}

func TestWatchHeaders(t *testing.T) {
	mem := registry.NewMemory()
	socket, _ := serve(t, NewHandler(mem, testHealth))
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "http://m/v1/agents/watch", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || resp.StatusCode != 200 ||
		resp.Header.Get("Content-Type") != "application/x-ndjson" ||
		resp.Header.Get("Cache-Control") != "no-cache" ||
		!strings.Contains(line, `"agents":[]`) {
		t.Fatalf("%d %v %q %v", resp.StatusCode, resp.Header, line, err)
	}
}

func TestFingerprintIgnoresFreshnessOnlyFields(t *testing.T) {
	now := time.Now()
	base := newAgent("a1")
	base.InitialRequest = &apiv1.TextPreview{Text: "t", ObservedAt: now}
	base.Workspace = &apiv1.Workspace{Path: "/p", ObservedAt: now}
	base.FrontendBindings = []apiv1.FrontendBinding{{Kind: "zellij", State: "s", ObservedAt: now}}
	want := watchFingerprint([]apiv1.Agent{base})

	later := now.Add(time.Hour)
	fresh := base
	fresh.LastSeenAt = later
	fresh.SnapshotConfirmedAt, fresh.ModelObservedAt, fresh.ReasoningObservedAt, fresh.SummaryUpdatedAt = &later, &later, &later, &later
	ir := *base.InitialRequest
	ir.ObservedAt = later
	fresh.InitialRequest = &ir
	ws := *base.Workspace
	ws.ObservedAt = later
	fresh.Workspace = &ws
	fresh.FrontendBindings = []apiv1.FrontendBinding{{Kind: "zellij", State: "s", ObservedAt: later}}
	if watchFingerprint([]apiv1.Agent{fresh}) != want {
		t.Fatal("freshness-only change altered fingerprint")
	}
	if base.FrontendBindings[0].ObservedAt != now {
		t.Fatal("input mutated")
	}
	sem := fresh
	sem.ProcessID = 9
	if watchFingerprint([]apiv1.Agent{sem}) == want {
		t.Fatal("semantic change ignored")
	}
}

func TestListenSocketMode(t *testing.T) {
	path := shortSocketPath(t)
	l, cleanup, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	_ = l.Close()
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remains: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
}

func TestListenRefusesNonSocketPath(t *testing.T) {
	path := shortSocketPath(t)
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Listen(path)
	if err == nil || err.Error() != `refusing to replace non-socket path "`+path+`"` {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "keep" {
		t.Fatal("file was modified")
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	path := shortSocketPath(t)
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Close without unlinking to leave an orphan socket file.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("setup: %v", err)
	}
	l, cleanup, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer l.Close()
}

func TestListenDetectsLiveServer(t *testing.T) {
	path, _ := serve(t, NewHandler(registry.NewMemory(), testHealth))
	_, _, err := Listen(path)
	if !errors.Is(err, ErrServerRunning) || err.Error() != "noc server is already running" {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("live socket was removed")
	}
}

func TestListenCreatesPrivateParent(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "noc-api-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	l, cleanup, err := Listen(base + "/run/noc.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer l.Close()
	if info, err := os.Stat(base + "/run"); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", info, err)
	}
}
