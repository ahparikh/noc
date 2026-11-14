// Package hooks turns provider hook payloads into allow-listed observations
// and projects spooled observations into the agent registry.
package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"noc.local/noc/internal/proclineage"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	// MaxPayloadBytes caps a hook payload read from stdin.
	MaxPayloadBytes = 256 * 1024

	maxPathRunes     = 4096
	maxMetadataRunes = 512
	maxModelRunes    = 256
	maxRequestRunes  = 2048
	maxSummaryRunes  = 1200

	sourceFirstPrompt = "hook_first_observed_user_prompt"
	sourceLastMessage = "hook_last_assistant_message"

	rejectedFallback = "hook payload was rejected"
)

// Normalized event types.
const (
	EventSessionStarted    = "session.started"
	EventSessionEnded      = "session.ended"
	EventTurnPrompted      = "turn.prompted"
	EventTurnStopped       = "turn.stopped"
	EventTurnFailed        = "turn.failed"
	EventWorkspaceChanged  = "workspace.changed"
	EventModelChanged      = "model.changed"
	EventApprovalRequired  = "attention.approval_required"
	EventNotification      = "notification.received"
	EventHookObserved      = "hook.observed"
	EventHookRejected      = "hook.rejected"
	nativeEventUnknownName = "unknown"
)

var (
	// ErrPayloadTooLarge reports a payload over MaxPayloadBytes.
	ErrPayloadTooLarge = errors.New("hook payload exceeds the size limit")
	// ErrCloudSession reports a Claude cloud/web session, which is not tracked.
	ErrCloudSession = errors.New("claude cloud sessions are not tracked")

	errInvalidJSON     = errors.New("hook payload is not valid JSON")
	errNoSessionID     = errors.New("hook payload has no session_id")
	errNoHookEventName = errors.New("hook payload has no hook_event_name")
	errHostIDRequired  = errors.New("hook host ID is required")
)

// Environment is the allow-listed slice of the hook process environment.
type Environment struct {
	ZellijSession       string
	ZellijPaneID        string
	ClaudeRemote        bool
	ClaudeRemoteControl bool
	ProcessID           int
}

// EnvironmentFromOS reads only the allow-listed variables plus process lineage.
func EnvironmentFromOS() Environment {
	return Environment{
		ZellijSession:       os.Getenv("ZELLIJ_SESSION_NAME"),
		ZellijPaneID:        os.Getenv("ZELLIJ_PANE_ID"),
		ClaudeRemote:        os.Getenv("CLAUDE_CODE_REMOTE") == "true",
		ClaudeRemoteControl: os.Getenv("CLAUDE_CODE_BRIDGE_SESSION_ID") != "",
		ProcessID:           proclineage.ProviderProcessID("claude", "codex"),
	}
}

// providerInput is the fixed allow-list of provider payload keys. Everything
// else is dropped by the decoder.
type providerInput struct {
	SessionID            string `json:"session_id"`
	HookEventName        string `json:"hook_event_name"`
	PromptID             string `json:"prompt_id"`
	TurnID               string `json:"turn_id"`
	ToolUseID            string `json:"tool_use_id"`
	CWD                  string `json:"cwd"`
	NewCWD               string `json:"new_cwd"`
	OldCWD               string `json:"old_cwd"`
	Model                string `json:"model"`
	ToModel              string `json:"to_model"`
	Prompt               string `json:"prompt"`
	LastAssistantMessage string `json:"last_assistant_message"`
	SessionTitle         string `json:"session_title"`
	SourceKind           string `json:"source_kind"`
	PermissionMode       string `json:"permission_mode"`
	Reason               string `json:"reason"`
	NotificationType     string `json:"notification_type"`
	Message              string `json:"message"`
	Error                string `json:"error"`
}

// ReadObservation reads at most MaxPayloadBytes from r and normalizes it.
func ReadObservation(r io.Reader, provider apiv1.Provider, hostID string, receivedAt time.Time, env Environment) (apiv1.Observation, error) {
	payload, err := readBounded(r)
	if err != nil {
		return apiv1.Observation{}, err
	}
	return Normalize(provider, hostID, payload, receivedAt, env)
}

func readBounded(r io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(r, MaxPayloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read hook payload: %w", err)
	}
	if len(payload) > MaxPayloadBytes {
		return nil, ErrPayloadTooLarge
	}
	return payload, nil
}

