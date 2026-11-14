package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"noc.local/noc/internal/provider"
)

type loggedRequest struct {
	Method string
	Params json.RawMessage
}

// fakeDaemon is a scriptable Codex app-server speaking WebSocket + JSON-RPC.
type fakeDaemon struct {
	mu            sync.Mutex
	requests      []loggedRequest
	rejections    []int // error codes of replies to server-initiated requests
	sawClose      bool
	loaded        []string
	threads       map[string]string // thread ID -> thread JSON
	serverRequest bool              // send a server request during initialize
	failMethod    string            // answer this method with an RPC error
	hang          bool              // never answer initialize
}

func (d *fakeDaemon) methods() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, r := range d.requests {
		out = append(out, r.Method)
	}
	return out
}

func (d *fakeDaemon) paramsOf(method string) []json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []json.RawMessage
	for _, r := range d.requests {
		if r.Method == method {
			out = append(out, r.Params)
		}
	}
	return out
}

func sendJSON(w io.Writer, v string) error {
	return writeWebSocketFrame(w, true, opText, []byte(v), false)
}

// serve handles one connection until the client closes it.
func (d *fakeDaemon) serve(rw io.ReadWriter) error {
	br := bufio.NewReader(rw)
	if _, err := serveUpgrade(rw, br, okUpgrade); err != nil {
		return err
	}
	for {
		frame, err := readWebSocketFrame(br, true)
		if err != nil {
			return nil // abrupt EOF is expected: the client sends no Close frame
		}
		if frame.opcode == opClose {
			d.mu.Lock()
			d.sawClose = true
			d.mu.Unlock()
			return nil
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(frame.payload, &msg); err != nil {
			return err
		}
		if msg.Method == "" {
			continue
		}
		d.mu.Lock()
		d.requests = append(d.requests, loggedRequest{msg.Method, msg.Params})
		d.mu.Unlock()
		if msg.ID == nil {
			continue
		}
		id := string(msg.ID)
		if d.failMethod == msg.Method {
			if err := sendJSON(rw, fmt.Sprintf(`{"id":%s,"error":{"code":-32000,"message":"denied"}}`, id)); err != nil {
				return err
			}
			continue
		}
		switch msg.Method {
		case "initialize":
			if d.hang {
				continue
			}
			if d.serverRequest {
				if err := sendJSON(rw, `{"id":"s-1","method":"account/chatgptAuthTokens/refresh"}`); err != nil {
					return err
				}
				reply, err := readWebSocketFrame(br, true)
				if err != nil {
					return err
				}
				var r struct {
					Error struct{ Code int }
				}
				_ = json.Unmarshal(reply.payload, &r)
				d.mu.Lock()
				d.rejections = append(d.rejections, r.Error.Code)
				d.mu.Unlock()
			}
			err = sendJSON(rw, fmt.Sprintf(`{"id":%s,"result":{"userAgent":"fake"}}`, id))
		case "thread/loaded/list":
			data, _ := json.Marshal(d.loaded)
			err = sendJSON(rw, fmt.Sprintf(`{"id":%s,"result":{"data":%s,"nextCursor":null}}`, id, data))
		case "thread/read":
			var p struct{ ThreadID string }
			_ = json.Unmarshal(msg.Params, &p)
			if thread, ok := d.threads[p.ThreadID]; ok {
				err = sendJSON(rw, fmt.Sprintf(`{"id":%s,"result":{"thread":%s}}`, id, thread))
			} else {
				err = sendJSON(rw, fmt.Sprintf(`{"id":%s,"error":{"code":-32602,"message":"thread is no longer loaded"}}`, id))
			}
		case "thread/name/set":
			err = sendJSON(rw, fmt.Sprintf(`{"id":%s,"result":{}}`, id))
		default:
			err = sendJSON(rw, fmt.Sprintf(`{"id":%s,"error":{"code":-32601,"message":"unknown method"}}`, id))
		}
		if err != nil {
			return err
		}
	}
}

func standardDaemon() *fakeDaemon {
	return &fakeDaemon{
		loaded: []string{"t1", "gone", "t2", "t1"},
		threads: map[string]string{
			"t1": `{"id":"t1","name":"One","cwd":"/w/one","source":"cli","status":{"type":"idle"},"path":"/private/x"}`,
			"t2": `{"id":"t2","name":null,"cwd":"/w/two","source":"exec","status":{"type":"idle"}}`,
		},
	}
}

// listenUnix opens a private Unix socket under a short /tmp path (macOS limits
// socket paths to about 104 bytes) and serves each connection with handle.
func listenUnix(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "noc-codex-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				handle(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		wg.Wait()
		os.RemoveAll(dir)
	})
	return path
}

func serveDaemon(t *testing.T, d *fakeDaemon) string {
	return listenUnix(t, func(c net.Conn) { _ = d.serve(c) })
}

type recordingFallback struct {
	agents  []provider.NativeAgent
	err     error
	nameErr error
	calls   int
	named   []string
}

