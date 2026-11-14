package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"noc.local/noc/internal/identity"
	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	maxTeamBodyBytes  = 4096
	maxSpawnBodyBytes = 64 * 1024
	maxSpawnPrompt    = 2048
	originSpawn       = "spawn"
	spawnOwner        = "unknown"
)

// WorkspaceInspector describes the directory a spawned agent starts in.
type WorkspaceInspector interface {
	Inspect(ctx context.Context, path, owner string, observedAt time.Time) *apiv1.Workspace
}

// SetSpawnSupport gives the handler what registering a spawn needs: the
// host the rows belong to and, optionally, a workspace inspector.
func (h *Handler) SetSpawnSupport(hostID string, inspector WorkspaceInspector) {
	h.hostID = hostID
	h.inspector = inspector
}

func (h *Handler) handleAgentTeam(w http.ResponseWriter, r *http.Request, id string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	if h.teams == nil {
		writeError(w, http.StatusNotImplemented, "agent_teams_unavailable", "agent teams are unavailable")
		return
	}
	var update apiv1.AgentTeamUpdate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTeamBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_team", "request body must contain a team")
		return
	}
	if update.Team != "" && !apiv1.ValidTeamName(update.Team) {
		writeError(w, http.StatusBadRequest, "invalid_team", invalidTeamMessage)
		return
	}
	var (
		agent apiv1.Agent
		err   error
	)
	switch {
	case !update.Lead:
		agent, err = h.teams.SetTeam(r.Context(), id, update.Team)
	case update.Team == "":
		writeError(w, http.StatusBadRequest, "invalid_team", "a lead needs a team")
		return
	default:
		agent, err = h.reader.Get(r.Context(), id)
		if err == nil && agent.Team != update.Team {
			writeError(w, http.StatusConflict, "not_in_team", fmt.Sprintf("%s is not in team %s", id, update.Team))
			return
		}
		if err == nil {
			agent, err = h.teams.SetLead(r.Context(), id)
		}
	}
	if err != nil {
		if errors.Is(err, registry.ErrNoTeam) {
			writeError(w, http.StatusConflict, "not_in_team", fmt.Sprintf("%s is not in team %s", id, update.Team))
			return
		}
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

const invalidTeamMessage = "a team name is 1 to 48 characters from A-Za-z0-9._-, not starting with - or ., and not none"

// handleSpawn registers a provider session that the caller is about to
// start, so the agent has its team and parent from its first moment. It
// starts nothing.
func (h *Handler) handleSpawn(w http.ResponseWriter, r *http.Request) {
	if h.store == nil || h.teams == nil || h.hostID == "" {
		writeError(w, http.StatusNotImplemented, "agent_spawn_unavailable", "registering spawned agents is unavailable")
		return
	}
	var spawn apiv1.AgentSpawn
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSpawnBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spawn); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_spawn", "request body must describe a spawned agent")
		return
	}
	switch spawn.Provider {
	case apiv1.ProviderClaude, apiv1.ProviderCodex:
	default:
		writeError(w, http.StatusBadRequest, "invalid_provider", "provider must be claude or codex")
		return
	}
	sessionID := strings.TrimSpace(spawn.NativeSessionID)
	if sessionID == "" || utf8.RuneCountInString(sessionID) > 512 {
		writeError(w, http.StatusBadRequest, "invalid_spawn", "a native session ID is required")
		return
	}
	if spawn.Team != "" && spawn.Team != apiv1.NoTeam && !apiv1.ValidTeamName(spawn.Team) {
		writeError(w, http.StatusBadRequest, "invalid_team", invalidTeamMessage)
		return
	}
	name := strings.TrimSpace(spawn.Name)
	if utf8.RuneCountInString(name) > maxNameRunes {
		writeError(w, http.StatusBadRequest, "invalid_agent_name", "agent name must be at most 120 characters")
		return
	}
	ctx := r.Context()

	var parent apiv1.Agent
	if spawn.ParentID != "" {
		var err error
		if parent, err = h.reader.Get(ctx, spawn.ParentID); err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				writeError(w, http.StatusBadRequest, "invalid_parent", "the parent agent was not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "registry_error", "could not read agent")
			return
		}
	}

	// The team: named, inherited from the parent, or none.
	team, named := spawn.Team, spawn.Team != "" && spawn.Team != apiv1.NoTeam
	switch {
	case team == apiv1.NoTeam:
		team = ""
	case team == "":
		team = parent.Team
	}

	agent, err := h.registerSpawn(ctx, spawn, sessionID, name, team, parent.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "registry_error", "could not register the spawned agent")
		return
	}

	// An agent in no team that names a team with no lead for its first
	// worker joins that team and leads it.
	if named && parent.ID != "" && parent.Team == "" {
		led, err := h.teamHasLead(ctx, team)
		if err == nil && !led {
			if _, err = h.teams.SetTeam(ctx, parent.ID, team); err == nil {
				_, err = h.teams.SetLead(ctx, parent.ID)
			}
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "registry_error", "could not set the team's lead")
			return
		}
	}
	writeJSON(w, http.StatusCreated, agent)
}