// Normalize converts one provider payload into an allow-listed observation.
func Normalize(provider apiv1.Provider, hostID string, payload []byte, receivedAt time.Time, env Environment) (apiv1.Observation, error) {
	if provider != apiv1.ProviderClaude && provider != apiv1.ProviderCodex {
		return apiv1.Observation{}, fmt.Errorf("unsupported hook provider %q", provider)
	}
	if strings.TrimSpace(hostID) == "" {
		return apiv1.Observation{}, errHostIDRequired
	}
	if provider == apiv1.ProviderClaude && env.ClaudeRemote {
		return apiv1.Observation{}, ErrCloudSession
	}
	var in providerInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return apiv1.Observation{}, errInvalidJSON
	}
	sessionID := bounded(in.SessionID, maxMetadataRunes)
	nativeEvent := bounded(in.HookEventName, maxMetadataRunes)
	if sessionID == "" {
		return apiv1.Observation{}, errNoSessionID
	}
	if nativeEvent == "" {
		return apiv1.Observation{}, errNoHookEventName
	}
	receivedAt = receivedAt.UTC()

	cwd := in.CWD
	if nativeEvent == "CwdChanged" && strings.TrimSpace(in.NewCWD) != "" {
		cwd = in.NewCWD
	}
	model := in.Model
	if strings.TrimSpace(in.ToModel) != "" {
		model = in.ToModel
	}
	eventType, attention := normalizeEvent(nativeEvent, in.NotificationType)

	data := apiv1.ObservationData{
		NativeEvent:    nativeEvent,
		NativeEventID:  bounded(firstNonEmpty(in.PromptID, in.TurnID, in.ToolUseID), maxMetadataRunes),
		Name:           bounded(in.SessionTitle, maxMetadataRunes),
		SourceKind:     bounded(in.SourceKind, maxMetadataRunes),
		CWD:            bounded(cwd, maxPathRunes),
		Model:          bounded(model, maxModelRunes),
		AttentionType:  attention,
		PermissionMode: bounded(in.PermissionMode, maxMetadataRunes),
		Reason:         bounded(compactText(in.Reason), maxMetadataRunes),
		ProcessID:      env.ProcessID,
	}
	if nativeEvent == "UserPromptSubmit" {
		data.InitialRequest = textPreview(in.Prompt, sourceFirstPrompt, maxRequestRunes, receivedAt)
	}
	if nativeEvent == "Stop" || nativeEvent == "StopFailure" {
		data.Summary = textPreview(in.LastAssistantMessage, sourceLastMessage, maxSummaryRunes, receivedAt)
	}
	switch eventType {
	case EventTurnFailed:
		data.AttentionDetail = bounded(compactText(firstNonEmpty(in.Error, in.Message)), maxMetadataRunes)
	case EventApprovalRequired, EventNotification:
		data.AttentionDetail = bounded(compactText(in.Message), maxMetadataRunes)
	}
	data.FrontendBindings = frontendBindings(provider, env, receivedAt)

	obs := apiv1.Observation{
		SchemaVersion:   apiv1.SchemaVersion,
		Provider:        provider,
		HostID:          hostID,
		NativeSessionID: sessionID,
		EventType:       eventType,
		Source:          apiv1.ObservationSourceHook,
		ReceivedAt:      receivedAt,
		Data:            data,
	}
	obs.EventID = deterministicEventID(obs)
	return obs, nil
}

// RejectedObservation records a payload that could not be normalized.
func RejectedObservation(provider apiv1.Provider, hostID string, receivedAt time.Time, err error) apiv1.Observation {
	message := rejectedFallback
	if err != nil {
		message = err.Error()
	}
	receivedAt = receivedAt.UTC()
	idInput := string(provider) + "\x00" + hostID + "\x00" + message + "\x00" + strconv.FormatInt(receivedAt.UnixNano(), 10)
	sum := sha256.Sum256([]byte(idInput))
	return apiv1.Observation{
		SchemaVersion: apiv1.SchemaVersion,
		EventID:       "hook-" + hex.EncodeToString(sum[:16]),
		Provider:      provider,
		HostID:        hostID,
		EventType:     EventHookRejected,
		Source:        apiv1.ObservationSourceHook,
		ReceivedAt:    receivedAt,
		Data:          apiv1.ObservationData{NativeEvent: nativeEventUnknownName},
		Error:         bounded(compactText(message), maxMetadataRunes),
	}
}

