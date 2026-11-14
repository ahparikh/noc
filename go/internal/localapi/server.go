package localapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"noc.local/noc/internal/provider"
	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	defaultWatchInterval  = 500 * time.Millisecond
	maxNameBodyBytes      = 4096
	maxNameRunes          = 120
	providerRenameTimeout = 5 * time.Second
	maxSyncErrorRunes     = 300
)

// HealthFunc computes the health document for each request.
type HealthFunc func() apiv1.Health

// HookWakeFunc nudges the hook drainer.
type HookWakeFunc func()

// Handler serves the noc API.
type Handler struct {
	reader        registry.Reader
	namer         registry.Namer
	forgetter     registry.Forgetter
	events        registry.EventReader
	health        HealthFunc
	wake          HookWakeFunc
	renamers      map[apiv1.Provider]provider.Renamer
	watchInterval time.Duration
	mux           *http.ServeMux
}

// NewHandler builds the handler. Optional capabilities are discovered by
// interface assertion on reader; only the first wake function is used.
func NewHandler(reader registry.Reader, health HealthFunc, wake ...HookWakeFunc) *Handler {
	h := &Handler{reader: reader, health: health, watchInterval: defaultWatchInterval}
	h.namer, _ = reader.(registry.Namer)
	h.forgetter, _ = reader.(registry.Forgetter)
	h.events, _ = reader.(registry.EventReader)
	if len(wake) > 0 {
		h.wake = wake[0]
	}
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("/v1/health", h.handleHealth)
	h.mux.HandleFunc("/v1/agents", h.handleAgents)
	h.mux.HandleFunc("/v1/agents/watch", h.handleAgentWatch)
	h.mux.HandleFunc("/v1/agents/", h.handleAgent)
	h.mux.HandleFunc("/v1/events", h.handleEvents)
	h.mux.HandleFunc("/v1/hooks/wake", h.handleHookWake)
	return h
}

// SetRenamers sets the per-provider name push adapters.
func (h *Handler) SetRenamers(r map[apiv1.Provider]provider.Renamer) { h.renamers = r }

// SetWatchInterval overrides the watch poll interval; d <= 0 restores the default.
func (h *Handler) SetWatchInterval(d time.Duration) {
	if d <= 0 {
		d = defaultWatchInterval
	}
	h.watchInterval = d
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiv1.Error{SchemaVersion: apiv1.SchemaVersion, Code: code, Message: message})
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	return false
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, h.health())
}

func (h *Handler) handleHookWake(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h.wake == nil {
		writeError(w, http.StatusServiceUnavailable, "hook_ingestion_unavailable", "hook ingestion is unavailable")
		return
	}
	h.wake()
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) list(ctx context.Context, includeNotLive bool) (apiv1.AgentList, error) {
	agents, err := h.reader.List(ctx, registry.ListOptions{IncludeNotLive: includeNotLive})
	if err != nil {
		return apiv1.AgentList{}, err
	}
	if agents == nil {
		agents = []apiv1.Agent{}
	}
	return apiv1.AgentList{SchemaVersion: apiv1.SchemaVersion, GeneratedAt: time.Now().UTC(), Agents: agents}, nil
}

func (h *Handler) handleAgents(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	list, err := h.list(r.Context(), r.URL.Query().Get("all") == "true")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "registry_error", "could not list agents")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) handleAgentWatch(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unavailable", "response streaming is unavailable")
		return
	}
	includeNotLive := r.URL.Query().Get("all") == "true"
	list, err := h.list(r.Context(), includeNotLive)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "registry_error", "could not list agents")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(list); err != nil {
		return
	}
	flusher.Flush()
	previous := watchFingerprint(list.Agents)

	ticker := time.NewTicker(h.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		list, err := h.list(r.Context(), includeNotLive)
		if err != nil {
			return
		}
		current := watchFingerprint(list.Agents)
		if current == previous {
			continue
		}
		if err := encoder.Encode(list); err != nil {
			return
		}
		flusher.Flush()
		previous = current
	}
}

// watchFingerprint hashes the agent slice with freshness-only fields zeroed.
func watchFingerprint(agents []apiv1.Agent) [sha256.Size]byte {
	projected := make([]apiv1.Agent, len(agents))
	for i, agent := range agents {
		agent.LastSeenAt = time.Time{}
		agent.SnapshotConfirmedAt = nil
		agent.ModelObservedAt = nil
		agent.ReasoningObservedAt = nil
		agent.SummaryUpdatedAt = nil
		if agent.InitialRequest != nil {
			preview := *agent.InitialRequest
			preview.ObservedAt = time.Time{}
			agent.InitialRequest = &preview
		}
		if agent.Workspace != nil {
			workspace := *agent.Workspace
			workspace.ObservedAt = time.Time{}
			agent.Workspace = &workspace
		}
		if agent.FrontendBindings != nil {
			bindings := make([]apiv1.FrontendBinding, len(agent.FrontendBindings))
			copy(bindings, agent.FrontendBindings)
			for j := range bindings {
				bindings[j].ObservedAt = time.Time{}
			}
			agent.FrontendBindings = bindings
		}
		projected[i] = agent
	}
	data, _ := json.Marshal(projected)
	return sha256.Sum256(data)
}

