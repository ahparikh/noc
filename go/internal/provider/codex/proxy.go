package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"noc.local/noc/internal/buildinfo"
	"noc.local/noc/internal/provider"
)

const (
	proxyStderrLimit = 64 << 10
	proxyExitGrace   = time.Second
	loadedListLimit  = 1000

	codeMethodNotFound = -32601
)

// --- JSON-RPC layer -------------------------------------------------------

// rpcCallError is a JSON-RPC error response to a call.
type rpcCallError struct {
	Code    int
	Message string
}

func (e *rpcCallError) Error() string {
	return fmt.Sprintf("Codex app-server error %d: %s", e.Code, e.Message)
}

type rpcRequest struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcNotification struct {
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcErrorResponse struct {
	ID    json.RawMessage `json:"id"`
	Error rpcErrorBody    `json:"error"`
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// rpcConnection issues strictly sequential JSON-RPC calls (one outstanding)
// over a message transport. Request IDs are integers starting at 1.
type rpcConnection struct {
	transport messageTransport
	nextID    int
}

func newRPCConnection(transport messageTransport) *rpcConnection {
	return &rpcConnection{transport: transport}
}

func (c *rpcConnection) write(ctx context.Context, message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode Codex app-server message: %w", err)
	}
	if err := c.transport.WriteMessage(ctx, data); err != nil {
		return fmt.Errorf("flush Codex app-server message: %w", err)
	}
	return nil
}

func (c *rpcConnection) notify(ctx context.Context, method string, params any) error {
	return c.write(ctx, rpcNotification{Method: method, Params: params})
}

// call sends a request and reads until the matching response, rejecting any
// server-initiated request along the way.
func (c *rpcConnection) call(ctx context.Context, method string, params, result any) error {
	c.nextID++
	id := c.nextID
	if err := c.write(ctx, rpcRequest{ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	for {
		message, err := c.transport.ReadMessage(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("read Codex app-server message: %w", err)
		}
		var env rpcEnvelope
		if err := json.Unmarshal(message, &env); err != nil {
			return fmt.Errorf("decode Codex app-server message: %w", err)
		}
		if env.Method != "" && env.ID != nil {
			// Reply so the daemon does not wait on noc.
			if err := c.rejectServerRequest(ctx, env.ID); err != nil {
				return err
			}
			continue
		}
		if !matchesRPCID(env.ID, id) {
			continue
		}
		if len(env.Error) > 0 && !bytes.Equal(bytes.TrimSpace(env.Error), []byte("null")) {
			var body rpcErrorBody
			if err := json.Unmarshal(env.Error, &body); err != nil {
				return fmt.Errorf("decode %s error: %w", method, err)
			}
			return &rpcCallError{Code: body.Code, Message: body.Message}
		}
		if result != nil && len(env.Result) > 0 && !bytes.Equal(bytes.TrimSpace(env.Result), []byte("null")) {
			if err := json.Unmarshal(env.Result, result); err != nil {
				return fmt.Errorf("decode %s response: %w", method, err)
			}
		}
		return nil
	}
}

func (c *rpcConnection) rejectServerRequest(ctx context.Context, id json.RawMessage) error {
	return c.write(ctx, rpcErrorResponse{
		ID: id,
		Error: rpcErrorBody{
			Code:    codeMethodNotFound,
			Message: "noc does not implement server-initiated requests",
		},
	})
}

// matchesRPCID reports whether raw is exactly the integer want. Strings, null,
// and non-integral numbers (1.0) do not match.
func matchesRPCID(raw json.RawMessage, want int) bool {
	if len(raw) == 0 {
		return false
	}
	var got int
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	return got == want
}

// jsonLineMessageTransport carries one JSON message per line. Production code
// does not use it; protocol tests replay testdata/discovery.jsonl through it.
type jsonLineMessageTransport struct {
	r *bufio.Reader
	w io.Writer
}

func newJSONLineMessageTransport(r io.Reader, w io.Writer) *jsonLineMessageTransport {
	return &jsonLineMessageTransport{r: bufio.NewReader(r), w: w}
}

func (t *jsonLineMessageTransport) ReadMessage(ctx context.Context) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := t.r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			return trimmed, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (t *jsonLineMessageTransport) WriteMessage(_ context.Context, message []byte) error {
	_, err := t.w.Write(append(bytes.Clone(message), '\n'))
	return err
}

// --- Protocol exchanges ---------------------------------------------------

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type clientCapabilities struct {
	ExperimentalAPI           bool     `json:"experimentalApi"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods"`
}

type initializeParams struct {
	ClientInfo   clientInfo         `json:"clientInfo"`
	Capabilities clientCapabilities `json:"capabilities"`
}

func initializeConnection(ctx context.Context, conn *rpcConnection) error {
	params := initializeParams{
		ClientInfo: clientInfo{Name: "noc", Title: "noc agent registry", Version: buildinfo.Version},
		Capabilities: clientCapabilities{
			ExperimentalAPI: false,
			OptOutNotificationMethods: []string{
				"item/agentMessage/delta",
				"item/commandExecution/outputDelta",
				"item/reasoning/summaryTextDelta",
				"item/reasoning/textDelta",
			},
		},
	}
	if err := conn.call(ctx, "initialize", params, nil); err != nil {
		return fmt.Errorf("initialize Codex app-server connection: %w", err)
	}
	if err := conn.notify(ctx, "initialized", struct{}{}); err != nil {
		return fmt.Errorf("acknowledge Codex app-server initialization: %w", err)
	}
	return nil
}

type setThreadNameParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

func setThreadNameOnConnection(ctx context.Context, conn *rpcConnection, threadID, name string) error {
	if err := initializeConnection(ctx, conn); err != nil {
		return err
	}
	if err := conn.call(ctx, "thread/name/set", setThreadNameParams{ThreadID: threadID, Name: name}, nil); err != nil {
		return fmt.Errorf("set Codex thread name: %w", err)
	}
	return nil
}

type loadedListParams struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor,omitempty"`
}

type loadedListResult struct {
	Data       []string `json:"data"`
	NextCursor *string  `json:"nextCursor"`
}

type threadReadParams struct {
	ThreadID     string `json:"threadId"`
	IncludeTurns bool   `json:"includeTurns"`
}

// threadRecord holds the only thread keys the adapter decodes.
type threadRecord struct {
	ID     string          `json:"id"`
	Name   *string         `json:"name"`
	CWD    string          `json:"cwd"`
	Source json.RawMessage `json:"source"`
	Status struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags"`
	} `json:"status"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
	Preview         string `json:"preview"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

type threadReadResult struct {
	Thread threadRecord `json:"thread"`
}

// discoverFromConnection takes one loaded-thread snapshot. It only reads: no
// thread/list, start, resume, subscribe, or mutating method.
func discoverFromConnection(ctx context.Context, conn *rpcConnection) ([]provider.NativeAgent, error) {
	if err := initializeConnection(ctx, conn); err != nil {
		return nil, err
	}
	ids, err := listLoadedThreads(ctx, conn)
	if err != nil {
		return nil, err
	}
	var agents []provider.NativeAgent
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		var res threadReadResult
		err := conn.call(ctx, "thread/read", threadReadParams{ThreadID: id, IncludeTurns: false}, &res)
		if err != nil {
			// A thread unloading between list and read does not invalidate
			// the rest of the authoritative loaded snapshot.
			if isThreadNoLongerLoadedError(err) {
				continue
			}
			return nil, fmt.Errorf("read loaded Codex thread metadata: %w", err)
		}
		agent, ok, err := nativeAgentFromThread(res.Thread)
		if err != nil {
			return nil, err
		}
		if ok {
			agents = append(agents, agent)
		}
	}
	return agents, nil
}

func listLoadedThreads(ctx context.Context, conn *rpcConnection) ([]string, error) {
	var ids []string
	cursors := map[string]bool{}
	cursor := ""
	for {
		var res loadedListResult
		params := loadedListParams{Limit: loadedListLimit, Cursor: cursor}
		if err := conn.call(ctx, "thread/loaded/list", params, &res); err != nil {
			return nil, fmt.Errorf("list loaded Codex threads: %w", err)
		}
		ids = append(ids, res.Data...)
		if res.NextCursor == nil || *res.NextCursor == "" {
			return ids, nil
		}
		cursor = *res.NextCursor
		if cursors[cursor] {
			return nil, errors.New("Codex repeated a loaded-thread pagination cursor")
		}
		cursors[cursor] = true
	}
}

func isThreadNoLongerLoadedError(err error) bool {
	var rpcErr *rpcCallError
	if !errors.As(err, &rpcErr) {
		return false
	}
	msg := strings.ToLower(rpcErr.Message)
	return strings.Contains(msg, "not loaded") ||
		strings.Contains(msg, "no longer loaded") ||
		strings.Contains(msg, "thread not found")
}

// nativeAgentFromThread sanitizes one thread. ok is false when the thread is
// silently dropped: empty ID, notLoaded, or not a top-level source.
func nativeAgentFromThread(t threadRecord) (agent provider.NativeAgent, ok bool, err error) {
	id := strings.TrimSpace(t.ID)
	if id == "" || t.Status.Type == statusNotLoaded {
		return provider.NativeAgent{}, false, nil
	}
	source, accepted := topLevelSourceKind(t.Source)
	if !accepted {
		return provider.NativeAgent{}, false, nil
	}
	name := ""
	if t.Name != nil {
		name = strings.TrimSpace(*t.Name)
	}
	model, _ := boundedText(t.Model, maxModelRunes)
	effort, _ := boundedText(t.ReasoningEffort, maxReasoningRunes)
	preview, previewTruncated := boundedText(t.Preview, maxPreviewRunes)

	raw, err := json.Marshal(evidence{
		Status:           t.Status.Type,
		ActiveFlags:      slices.Clone(t.Status.ActiveFlags),
		CreatedAt:        t.CreatedAt,
		UpdatedAt:        t.UpdatedAt,
		Model:            model,
		ReasoningEffort:  effort,
		Preview:          preview,
		PreviewTruncated: previewTruncated,
	})
	if err != nil {
		return provider.NativeAgent{}, false, fmt.Errorf("encode sanitized Codex state: %w", err)
	}
	return provider.NativeAgent{
		NativeSessionID: id,
		Name:            name,
		SourceKind:      source,
		CWD:             strings.TrimSpace(t.CWD),
		Raw:             raw,
	}, true, nil
}

// topLevelSourceKind accepts only a JSON string equal to an accepted source.
// Structured sources (sub-agents, custom) and other strings (exec) are not
// top-level conversations.
func topLevelSourceKind(raw json.RawMessage) (string, bool) {
	var source string
	if len(raw) == 0 || json.Unmarshal(raw, &source) != nil {
		return "", false
	}
	if !slices.Contains(acceptedSources, source) {
		return "", false
	}
	return source, true
}

// --- Proxy fallback client ------------------------------------------------

// proxyClient speaks the same WebSocket stream through a short-lived
// `codex app-server proxy` process, one process per operation.
type proxyClient struct {
	executable string
	timeout    time.Duration
}

var (
	_ DiscoveryClient = (*proxyClient)(nil)
	_ ThreadNamer     = (*proxyClient)(nil)
)

func newProxyClient(executable string, timeout time.Duration) *proxyClient {
	return &proxyClient{executable: executable, timeout: timeout}
}

func (c *proxyClient) DiscoverLive(ctx context.Context) ([]provider.NativeAgent, error) {
	var agents []provider.NativeAgent
	err := c.withConnection(ctx, func(ctx context.Context, conn *rpcConnection) error {
		var err error
		agents, err = discoverFromConnection(ctx, conn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return agents, nil
}

func (c *proxyClient) SetThreadName(ctx context.Context, threadID, name string) error {
	return c.withConnection(ctx, func(ctx context.Context, conn *rpcConnection) error {
		return setThreadNameOnConnection(ctx, conn, threadID, name)
	})
}

type pipeReadWriter struct {
	io.Reader
	io.Writer
}

func (c *proxyClient) withConnection(ctx context.Context, fn func(context.Context, *rpcConnection) error) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.executable, "app-server", "proxy")
	stderr := &boundedBuffer{limit: proxyStderrLimit}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Codex proxy stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Codex proxy stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Codex app-server proxy: %w", err)
	}

	var opErr error
	transport, err := newWebSocketMessageTransport(ctx, pipeReadWriter{stdout, stdin})
	if err != nil {
		opErr = fmt.Errorf("open Codex app-server WebSocket: %w", err)
	} else {
		opErr = fn(ctx, newRPCConnection(transport))
	}
	_ = stdin.Close()
	waitErr := waitForProxy(cmd)
	return proxyError(opErr, waitErr, stderr)
}

// waitForProxy waits up to proxyExitGrace for the process to exit after stdin
// closes, then kills it. The RPC result is already in hand by then, so a
// forced exit is not an error.
func waitForProxy(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(proxyExitGrace):
		_ = cmd.Process.Kill()
		<-done
		return nil
	}
}

func proxyError(opErr, waitErr error, stderr *boundedBuffer) error {
	suffix := ""
	if detail := strings.TrimSpace(stderr.String()); detail != "" {
		suffix = ": " + detail
	}
	if opErr != nil {
		return fmt.Errorf("%w%s", opErr, suffix)
	}
	if waitErr != nil {
		return fmt.Errorf("Codex app-server proxy exited: %w%s", waitErr, suffix)
	}
	return nil
}

// boundedBuffer keeps the first limit bytes written and notes truncation.
type boundedBuffer struct {
	mu        sync.Mutex
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.limit - b.buf.Len()
	if room < len(p) {
		b.truncated = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buf.String() + " (truncated)"
	}
	return b.buf.String()
}