func (f *recordingFallback) DiscoverLive(context.Context) ([]provider.NativeAgent, error) {
	f.calls++
	return f.agents, f.err
}

type namingFallback struct{ recordingFallback }

func (f *namingFallback) SetThreadName(_ context.Context, id, name string) error {
	f.named = append(f.named, id+"="+name)
	return f.nameErr
}

func missingSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "noc-codex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "none.sock")
}

func TestSocketDiscoveryClientUsesDirectUnixSocket(t *testing.T) {
	d := standardDaemon()
	d.serverRequest = true
	path := serveDaemon(t, d)
	fallback := &recordingFallback{}
	client := newSocketClient(path, 5*time.Second, fallback)

	agents, err := client.DiscoverLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].NativeSessionID != "t1" || agents[0].Name != "One" || agents[0].CWD != "/w/one" {
		t.Fatalf("agents = %+v", agents)
	}
	if strings.Contains(string(agents[0].Raw), "/private/") {
		t.Fatalf("raw leaks path: %s", agents[0].Raw)
	}
	want := "initialize,initialized,thread/loaded/list,thread/read,thread/read,thread/read"
	// t1 and gone and t2 are read once each; the duplicate t1 is skipped.
	if got := strings.Join(d.methods(), ","); got != want {
		t.Fatalf("methods = %s", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.rejections) != 1 || d.rejections[0] != -32601 {
		t.Fatalf("rejections = %v", d.rejections)
	}
	if d.sawClose {
		t.Fatal("client must not send a WebSocket Close frame")
	}
	if fallback.calls != 0 {
		t.Fatal("fallback used although the socket was reachable")
	}
}

func TestSocketDiscoveryClientFallsBackOnlyWhenDialFails(t *testing.T) {
	rows := []provider.NativeAgent{{NativeSessionID: "fb"}}
	fallback := &recordingFallback{agents: rows}
	client := newSocketClient(missingSocket(t), time.Second, fallback)
	got, err := client.DiscoverLive(context.Background())
	if err != nil || len(got) != 1 || got[0].NativeSessionID != "fb" || fallback.calls != 1 {
		t.Fatalf("got %v, %v (calls %d)", got, err, fallback.calls)
	}

	// An empty path is also "not connected".
	fallback = &recordingFallback{agents: rows}
	got, err = newSocketClient("  ", time.Second, fallback).DiscoverLive(context.Background())
	if err != nil || len(got) != 1 || fallback.calls != 1 {
		t.Fatalf("empty path: %v, %v", got, err)
	}
	_, err = newSocketClient("", time.Second, nil).DiscoverLive(context.Background())
	if err == nil || err.Error() != "Codex app-server socket path is unavailable" {
		t.Fatalf("err = %v", err)
	}

	// An explicit socket has no fallback: the dial error is returned as is.
	path := missingSocket(t)
	_, err = newSocketClient(path, time.Second, nil).DiscoverLive(context.Background())
	want := fmt.Sprintf("dial Codex app-server socket %q: dial unix %s: connect: no such file or directory", path, path)
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
}

func TestSocketDiscoveryClientDoesNotHideProtocolFailureWithFallback(t *testing.T) {
	notWebSocket := listenUnix(t, func(c net.Conn) {
		_, _ = serveUpgrade(c, bufio.NewReader(c), func(string) string {
			return "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
		})
	})
	fallback := &recordingFallback{agents: []provider.NativeAgent{{NativeSessionID: "fb"}}}
	_, err := newSocketClient(notWebSocket, time.Second, fallback).DiscoverLive(context.Background())
	if err == nil || fallback.calls != 0 {
		t.Fatalf("err = %v, fallback calls %d", err, fallback.calls)
	}
	want := fmt.Sprintf("open Codex app-server WebSocket on %q: WebSocket upgrade returned 200 OK", notWebSocket)
	if err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}

	// An RPC failure on an open connection is not hidden either.
	d := standardDaemon()
	d.failMethod = "thread/loaded/list"
	path := serveDaemon(t, d)
	_, err = newSocketClient(path, time.Second, fallback).DiscoverLive(context.Background())
	want = fmt.Sprintf("discover through Codex app-server socket %q: list loaded Codex threads: Codex app-server error -32000: denied", path)
	if err == nil || err.Error() != want || fallback.calls != 0 {
		t.Fatalf("err = %v\nwant %s (fallback calls %d)", err, want, fallback.calls)
	}
}

func TestSocketDiscoveryClientReportsBothConnectionFailures(t *testing.T) {
	path := missingSocket(t)
	fallback := &recordingFallback{err: errors.New("proxy exploded: stderr text")}
	_, err := newSocketClient(path, time.Second, fallback).DiscoverLive(context.Background())
	prefix := fmt.Sprintf("dial Codex app-server socket %q: dial unix %s: ", path, path)
	if err == nil || !strings.HasPrefix(err.Error(), prefix) ||
		!strings.HasSuffix(err.Error(), "; Codex proxy fallback failed: proxy exploded: stderr text") {
		t.Fatalf("err = %v", err)
	}
}