func (h *Handler) handleAgent(w http.ResponseWriter, r *http.Request) {
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/agents/"), "/")
	parts := strings.Split(suffix, "/")
	if suffix == "" || len(parts) > 2 {
		writeError(w, http.StatusBadRequest, "invalid_agent_id", "an agent ID is required")
		return
	}
	id := parts[0]
	if len(parts) == 2 {
		switch parts[1] {
		case "events":
			h.handleAgentEvents(w, r, id)
		case "name":
			h.handleAgentName(w, r, id)
		default:
			writeError(w, http.StatusNotFound, "endpoint_not_found", "endpoint was not found")
		}
		return
	}
	if r.Method == http.MethodDelete {
		h.handleForget(w, r, id)
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	agent, err := h.reader.Get(r.Context(), id)
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func writeReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "agent_not_found", "agent was not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "registry_error", "could not read agent")
}

func (h *Handler) handleForget(w http.ResponseWriter, r *http.Request, id string) {
	if h.forgetter == nil {
		writeError(w, http.StatusNotImplemented, "agent_forget_unavailable", "forgetting agents is unavailable")
		return
	}
	if err := h.forgetter.Forget(r.Context(), id); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "agent_not_found", "agent was not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "registry_error", "could not forget agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleAgentName(w http.ResponseWriter, r *http.Request, id string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	if h.namer == nil {
		writeError(w, http.StatusNotImplemented, "agent_naming_unavailable", "agent naming is unavailable")
		return
	}
	var update apiv1.AgentNameUpdate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxNameBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_agent_name", "request body must contain a name")
		return
	}
	name := strings.TrimSpace(update.Name)
	if utf8.RuneCountInString(name) > maxNameRunes {
		writeError(w, http.StatusBadRequest, "invalid_agent_name", "agent name must be at most 120 characters")
		return
	}
	agent, err := h.namer.SetUserName(r.Context(), id, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "agent_not_found", "agent was not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "registry_error", "could not name agent")
		return
	}
	sync, syncErr := h.pushNameToProvider(r.Context(), agent, name)
	w.Header().Set(apiv1.ProviderNameSyncHeader, string(sync))
	if sync == apiv1.ProviderNameFailed {
		w.Header().Set(apiv1.ProviderNameSyncErrorHeader, boundedSyncError(syncErr))
	}
	writeJSON(w, http.StatusOK, agent)
}

func (h *Handler) pushNameToProvider(ctx context.Context, agent apiv1.Agent, name string) (apiv1.ProviderNameSync, error) {
	if name == "" {
		return apiv1.ProviderNameNotAttempted, nil
	}
	renamer, ok := h.renamers[agent.Provider]
	if !ok || renamer == nil {
		return apiv1.ProviderNameUnsupported, nil
	}
	ctx, cancel := context.WithTimeout(ctx, providerRenameTimeout)
	defer cancel()
	if err := renamer.SetName(ctx, agent.NativeSessionID, name); err != nil {
		if errors.Is(err, provider.ErrNotImplemented) {
			return apiv1.ProviderNameUnsupported, nil
		}
		return apiv1.ProviderNameFailed, err
	}
	return apiv1.ProviderNameSynced, nil
}

func boundedSyncError(err error) string {
	text := strings.Join(strings.Fields(err.Error()), " ")
	if utf8.RuneCountInString(text) > maxSyncErrorRunes {
		text = string([]rune(text)[:maxSyncErrorRunes]) + "…"
	}
	return text
}

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	h.serveEvents(w, r, "")
}

func (h *Handler) handleAgentEvents(w http.ResponseWriter, r *http.Request, id string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	h.serveEvents(w, r, id)
}

func (h *Handler) serveEvents(w http.ResponseWriter, r *http.Request, agentID string) {
	if h.events == nil {
		writeError(w, http.StatusNotImplemented, "events_unavailable", "event queries are unavailable")
		return
	}
	if agentID != "" {
		if _, err := h.reader.Get(r.Context(), agentID); err != nil {
			writeReadError(w, err)
			return
		}
	}
	query := r.URL.Query()
	prov := apiv1.Provider(query.Get("provider"))
	switch prov {
	case "", apiv1.ProviderClaude, apiv1.ProviderCodex:
	default:
		writeError(w, http.StatusBadRequest, "invalid_provider", "provider must be claude or codex")
		return
	}
	limit := registry.DefaultEventLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > registry.MaxEventLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	events, err := h.events.ListEvents(r.Context(), registry.EventListOptions{AgentID: agentID, Provider: prov, Limit: limit})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "registry_error", "could not list events")
		return
	}
	if events == nil {
		events = []apiv1.AgentEvent{}
	}
	writeJSON(w, http.StatusOK, apiv1.AgentEventList{
		SchemaVersion: apiv1.SchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		AgentID:       agentID,
		Provider:      prov,
		Events:        events,
	})
}