func normalizeEvent(nativeEvent, notificationType string) (string, apiv1.AttentionType) {
	switch nativeEvent {
	case "SessionStart":
		return EventSessionStarted, apiv1.AttentionNone
	case "SessionEnd":
		return EventSessionEnded, apiv1.AttentionNone
	case "UserPromptSubmit":
		return EventTurnPrompted, apiv1.AttentionNone
	case "Stop", "Interrupt":
		return EventTurnStopped, apiv1.AttentionNone
	case "StopFailure":
		return EventTurnFailed, apiv1.AttentionError
	case "CwdChanged":
		return EventWorkspaceChanged, apiv1.AttentionNone
	case "PostModelSwitch":
		return EventModelChanged, apiv1.AttentionNone
	case "PermissionRequest":
		return EventApprovalRequired, apiv1.AttentionApprovalRequired
	case "Notification":
		return EventNotification, notificationAttention(notificationType)
	}
	return EventHookObserved, apiv1.AttentionNone
}

func notificationAttention(notificationType string) apiv1.AttentionType {
	kind := strings.ToLower(notificationType)
	switch {
	case strings.Contains(kind, "permission"), strings.Contains(kind, "approval"):
		return apiv1.AttentionApprovalRequired
	case strings.Contains(kind, "auth") && !strings.Contains(kind, "success"):
		return apiv1.AttentionAuthenticationRequired
	case strings.Contains(kind, "input"), strings.Contains(kind, "elicitation"):
		return apiv1.AttentionInputRequired
	}
	return apiv1.AttentionNone
}

func frontendBindings(provider apiv1.Provider, env Environment, at time.Time) []apiv1.FrontendBinding {
	var out []apiv1.FrontendBinding
	if env.ZellijSession != "" || env.ZellijPaneID != "" {
		out = append(out, apiv1.FrontendBinding{
			Kind:       "zellij",
			Session:    bounded(env.ZellijSession, maxMetadataRunes),
			PaneID:     bounded(env.ZellijPaneID, maxMetadataRunes),
			State:      "observed",
			ObservedAt: at,
		})
	}
	if provider == apiv1.ProviderClaude && env.ClaudeRemoteControl {
		out = append(out, apiv1.FrontendBinding{Kind: "claude_remote_control", State: "observed", ObservedAt: at})
	}
	return out
}

func textPreview(text, source string, limit int, at time.Time) *apiv1.TextPreview {
	compacted := compactText(text)
	if compacted == "" {
		return nil
	}
	return &apiv1.TextPreview{
		Text:       bounded(compacted, limit),
		Source:     source,
		Truncated:  len([]rune(compacted)) > limit,
		ObservedAt: at,
	}
}

// bounded trims value and keeps at most n code points.
func bounded(value string, n int) string {
	value = strings.TrimSpace(value)
	if runes := []rune(value); len(runes) > n {
		return string(runes[:n])
	}
	return value
}

func compactText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			return v
		}
	}
	return ""
}

func deterministicEventID(obs apiv1.Observation) string {
	data := obs.Data
	if data.InitialRequest != nil {
		preview := *data.InitialRequest
		preview.ObservedAt = time.Time{}
		data.InitialRequest = &preview
	}
	if data.Summary != nil {
		preview := *data.Summary
		preview.ObservedAt = time.Time{}
		data.Summary = &preview
	}
	if len(data.FrontendBindings) > 0 {
		bindings := make([]apiv1.FrontendBinding, len(data.FrontendBindings))
		copy(bindings, data.FrontendBindings)
		for i := range bindings {
			bindings[i].ObservedAt = time.Time{}
		}
		data.FrontendBindings = bindings
	}
	input := struct {
		Provider        apiv1.Provider        `json:"provider"`
		HostID          string                `json:"host_id"`
		NativeSessionID string                `json:"native_session_id"`
		EventType       string                `json:"event_type"`
		TimeBucket      int64                 `json:"time_bucket,omitempty"`
		Data            apiv1.ObservationData `json:"data"`
	}{
		Provider:        obs.Provider,
		HostID:          obs.HostID,
		NativeSessionID: obs.NativeSessionID,
		EventType:       obs.EventType,
		Data:            data,
	}
	if data.NativeEventID == "" {
		input.TimeBucket = obs.ReceivedAt.Unix()
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		// Unreachable for these types; fall back to a hash of the identity.
		encoded = []byte(string(obs.Provider) + obs.HostID + obs.NativeSessionID + obs.EventType)
	}
	sum := sha256.Sum256(encoded)
	return "hook-" + hex.EncodeToString(sum[:16])
}