func TestSocketDiscoveryClientSnapshotDeadline(t *testing.T) {
	d := standardDaemon()
	d.hang = true
	path := serveDaemon(t, d)
	start := time.Now()
	_, err := newSocketClient(path, 150*time.Millisecond, nil).DiscoverLive(context.Background())
	if err == nil || !strings.HasSuffix(err.Error(), "context deadline exceeded") ||
		!strings.Contains(err.Error(), "discover through Codex app-server socket") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("deadline not enforced: %v", time.Since(start))
	}
}

func TestSocketClientRenamesThreadThroughAppServer(t *testing.T) {
	d := standardDaemon()
	path := serveDaemon(t, d)
	client := newSocketClient(path, 5*time.Second, nil)
	if err := client.SetThreadName(context.Background(), "t1", "New name"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.methods(), ","); got != "initialize,initialized,thread/name/set" {
		t.Fatalf("methods = %s", got)
	}
	if got := string(d.paramsOf("thread/name/set")[0]); got != `{"threadId":"t1","name":"New name"}` {
		t.Fatalf("params = %s", got)
	}

	// Errors on a connected socket come back without discovery wrapping.
	d2 := standardDaemon()
	d2.failMethod = "thread/name/set"
	path2 := serveDaemon(t, d2)
	fallback := &namingFallback{}
	err := newSocketClient(path2, 5*time.Second, fallback).SetThreadName(context.Background(), "t1", "x")
	if err == nil || err.Error() != "set Codex thread name: Codex app-server error -32000: denied" || len(fallback.named) != 0 {
		t.Fatalf("err = %v", err)
	}

	// Not connected: the proxy fallback renames when it can.
	missing := missingSocket(t)
	fallback = &namingFallback{}
	if err := newSocketClient(missing, time.Second, fallback).SetThreadName(context.Background(), "t1", "n"); err != nil ||
		len(fallback.named) != 1 || fallback.named[0] != "t1=n" {
		t.Fatalf("err = %v, named %v", err, fallback.named)
	}
	fallback = &namingFallback{recordingFallback{nameErr: errors.New("nope")}}
	err = newSocketClient(missing, time.Second, fallback).SetThreadName(context.Background(), "t1", "n")
	if err == nil || !strings.HasPrefix(err.Error(), "dial Codex app-server socket") ||
		!strings.HasSuffix(err.Error(), "; Codex proxy fallback failed: nope") {
		t.Fatalf("err = %v", err)
	}
	// A discovery-only fallback or no fallback returns the dial error.
	for _, fb := range []DiscoveryClient{nil, &recordingFallback{}} {
		err = newSocketClient(missing, time.Second, fb).SetThreadName(context.Background(), "t1", "n")
		if err == nil || !strings.HasPrefix(err.Error(), "dial Codex app-server socket") || strings.Contains(err.Error(), "fallback") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestAdapterWithoutRenamingClientReportsNotImplemented(t *testing.T) {
	a := NewWithOptions(Options{Client: &fakeClient{}})
	err := a.SetName(context.Background(), "t1", "name")
	if err != provider.ErrNotImplemented || !errors.Is(err, provider.ErrNotImplemented) {
		t.Fatalf("err = %v", err)
	}

	d := standardDaemon()
	path := serveDaemon(t, d)
	a = NewWithOptions(Options{SocketPath: path})
	if err := a.SetName(context.Background(), "t1", "name"); err != nil {
		t.Fatal(err)
	}
	d2 := standardDaemon()
	d2.failMethod = "thread/name/set"
	a = NewWithOptions(Options{SocketPath: serveDaemon(t, d2)})
	err = a.SetName(context.Background(), "t1", "name")
	if err == nil || err.Error() != "rename Codex thread: set Codex thread name: Codex app-server error -32000: denied" {
		t.Fatalf("err = %v", err)
	}
}

// TestInstalledSharedDaemonDiscovery performs one read-only snapshot against a
// real Codex install. Opt in with NOC_CODEX_INTEGRATION=1; set NOC_CODEX_SOCKET
// to forbid the `codex app-server proxy` fallback.
func TestInstalledSharedDaemonDiscovery(t *testing.T) {
	if os.Getenv("NOC_CODEX_INTEGRATION") != "1" {
		t.Skip("set NOC_CODEX_INTEGRATION=1 to query the installed Codex daemon")
	}
	a := New()
	if _, ok := a.client.(*socketClient); !ok {
		t.Fatalf("client = %T, want direct socket client", a.client)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	probe := a.Probe(ctx)
	if !probe.Available {
		t.Fatalf("probe unavailable: %s", probe.Diagnostic)
	}
	agents, err := a.DiscoverLive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("codex %s: %d loaded top-level threads", probe.Version, len(agents))
}
