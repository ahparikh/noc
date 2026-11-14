package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	apiv1 "noc.local/noc/pkg/api/v1"
)

// ErrConnection marks failures to reach the noc server.
var ErrConnection = errors.New("connect to noc server")

// Client talks to the noc server over its Unix socket.
type Client struct {
	http *http.Client
}

// NewClient returns a client that dials socketPath for every connection.
// It sets no timeouts; deadlines come from the caller's context.
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: transport}}
}

// Close closes idle keep-alive connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// AgentNameResult is the rename response plus the provider sync headers.
type AgentNameResult struct {
	Agent             apiv1.Agent
	ProviderSync      apiv1.ProviderNameSync
	ProviderSyncError string
}

func (c *Client) Health(ctx context.Context) (apiv1.Health, error) {
	var out apiv1.Health
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/health", nil, &out)
	return out, err
}

func (c *Client) ListAgents(ctx context.Context, includeNotLive bool) (apiv1.AgentList, error) {
	var out apiv1.AgentList
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/agents"+allQuery(includeNotLive), nil, &out)
	return out, err
}

func (c *Client) GetAgent(ctx context.Context, id string) (apiv1.Agent, error) {
	var out apiv1.Agent
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) RenameAgent(ctx context.Context, id, name string) (AgentNameResult, error) {
	body, err := json.Marshal(apiv1.AgentNameUpdate{Name: name})
	if err != nil {
		return AgentNameResult{}, fmt.Errorf("encode agent name: %w", err)
	}
	var result AgentNameResult
	header, err := c.doJSON(ctx, http.MethodPut, "/v1/agents/"+url.PathEscape(id)+"/name", body, &result.Agent)
	if err != nil {
		return AgentNameResult{}, err
	}
	result.ProviderSync = apiv1.ProviderNameSync(header.Get(apiv1.ProviderNameSyncHeader))
	result.ProviderSyncError = header.Get(apiv1.ProviderNameSyncErrorHeader)
	return result, nil
}

func (c *Client) SetAgentName(ctx context.Context, id, name string) (apiv1.Agent, error) {
	result, err := c.RenameAgent(ctx, id, name)
	return result.Agent, err
}

func (c *Client) ForgetAgent(ctx context.Context, id string) error {
	_, err := c.doJSON(ctx, http.MethodDelete, "/v1/agents/"+url.PathEscape(id), nil, nil)
	return err
}

func (c *Client) ListEvents(ctx context.Context, agentID string, provider apiv1.Provider, limit int) (apiv1.AgentEventList, error) {
	path := "/v1/events"
	if agentID != "" {
		path = "/v1/agents/" + url.PathEscape(agentID) + "/events"
	}
	query := url.Values{}
	if provider != "" {
		query.Set("provider", string(provider))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out apiv1.AgentEventList
	_, err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// WakeHooks asks the server to drain the hook outbox.
func (c *Client) WakeHooks(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://noc/v1/hooks/wake", nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("noc server: unexpected HTTP status %s", resp.Status)
	}
	return nil
}

// WatchAgents streams semantic snapshots, calling handle once per frame.
func (c *Client) WatchAgents(ctx context.Context, includeNotLive bool, handle func(apiv1.AgentList) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://noc/v1/agents/watch"+allQuery(includeNotLive), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConnection, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	decoder := json.NewDecoder(resp.Body)
	for {
		var list apiv1.AgentList
		if err := decoder.Decode(&list); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: agent watch stream closed", ErrConnection)
			}
			return fmt.Errorf("decode agent watch stream: %w", err)
		}
		if handle == nil {
			continue
		}
		if err := handle(list); err != nil {
			return err
		}
	}
}

func allQuery(includeNotLive bool) string {
	if includeNotLive {
		return "?all=true"
	}
	return ""
}

// statusError maps a non-2xx response to the server's message or a generic one.
func statusError(resp *http.Response) error {
	var apiErr apiv1.Error
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err == nil && apiErr.Message != "" {
		return fmt.Errorf("noc server: %s", apiErr.Message)
	}
	return fmt.Errorf("noc server: unexpected HTTP status %s", resp.Status)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body []byte, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://noc"+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, fmt.Errorf("decode noc server response: %w", err)
		}
	}
	return resp.Header, nil
}