// registerSpawn creates the agent's row, or, when a hook or a snapshot got
// there first, puts the team and parent on the row that exists.
func (h *Handler) registerSpawn(ctx context.Context, spawn apiv1.AgentSpawn, sessionID, name, team, parentID string) (apiv1.Agent, error) {
	existing, err := h.store.GetByNativeIdentity(ctx, h.hostID, spawn.Provider, sessionID)
	if err == nil {
		if _, err := h.teams.SetTeam(ctx, existing.ID, team); err != nil {
			return apiv1.Agent{}, err
		}
		return h.teams.SetParent(ctx, existing.ID, parentID)
	}
	if !errors.Is(err, registry.ErrNotFound) {
		return apiv1.Agent{}, err
	}
	id, err := identity.NewUUID()
	if err != nil {
		return apiv1.Agent{}, err
	}
	now := time.Now().UTC()
	agent := apiv1.Agent{
		SchemaVersion:   apiv1.SchemaVersion,
		ID:              id,
		HostID:          h.hostID,
		Provider:        spawn.Provider,
		NativeSessionID: sessionID,
		Name:            name,
		Origin:          originSpawn,
		LiveState:       apiv1.LiveStateLive,
		RuntimeState:    apiv1.RuntimeStateStarting,
		ActivityState:   apiv1.ActivityStateUnknown,
		Attention:       apiv1.Attention{Type: apiv1.AttentionNone},
		CreatedAt:       &now,
		FirstSeenAt:     now,
		LastSeenAt:      now,
		Team:            team,
		ParentID:        parentID,
	}
	if prompt := strings.TrimSpace(spawn.Prompt); prompt != "" {
		preview := apiv1.TextPreview{Text: prompt, Source: originSpawn, ObservedAt: now}
		if runes := []rune(prompt); len(runes) > maxSpawnPrompt {
			preview.Text, preview.Truncated = string(runes[:maxSpawnPrompt]), true
		}
		agent.InitialRequest = &preview
	}
	if directory := strings.TrimSpace(spawn.Directory); directory != "" {
		if h.inspector != nil {
			agent.Workspace = h.inspector.Inspect(ctx, directory, spawnOwner, now)
		}
		if agent.Workspace == nil {
			agent.Workspace = &apiv1.Workspace{Path: directory, Owner: spawnOwner, ObservedAt: now}
		}
	}
	if err := h.store.Upsert(ctx, agent); err != nil {
		return apiv1.Agent{}, err
	}
	return h.reader.Get(ctx, id)
}

func (h *Handler) teamHasLead(ctx context.Context, team string) (bool, error) {
	agents, err := h.reader.List(ctx, registry.ListOptions{IncludeNotLive: true})
	if err != nil {
		return false, err
	}
	for _, agent := range agents {
		if agent.Team == team && agent.Lead {
			return true, nil
		}
	}
	return false, nil
}
