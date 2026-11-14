// Package cli implements the noc command line: dispatch, every command, and
// the manual server process lifecycle.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"noc.local/noc/internal/attach"
	"noc.local/noc/internal/buildinfo"
	"noc.local/noc/internal/config"
	"noc.local/noc/internal/doctor"
	"noc.local/noc/internal/hooks"
	"noc.local/noc/internal/localapi"
	"noc.local/noc/internal/registry"
	"noc.local/noc/internal/server"
	setupcmd "noc.local/noc/internal/setup"
	sqlitestore "noc.local/noc/internal/storage/sqlite"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	requestTimeout       = 3 * time.Second
	renameRequestTimeout = 10 * time.Second
	hookIngestTimeout    = 900 * time.Millisecond
	hookWakeTimeout      = 150 * time.Millisecond
	defaultServerTimeout = 15 * time.Second
	agentIDEnvironment   = "NOC_AGENT_ID"
)

const usageText = `noc manages the local coding-agent registry.

Usage:
  noc
  noc version
  noc server [--bg] [--socket path] [--database path]
  noc server restart
  noc server stop
  noc server status [--json]
  noc ls [--json] [--all] [--bucket blocked,working,review,quiet] [--team name|none]
  noc search [--json] [--older-than age] [--newer-than age] [--bucket ...] [--team name|none] [text...]
  noc show [--json] <agent-id>
  noc attach [--force] <agent-id>
  noc rename [--clear] <agent-id> [name]
  noc stop <agent-id>
  noc forget <agent-id> [<agent-id>...]
  noc events [--json] [--provider claude|codex] [--limit count] [agent-id]
  noc watch [--json] [--all] [--team name|none]
  noc teams [--json]
  noc team set <name> [agent-id...]
  noc team clear [agent-id...]
  noc team lead <name> [agent-id]
  noc spawn [--provider claude] [--team name|none] [--name name] [--dir path] <prompt> [-- provider-arguments]
  noc doctor [--json]
  noc setup [--check|--uninstall] [claude|codex]

Internal provider hook entrypoint:
  noc hook ingest --provider claude|codex

The server runs in the foreground unless --bg is supplied. Provider discovery
and durable hook ingestion are implemented. Run noc setup after installing the
binary to configure Claude and Codex hooks.`

// Run executes the noc command line and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunWithIO(ctx, args, os.Stdin, stdout, stderr)
}

// RunWithIO is Run with an explicit standard input.
func RunWithIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runList(ctx, nil, stdout, stderr)
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usageText)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "ls", "list":
		return runList(ctx, rest, stdout, stderr)
	case "search", "find":
		return runSearch(ctx, rest, stdout, stderr)
	case "show":
		return runShow(ctx, rest, stdout, stderr)
	case "attach":
		return runAttach(ctx, rest, stdin, stdout, stderr)
	case "rename", "name":
		return runRename(ctx, rest, stdout, stderr)
	case "stop":
		return runStop(ctx, rest, stdout, stderr)
	case "forget":
		return runForget(ctx, rest, stdout, stderr)
	case "events":
		return runEvents(ctx, rest, stdout, stderr)
	case "watch":
		return runWatch(ctx, rest, stdout, stderr)
	case "team":
		return runTeam(ctx, rest, stdout, stderr)
	case "teams":
		return runTeams(ctx, rest, stdout, stderr)
	case "spawn":
		return runSpawn(ctx, rest, stdout, stderr)
	case "doctor":
		return runDoctor(ctx, rest, stdout, stderr)
	case "setup":
		return runSetup(ctx, rest, stdout, stderr)
	case "server":
		return runServer(ctx, rest, stdout, stderr)
	case "hook":
		return runHook(ctx, rest, stdin, stderr)
	default:
		fmt.Fprintf(stderr, "noc: unknown command %q\n\n%s\n", args[0], usageText)
		return 2
	}
}

// ---- shared helpers -------------------------------------------------------

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags returns (exit code, true) when the caller should stop.
func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	return 0, false
}

func printError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "noc: %v\n", err)
	return 1
}

func printServerError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "noc: %v\n", err)
	if errors.Is(err, localapi.ErrConnection) {
		fmt.Fprintln(stderr, "Is the server running? Start it with: noc server")
	}
	return 1
}

func printServerLifecycleError(stderr io.Writer, err error) int {
	if isServerAlreadyRunningError(err) {
		fmt.Fprintln(stderr, "noc: noc server is already running")
		fmt.Fprintln(stderr, "Stop the previous instance by running: noc server stop")
		return 1
	}
	return printError(stderr, err)
}

func writeJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(stderr, "noc: write JSON: %v\n", err)
		return 1
	}
	return 0
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func agentDisplayName(agent apiv1.Agent) string {
	if agent.UserName != "" {
		return agent.UserName
	}
	return agent.Name
}

func agentWorkspacePath(agent apiv1.Agent) string {
	if agent.Workspace == nil {
		return ""
	}
	return agent.Workspace.Path
}

// ---- ls -------------------------------------------------------------------

func runList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc ls", stderr)
	asJSON := fs.Bool("json", false, "write JSON")
	all := fs.Bool("all", false, "include retained non-live agents")
	bucketFlag := fs.String("bucket", "", "comma-separated buckets: blocked, working, review, quiet")
	teamFlag := fs.String("team", "", "keep one team's agents, or none for agents in no team")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "noc ls: unexpected positional arguments")
		return 2
	}
	buckets, err := parseBuckets(*bucketFlag)
	if err != nil {
		fmt.Fprintf(stderr, "noc ls: %v\n", err)
		return 2
	}
	team, err := parseTeamFilter(*teamFlag)
	if err != nil {
		fmt.Fprintf(stderr, "noc ls: %v\n", err)
		return 2
	}

	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	list, err := client.ListAgents(callCtx, *all)
	if err != nil {
		return printServerError(stderr, err)
	}
	list.Agents = filterTeam(filterBuckets(list.Agents, buckets), team)
	sortByBucket(list.Agents)
	if list.Agents == nil {
		list.Agents = []apiv1.Agent{}
	}
	if *asJSON {
		return writeJSON(stdout, stderr, list)
	}
	renderAgents(stdout, list.Agents)
	return 0
}

func parseBuckets(value string) (map[apiv1.Bucket]bool, error) {
	selected := map[apiv1.Bucket]bool{}
	for _, part := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		known := false
		for _, bucket := range apiv1.Buckets {
			if string(bucket) == name {
				known = true
				selected[bucket] = true
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown bucket %q (want blocked, working, review, or quiet)", name)
		}
	}
	return selected, nil
}

func filterBuckets(agents []apiv1.Agent, selected map[apiv1.Bucket]bool) []apiv1.Agent {
	if len(selected) == 0 {
		return agents
	}
	kept := make([]apiv1.Agent, 0, len(agents))
	for _, agent := range agents {
		if selected[agent.Bucket] {
			kept = append(kept, agent)
		}
	}
	return kept
}

func sortByBucket(agents []apiv1.Agent) {
	sort.SliceStable(agents, func(i, j int) bool {
		ri, rj := agents[i].Bucket.Rank(), agents[j].Bucket.Rank()
		if ri != rj {
			return ri < rj
		}
		return agentRecency(agents[i]).After(agentRecency(agents[j]))
	})
}

// agentRecency deliberately ignores LastSeenAt: reconciliation refreshes it
// on every pass, so it says nothing about when the user last cared.
func agentRecency(agent apiv1.Agent) time.Time {
	switch {
	case agent.LastActivityAt != nil:
		return *agent.LastActivityAt
	case agent.EndedAt != nil:
		return *agent.EndedAt
	case agent.CreatedAt != nil:
		return *agent.CreatedAt
	default:
		return agent.FirstSeenAt
	}
}

func renderAgents(w io.Writer, agents []apiv1.Agent) {
	if len(agents) == 0 {
		fmt.Fprintln(w, "No live agents.")
		return
	}
	table := newTable(w)
	teams := anyInTeam(agents)
	if teams {
		fmt.Fprintln(table, "ID\tTEAM\tPROVIDER\tBUCKET\tSTATE\tWORKSPACE\tNAME")
	} else {
		fmt.Fprintln(table, "ID\tPROVIDER\tBUCKET\tSTATE\tWORKSPACE\tNAME")
	}
	for _, agent := range agents {
		fmt.Fprintf(table, "%s\t", agent.ID)
		if teams {
			fmt.Fprintf(table, "%s\t", teamCell(agent))
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", agent.Provider, agent.Bucket,
			agent.DisplayState, dash(agentWorkspacePath(agent)), dash(agentDisplayName(agent)))
	}
	_ = table.Flush()
}

// ---- show -----------------------------------------------------------------

func runShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc show", stderr)
	asJSON := fs.Bool("json", false, "write JSON")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noc show [--json] <agent-id>")
		return 2
	}
	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	agent, err := client.GetAgent(callCtx, fs.Arg(0))
	if err != nil {
		return printServerError(stderr, err)
	}
	if *asJSON {
		return writeJSON(stdout, stderr, agent)
	}
	renderAgent(stdout, agent)
	return 0
}

func renderAgent(w io.Writer, agent apiv1.Agent) {
	table := newTable(w)
	row := func(label, value string) { fmt.Fprintf(table, "%s:\t%s\n", label, value) }
	row("ID", agent.ID)
	row("Provider", string(agent.Provider))
	row("Native session", agent.NativeSessionID)
	row("Host", agent.HostID)
	row("State", string(agent.DisplayState))
	row("Runtime", string(agent.RuntimeState))
	row("Activity", string(agent.ActivityState))
	if agent.UserName != "" {
		row("User name", agent.UserName)
	}
	if agent.Name != "" {
		row("Provider name", agent.Name)
	}
	if agent.Team != "" {
		if agent.Lead {
			row("Team", agent.Team+" (lead)")
		} else {
			row("Team", agent.Team)
		}
	}
	if agent.ParentID != "" {
		row("Parent", agent.ParentID)
	}
	if agent.Model != "" {
		row("Model", agent.Model)
	}
	if agent.ReasoningEffort != "" {
		row("Reasoning effort", agent.ReasoningEffort)
	}
	if agent.InitialRequest != nil {
		row("Initial request", agent.InitialRequest.Text)
		row("Initial request source", agent.InitialRequest.Source)
	}
	if agent.Summary != "" {
		row("Current summary", agent.Summary)
		if agent.SummarySource != "" {
			row("Summary source", agent.SummarySource)
		}
	}
	if agent.Workspace != nil {
		row("Directory", agent.Workspace.Path)
		if agent.Workspace.RootPath != "" {
			row("Workspace root", agent.Workspace.RootPath)
		}
		if agent.Workspace.Kind != "" {
			row("Workspace kind", agent.Workspace.Kind)
		}
		if agent.Workspace.Branch != "" {
			row("Branch", agent.Workspace.Branch)
		}
	}
	for _, binding := range agent.FrontendBindings {
		value := binding.Kind
		if binding.Session != "" {
			value += ":" + binding.Session
		}
		if binding.PaneID != "" {
			value += "/" + binding.PaneID
		}
		row("Frontend", fmt.Sprintf("%s (%s)", value, binding.State))
	}
	_ = table.Flush()
}

// ---- events ---------------------------------------------------------------

func runEvents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc events", stderr)
	asJSON := fs.Bool("json", false, "write JSON")
	providerFlag := fs.String("provider", "", "filter by provider: claude or codex")
	limit := fs.Int("limit", registry.DefaultEventLimit, "maximum events (1-200)")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: noc events [--json] [--provider claude|codex] [--limit count] [agent-id]")
		return 2
	}
	switch *providerFlag {
	case "", string(apiv1.ProviderClaude), string(apiv1.ProviderCodex):
	default:
		fmt.Fprintln(stderr, "noc events: provider must be claude or codex")
		return 2
	}
	if *limit < 1 || *limit > registry.MaxEventLimit {
		fmt.Fprintf(stderr, "noc events: limit must be between 1 and %d\n", registry.MaxEventLimit)
		return 2
	}
	agentID := ""
	if fs.NArg() == 1 {
		agentID = fs.Arg(0)
	}
	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	list, err := client.ListEvents(callCtx, agentID, apiv1.Provider(*providerFlag), *limit)
	if err != nil {
		return printServerError(stderr, err)
	}
	if list.Events == nil {
		list.Events = []apiv1.AgentEvent{}
	}
	if *asJSON {
		return writeJSON(stdout, stderr, list)
	}
	renderEvents(stdout, list.Events)
	return 0
}

func renderEvents(w io.Writer, events []apiv1.AgentEvent) {
	if len(events) == 0 {
		fmt.Fprintln(w, "No events found.")
		return
	}
	table := newTable(w)
	fmt.Fprintln(table, "TIME\tPROVIDER\tSOURCE\tEVENT\tAGENT\tDETAIL")
	for _, event := range events {
		when := event.ReceivedAt
		if event.OccurredAt != nil {
			when = *event.OccurredAt
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", when.Local().Format("2006-01-02 15:04:05"),
			event.Provider, event.Source, event.EventType, dash(event.AgentID), renderEventDetail(event))
	}
	_ = table.Flush()
}

func renderEventDetail(event apiv1.AgentEvent) string {
	var parts []string
	if event.NativeEvent != "" {
		parts = append(parts, "native="+event.NativeEvent)
	}
	if event.PreviousLiveState != "" || event.LiveState != "" {
		parts = append(parts, fmt.Sprintf("%s -> %s", dash(string(event.PreviousLiveState)), dash(string(event.LiveState))))
	}
	if event.Reason != "" {
		parts = append(parts, "reason="+event.Reason)
	}
	if event.Error != "" {
		parts = append(parts, "error="+event.Error)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "; ")
}

// ---- watch ----------------------------------------------------------------

func runWatch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc watch", stderr)
	asJSON := fs.Bool("json", false, "write NDJSON snapshots")
	all := fs.Bool("all", false, "include retained non-live agents")
	teamFlag := fs.String("team", "", "keep one team's agents, or none for agents in no team")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "noc watch: unexpected positional arguments")
		return 2
	}
	team, err := parseTeamFilter(*teamFlag)
	if err != nil {
		fmt.Fprintf(stderr, "noc watch: %v\n", err)
		return 2
	}
	client := localapi.NewClient(*socket)
	defer client.Close()

	encoder := json.NewEncoder(stdout)
	var previous *apiv1.AgentList
	err = client.WatchAgents(ctx, *all, func(list apiv1.AgentList) error {
		list.Agents = filterTeam(list.Agents, team)
		if *asJSON {
			return encoder.Encode(list)
		}
		if previous == nil {
			fmt.Fprintln(stdout, "Watching agents; press Ctrl-C to stop.")
			renderAgents(stdout, list.Agents)
		} else {
			renderAgentChanges(stdout, *previous, list)
		}
		snapshot := list
		previous = &snapshot
		return nil
	})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0
		}
		return printServerError(stderr, err)
	}
	return 0
}

type agentChange struct {
	kind  string
	id    string
	cells []string
}

func renderAgentChanges(w io.Writer, previous, current apiv1.AgentList) {
	before := map[string]apiv1.Agent{}
	for _, agent := range previous.Agents {
		before[agent.ID] = agent
	}
	after := map[string]apiv1.Agent{}
	for _, agent := range current.Agents {
		after[agent.ID] = agent
	}
	stamp := current.GeneratedAt.Local().Format("15:04:05")
	var changes []agentChange
	row := func(kind string, agent apiv1.Agent, state, name string) {
		changes = append(changes, agentChange{kind: kind, id: agent.ID, cells: []string{
			stamp, kind, agent.ID, string(agent.Provider), state, dash(agentWorkspacePath(agent)), name,
		}})
	}
	for id, agent := range after {
		old, existed := before[id]
		switch {
		case !existed:
			row("ADDED", agent, string(agent.DisplayState), dash(agentDisplayName(agent)))
		case !watchAgentsEqual(old, agent):
			state := string(agent.DisplayState)
			if old.DisplayState != agent.DisplayState {
				state = fmt.Sprintf("%s -> %s", old.DisplayState, agent.DisplayState)
			}
			name := dash(agentDisplayName(agent))
			if agentDisplayName(old) != agentDisplayName(agent) {
				name = fmt.Sprintf("%s -> %s", dash(agentDisplayName(old)), dash(agentDisplayName(agent)))
			}
			row("UPDATED", agent, state, name)
		}
	}
	for id, agent := range before {
		if _, ok := after[id]; !ok {
			row("REMOVED", agent, string(agent.DisplayState), dash(agentDisplayName(agent)))
		}
	}
	if len(changes) == 0 {
		return
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].kind != changes[j].kind {
			return changes[i].kind < changes[j].kind
		}
		return changes[i].id < changes[j].id
	})
	table := newTable(w)
	for _, change := range changes {
		fmt.Fprintln(table, strings.Join(change.cells, "\t"))
	}
	_ = table.Flush()
}

func watchAgentsEqual(a, b apiv1.Agent) bool {
	return reflect.DeepEqual(watchAgentProjection(a), watchAgentProjection(b))
}

// watchAgentProjection drops fields that reconciliation refreshes without any
// user-visible change.
func watchAgentProjection(agent apiv1.Agent) apiv1.Agent {
	agent.LastSeenAt = time.Time{}
	agent.SnapshotConfirmedAt = nil
	agent.ModelObservedAt = nil
	agent.ReasoningObservedAt = nil
	agent.SummaryUpdatedAt = nil
	if agent.InitialRequest != nil {
		request := *agent.InitialRequest
		request.ObservedAt = time.Time{}
		agent.InitialRequest = &request
	}
	if agent.Workspace != nil {
		workspace := *agent.Workspace
		workspace.ObservedAt = time.Time{}
		agent.Workspace = &workspace
	}
	if agent.FrontendBindings != nil {
		bindings := make([]apiv1.FrontendBinding, len(agent.FrontendBindings))
		copy(bindings, agent.FrontendBindings)
		for i := range bindings {
			bindings[i].ObservedAt = time.Time{}
		}
		agent.FrontendBindings = bindings
	}
	return agent
}

// ---- rename / forget ------------------------------------------------------

func runRename(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc rename", stderr)
	clear := fs.Bool("clear", false, "remove the noc name")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	rest := fs.Args()
	if (*clear && len(rest) != 1) || (!*clear && len(rest) < 2) {
		fmt.Fprintln(stderr, "usage: noc rename [--clear] <agent-id> [name]")
		return 2
	}
	name := ""
	if !*clear {
		name = strings.TrimSpace(strings.Join(rest[1:], " "))
		if name == "" {
			fmt.Fprintln(stderr, "noc rename: name cannot be empty; use --clear to remove it")
			return 2
		}
	}
	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, renameRequestTimeout)
	defer cancel()
	result, err := client.RenameAgent(callCtx, rest[0], name)
	if err != nil {
		return printServerError(stderr, err)
	}
	agent := result.Agent
	if *clear {
		fmt.Fprintf(stdout, "Cleared the name for %s.\n", agent.ID)
		return 0
	}
	title := providerTitle(agent.Provider)
	switch result.ProviderSync {
	case apiv1.ProviderNameSynced:
		fmt.Fprintf(stdout, "Named %s %q; the %s session was renamed too.\n", agent.ID, agent.UserName, agent.Provider)
	case apiv1.ProviderNameUnsupported:
		fmt.Fprintf(stdout, "Named %s %q in noc. %s cannot be renamed from outside, so the session keeps its own name until you rename it there.\n",
			agent.ID, agent.UserName, title)
	case apiv1.ProviderNameFailed:
		fmt.Fprintf(stdout, "Named %s %q in noc.\n", agent.ID, agent.UserName)
		fmt.Fprintf(stderr, "noc rename: could not rename the %s session: %s\n", agent.Provider, result.ProviderSyncError)
	default:
		fmt.Fprintf(stdout, "Named %s %q.\n", agent.ID, agent.UserName)
	}
	return 0
}

func providerTitle(provider apiv1.Provider) string {
	switch provider {
	case apiv1.ProviderClaude:
		return "Claude"
	case apiv1.ProviderCodex:
		return "Codex"
	default:
		return string(provider)
	}
}

func runForget(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc forget", stderr)
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: noc forget <agent-id> [<agent-id>...]")
		return 2
	}
	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	code := 0
	for _, id := range fs.Args() {
		if err := client.ForgetAgent(callCtx, id); err != nil {
			printServerError(stderr, err)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "forgot %s\n", id)
	}
	return code
}

// ---- doctor / setup -------------------------------------------------------

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc doctor", stderr)
	asJSON := fs.Bool("json", false, "write JSON")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	database := fs.String("database", paths.DatabasePath, "database path")
	timeout := fs.Duration("timeout", defaultServerTimeout, "diagnostics timeout")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "noc doctor: unexpected positional arguments")
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "noc doctor: timeout must be positive")
		return 2
	}
	callCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	report, err := doctor.Run(callCtx, doctor.Options{SocketPath: *socket, DatabasePath: *database})
	if err != nil {
		return printError(stderr, err)
	}
	if *asJSON {
		if code := writeJSON(stdout, stderr, report); code != 0 {
			return code
		}
	} else {
		renderDoctorReport(stdout, report)
	}
	if report.HasFailures() {
		return 1
	}
	return 0
}

func renderDoctorReport(w io.Writer, report doctor.Report) {
	fmt.Fprintf(w, "noc doctor: %s\n", report.Status)
	table := newTable(w)
	for _, check := range report.Checks {
		fmt.Fprintf(table, "%s\t%s\t%s\n", strings.ToUpper(string(check.Status)), check.ID, check.Summary)
		if check.Detail != "" {
			fmt.Fprintf(table, "\t\t%s\n", check.Detail)
		}
		if check.Remediation != "" {
			fmt.Fprintf(table, "\t\tNext: %s\n", check.Remediation)
		}
	}
	_ = table.Flush()
}

func runSetup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("noc setup", stderr)
	check := fs.Bool("check", false, "check hook configuration without changing it")
	uninstall := fs.Bool("uninstall", false, "remove noc-managed hooks")
	asJSON := fs.Bool("json", false, "write JSON")
	executable := fs.String("executable", "", "noc executable path for hook commands")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if *check && *uninstall {
		fmt.Fprintln(stderr, "noc setup: --check and --uninstall are mutually exclusive")
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: noc setup [--check|--uninstall] [claude|codex]")
		return 2
	}
	var providers []apiv1.Provider
	if fs.NArg() == 1 {
		switch fs.Arg(0) {
		case "claude":
			providers = []apiv1.Provider{apiv1.ProviderClaude}
		case "codex":
			providers = []apiv1.Provider{apiv1.ProviderCodex}
		default:
			fmt.Fprintf(stderr, "noc setup: unsupported provider %q\n", fs.Arg(0))
			return 2
		}
	}
	action := setupcmd.ActionInstall
	if *check {
		action = setupcmd.ActionCheck
	} else if *uninstall {
		action = setupcmd.ActionUninstall
	}
	report, err := setupcmd.Run(ctx, setupcmd.Options{Action: action, Providers: providers, Executable: *executable})
	if err != nil {
		return printError(stderr, err)
	}
	if *asJSON {
		if code := writeJSON(stdout, stderr, report); code != 0 {
			return code
		}
	} else {
		renderSetupReport(stdout, report)
	}
	if report.HasFailures() {
		return 1
	}
	return 0
}

func renderSetupReport(w io.Writer, report setupcmd.Report) {
	fmt.Fprintf(w, "noc setup %s using %s\n", report.Action, report.ExecutablePath)
	for _, result := range report.Results {
		p := result.Provider
		if result.Error != "" {
			fmt.Fprintf(w, "  %s: error — %s\n", p, result.Error)
			continue
		}
		switch report.Action {
		case setupcmd.ActionCheck:
			if result.Complete {
				fmt.Fprintf(w, "  %s: configured\n", p)
			} else {
				fmt.Fprintf(w, "  %s: incomplete\n", p)
				renderSetupInspection(w, result)
			}
		case setupcmd.ActionUninstall:
			if result.Removed > 0 {
				fmt.Fprintf(w, "  %s: removed %d hook handlers from %s\n", p, result.Removed, result.Path)
			} else {
				fmt.Fprintf(w, "  %s: no managed hooks found in %s\n", p, result.Path)
			}
		default:
			if result.Changed {
				fmt.Fprintf(w, "  %s: installed in %s\n", p, result.Path)
			} else {
				fmt.Fprintf(w, "  %s: already configured in %s\n", p, result.Path)
			}
			renderSetupInspection(w, result)
			if p == apiv1.ProviderCodex {
				fmt.Fprintln(w, "    Review and trust the new hook definitions when Codex prompts you.")
			}
		}
	}
}

func renderSetupInspection(w io.Writer, result setupcmd.Result) {
	inspection := result.Inspection
	if len(inspection.Errors) > 0 {
		fmt.Fprintf(w, "    errors: %s\n", strings.Join(inspection.Errors, "; "))
	}
	if inspection.HooksDisabled {
		fmt.Fprintln(w, "    hooks are disabled in Codex [features]")
	}
	if len(inspection.MissingEvents) > 0 {
		fmt.Fprintf(w, "    missing events: %s\n", strings.Join(inspection.MissingEvents, ", "))
	}
	if len(inspection.MismatchedExecutableEvents) > 0 {
		fmt.Fprintf(w, "    events using another executable: %s\n", strings.Join(inspection.MismatchedExecutableEvents, ", "))
	}
}

// ---- hook -----------------------------------------------------------------

func runHook(ctx context.Context, args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "ingest" {
		fmt.Fprintln(stderr, "usage: noc hook ingest --provider claude|codex")
		return 2
	}
	return runHookIngest(ctx, args[1:], stdin)
}

// runHookIngest never writes to stdout or stderr and always returns 0 so a
// provider is never blocked by noc.
func runHookIngest(ctx context.Context, args []string, stdin io.Reader) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	paths, err := config.ResolvePaths()
	if err != nil {
		return 0
	}
	fs := newFlagSet("noc hook ingest", io.Discard)
	providerFlag := fs.String("provider", "", "claude or codex")
	database := fs.String("database", paths.DatabasePath, "database path")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return 0
	}
	provider := apiv1.Provider(*providerFlag)
	if provider != apiv1.ProviderClaude && provider != apiv1.ProviderCodex {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, hookIngestTimeout)
	defer cancel()
	store, err := sqlitestore.Open(ctx, *database)
	if err != nil {
		return 0
	}
	defer store.Close()
	hostID, err := store.HostID(ctx)
	if err != nil {
		return 0
	}
	receivedAt := time.Now().UTC()
	observation, err := hooks.ReadObservation(stdin, provider, hostID, receivedAt, hooks.EnvironmentFromOS())
	if err != nil {
		if errors.Is(err, hooks.ErrCloudSession) {
			return 0
		}
		observation = hooks.RejectedObservation(provider, hostID, receivedAt, err)
	}
	if _, err := store.EnqueueObservation(ctx, observation); err != nil {
		return 0
	}
	wakeCtx, wakeCancel := context.WithTimeout(ctx, hookWakeTimeout)
	defer wakeCancel()
	client := localapi.NewClient(*socket)
	defer client.Close()
	_ = client.WakeHooks(wakeCtx)
	return 0
}

// ---- attach / stop --------------------------------------------------------

func providerBinaries() attach.Binaries {
	bins := attach.DefaultBinaries
	if value := strings.TrimSpace(os.Getenv("NOC_CLAUDE_BIN")); value != "" {
		bins.Claude = value
	}
	if value := strings.TrimSpace(os.Getenv("NOC_CODEX_BIN")); value != "" {
		bins.Codex = value
	}
	return bins
}

func lookupAgent(ctx context.Context, socket, id string) (apiv1.Agent, error) {
	client := localapi.NewClient(socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return client.GetAgent(callCtx, id)
}

func runAttach(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc attach", stderr)
	force := fs.Bool("force", false, "attach even when doing so would run a second process on a live conversation")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noc attach [--force] <agent-id>")
		return 2
	}
	agent, err := lookupAgent(ctx, *socket, fs.Arg(0))
	if err != nil {
		return printServerError(stderr, err)
	}

	bins := providerBinaries()
	plan, err := attach.PlanForAgent(agent, bins)
	if err != nil {
		return printError(stderr, err)
	}
	if agent.Provider == apiv1.ProviderClaude {
		// A roster failure is deliberately swallowed: the registry plan stands.
		if roster, rosterErr := attach.ReadClaudeRoster(ctx, bins.Claude); rosterErr == nil {
			rosterPlan, planErr := attach.PlanClaudeWithRoster(agent, roster, bins.Claude, *force)
			switch {
			case errors.Is(planErr, attach.ErrRunningInTerminal):
				fmt.Fprintf(stderr, "noc: refusing to attach: %v.\n", planErr)
				fmt.Fprintln(stderr, "Re-run with --force to open it here anyway.")
				return 1
			case planErr == nil:
				plan = rosterPlan
			}
		}
	}
	if risky, suffix := attach.ConcurrentUseRisk(agent, plan); risky && !*force {
		fmt.Fprintf(stderr, "noc: refusing to attach: this Claude session is live and has no background attach ID, so resuming it here would start a second independent process on the same conversation rather than joining the existing one%s.\n", suffix)
		fmt.Fprintln(stderr, "Focus the terminal that already has it, or re-run with --force to start a second process anyway.")
		return 1
	}
	program, err := exec.LookPath(plan.Program)
	if err != nil {
		return printError(stderr, fmt.Errorf("find %s executable %q: %w", agent.Provider, plan.Program, err))
	}
	if plan.Directory != "" {
		info, err := os.Stat(plan.Directory)
		if err != nil {
			return printError(stderr, fmt.Errorf("open agent workspace %q: %w", plan.Directory, err))
		}
		if !info.IsDir() {
			return printError(stderr, fmt.Errorf("agent workspace %q is not a directory", plan.Directory))
		}
	}
	environment := replaceEnvironmentValue(os.Environ(), agentIDEnvironment, agent.ID)

	if plan.Mode == attach.ModeBackgroundResume {
		restore := exec.CommandContext(ctx, program, plan.Arguments...)
		restore.Dir = plan.Directory
		restore.Env = environment
		output, err := restore.CombinedOutput()
		if err != nil {
			_, _ = stderr.Write(output)
			return printError(stderr, fmt.Errorf("resume Claude session in the background: %w", err))
		}
		roster, err := attach.ReadClaudeRoster(ctx, bins.Claude)
		if err != nil {
			return printError(stderr, err)
		}
		job := attach.BackgroundJobFor(roster, agent.NativeSessionID)
		if job == "" {
			return printError(stderr, errors.New("Claude resumed the session but lists no background job for it"))
		}
		plan = attach.Plan{Program: plan.Program, Arguments: []string{"attach", job}, Directory: plan.Directory, Mode: attach.ModeAttach}
	}

	child := exec.CommandContext(ctx, program, plan.Arguments...)
	child.Dir = plan.Directory
	child.Env = environment
	child.Stdin = stdin
	child.Stdout = stdout
	child.Stderr = stderr
	err = child.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		if ctx.Err() != nil {
			return 130
		}
	}
	return printError(stderr, fmt.Errorf("attach %s agent: %w", agent.Provider, err))
}

func runStop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc stop", stderr)
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noc stop <agent-id>")
		return 2
	}
	agent, err := lookupAgent(ctx, *socket, fs.Arg(0))
	if err != nil {
		return printServerError(stderr, err)
	}
	plan, err := attach.StopPlan(agent, providerBinaries().Claude)
	if err != nil {
		return printError(stderr, err)
	}
	program, err := exec.LookPath(plan.Program)
	if err != nil {
		return printError(stderr, fmt.Errorf("find %s executable %q: %w", agent.Provider, plan.Program, err))
	}
	child := exec.CommandContext(ctx, program, plan.Arguments...)
	child.Stdout = stdout
	child.Stderr = stderr
	err = child.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode()
	}
	return printError(stderr, fmt.Errorf("stop %s agent: %w", agent.Provider, err))
}

// ---- server ---------------------------------------------------------------

// serverLaunchFlags are shared by `server` and `server restart`.
type serverLaunchFlags struct {
	socket, database, pidFile, logFormat string
	background, discovery                bool
	discoveryInterval, staleAfter        time.Duration
}

func (v *serverLaunchFlags) bind(fs *flag.FlagSet, paths config.Paths, includeBackground bool) {
	fs.StringVar(&v.socket, "socket", paths.SocketPath, "server socket")
	fs.StringVar(&v.database, "database", paths.DatabasePath, "database path")
	fs.StringVar(&v.pidFile, "pid-file", "", "PID record path (default: beside the socket)")
	if includeBackground {
		fs.BoolVar(&v.background, "bg", false, "run the server in the background")
	}
	fs.StringVar(&v.logFormat, "log-format", "text", "log format: text or json")
	fs.BoolVar(&v.discovery, "discovery", true, "enable provider discovery")
	fs.DurationVar(&v.discoveryInterval, "discovery-interval", server.DefaultDiscoveryInterval, "provider discovery interval")
	fs.DurationVar(&v.staleAfter, "stale-after", server.DefaultStaleAfter, "age after which an unseen agent is stale")
}

// validate returns a message for the first invalid value, or "".
func (v *serverLaunchFlags) validate() string {
	if v.logFormat != "text" && v.logFormat != "json" {
		return fmt.Sprintf("invalid log format %q", v.logFormat)
	}
	if v.discovery {
		if v.discoveryInterval <= 0 {
			return "discovery interval must be positive"
		}
		if v.staleAfter <= 0 {
			return "stale-after must be positive"
		}
	}
	return ""
}

func (v *serverLaunchFlags) options() serverOptions {
	pidFile := v.pidFile
	if pidFile == "" {
		pidFile = defaultServerPIDFile(v.socket)
	}
	return serverOptions{
		Socket:            v.socket,
		Database:          v.database,
		PIDFile:           pidFile,
		LogFormat:         v.logFormat,
		Discovery:         v.discovery,
		DiscoveryInterval: v.discoveryInterval,
		StaleAfter:        v.staleAfter,
	}
}

func runServer(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return runServerStatus(ctx, args[1:], stdout, stderr)
		case "stop":
			return runServerStop(ctx, args[1:], stdout, stderr)
		case "restart":
			return runServerRestart(ctx, args[1:], stdout, stderr)
		case "run", "start":
			args = args[1:]
		}
	}
	if len(args) > 0 && (args[0] == "" || args[0][0] != '-') {
		fmt.Fprintf(stderr, "noc server: unknown command %q\n", args[0])
		return 2
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc server", stderr)
	var values serverLaunchFlags
	values.bind(fs, paths, true)
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "noc server: unexpected positional arguments")
		return 2
	}
	if message := values.validate(); message != "" {
		fmt.Fprintf(stderr, "noc server: %s\n", message)
		return 2
	}
	opts := values.options()
	if values.background {
		return runServerBackground(ctx, opts, stdout, stderr)
	}
	return runServerForeground(ctx, opts, stderr)
}

func runServerBackground(ctx context.Context, opts serverOptions, stdout, stderr io.Writer) int {
	if err := checkServerAlreadyRunning(ctx, opts.Socket, opts.PIDFile); err != nil {
		return printServerLifecycleError(stderr, err)
	}
	pid, logPath, err := startBackgroundServer(ctx, opts)
	if err != nil {
		return printServerLifecycleError(stderr, err)
	}
	fmt.Fprintf(stdout, "noc server started in background (pid %d)\n", pid)
	fmt.Fprintf(stdout, "logs: %s\n", logPath)
	return 0
}

func runServerForeground(ctx context.Context, opts serverOptions, stderr io.Writer) int {
	notifier := startupNotifierFromEnvironment()
	defer notifier.close()
	mode := pidModeForeground
	if notifier != nil {
		mode = pidModeBackground
	}
	release, err := acquireServerPIDFile(opts.PIDFile, opts.Socket, mode)
	if err != nil {
		notifier.fail(err)
		return printServerLifecycleError(stderr, err)
	}
	defer release()

	var handler slog.Handler
	if opts.LogFormat == "json" {
		handler = slog.NewJSONHandler(stderr, nil)
	} else {
		handler = slog.NewTextHandler(stderr, nil)
	}
	err = server.Run(ctx, server.Config{
		SocketPath:        opts.Socket,
		DatabasePath:      opts.Database,
		Logger:            slog.New(handler),
		Ready:             notifier.ready,
		DiscoveryInterval: opts.DiscoveryInterval,
		StaleAfter:        opts.StaleAfter,
		DisableDiscovery:  !opts.Discovery,
	})
	if err != nil {
		notifier.fail(err)
		return printServerLifecycleError(stderr, err)
	}
	return 0
}

func runServerStop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc server stop", stderr)
	socket := fs.String("socket", paths.SocketPath, "server socket")
	pidFile := fs.String("pid-file", "", "PID record path (default: beside the socket)")
	timeout := fs.Duration("timeout", defaultServerTimeout, "how long to wait for the server to stop")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "noc server stop: unexpected positional arguments")
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "noc server stop: timeout must be positive")
		return 2
	}
	pidPath := *pidFile
	if pidPath == "" {
		pidPath = defaultServerPIDFile(*socket)
	}
	pid, err := stopServerProcess(ctx, *socket, pidPath, *timeout)
	if err != nil {
		return printError(stderr, err)
	}
	fmt.Fprintf(stdout, "noc server stopped (pid %d)\n", pid)
	return 0
}

func runServerRestart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc server restart", stderr)
	var values serverLaunchFlags
	values.bind(fs, paths, false)
	timeout := fs.Duration("timeout", defaultServerTimeout, "how long to wait for the old server to stop")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "noc server restart: unexpected positional arguments")
		return 2
	}
	if message := values.validate(); message != "" {
		fmt.Fprintf(stderr, "noc server restart: %s\n", message)
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "noc server restart: timeout must be positive")
		return 2
	}
	opts := values.options()
	if err := requireRestartableServer(opts.PIDFile); err != nil {
		return printError(stderr, err)
	}
	pid, err := stopServerProcess(ctx, opts.Socket, opts.PIDFile, *timeout)
	if err != nil {
		return printError(stderr, err)
	}
	fmt.Fprintf(stdout, "noc server stopped (pid %d)\n", pid)
	newPID, logPath, err := startBackgroundServer(ctx, opts)
	if err != nil {
		return printError(stderr, fmt.Errorf("server stopped but restart failed: %w", err))
	}
	fmt.Fprintf(stdout, "noc server restarted in background (pid %d)\n", newPID)
	fmt.Fprintf(stdout, "logs: %s\n", logPath)
	return 0
}

func runServerStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc server status", stderr)
	asJSON := fs.Bool("json", false, "write JSON")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	pidFile := fs.String("pid-file", "", "PID record path (default: beside the socket)")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "noc server status: unexpected positional arguments")
		return 2
	}
	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	health, err := client.Health(callCtx)
	if err != nil {
		return printServerError(stderr, err)
	}
	if *asJSON {
		return writeJSON(stdout, stderr, health)
	}
	list, err := client.ListAgents(callCtx, false)
	if err != nil {
		return printServerError(stderr, err)
	}
	pidPath := *pidFile
	if pidPath == "" {
		pidPath = defaultServerPIDFile(*socket)
	}
	var record *serverPIDRecord
	if found, err := readServerPIDFile(pidPath); err == nil && found.SocketPath == filepath.Clean(*socket) {
		record = &found
	}
	renderServerStatus(stdout, health, list.Agents, record)
	return 0
}

func formatStatusTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format(time.RFC3339)
}

func renderServerStatus(w io.Writer, health apiv1.Health, agents []apiv1.Agent, record *serverPIDRecord) {
	fmt.Fprintf(w, "noc server %s (%s)\n", health.Status, health.Version)
	fmt.Fprintf(w, "  started: %s\n", formatStatusTime(health.StartedAt))
	fmt.Fprintf(w, "  host: %s\n", health.HostID)
	if record != nil {
		fmt.Fprintf(w, "  process: pid %d (%s)\n", record.PID, dash2(record.Mode, "unknown"))
	}
	for _, p := range health.Providers {
		fmt.Fprintf(w, "\n%s: %s\n", p.Provider, p.Status)
		if p.Version != "" {
			fmt.Fprintf(w, "  version: %s\n", p.Version)
		}
		if p.DiscoverySource != "" {
			fmt.Fprintf(w, "  source: %s\n", p.DiscoverySource)
		}
		renderProviderRegistryStatus(w, p.Provider, agents)
		if p.StartupStatus != "" {
			line := "  startup: " + p.StartupStatus
			if p.StartupObservedAt != nil {
				line += " at " + formatStatusTime(*p.StartupObservedAt)
			}
			if p.StartupDetail != "" {
				line += " — " + p.StartupDetail
			}
			fmt.Fprintln(w, line)
		}
		line := fmt.Sprintf("  latest: %s at %s", p.Status, formatStatusTime(p.ObservedAt))
		if p.Detail != "" {
			line += " — " + p.Detail
		}
		fmt.Fprintln(w, line)
		if p.Provider == apiv1.ProviderCodex && (p.Status == "degraded" || p.Status == "unavailable") {
			fmt.Fprintln(w, "  hint: start or inspect the shared daemon with `codex app-server daemon start` or `codex app-server daemon version`")
		}
	}
}

func dash2(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func renderProviderRegistryStatus(w io.Writer, provider apiv1.Provider, agents []apiv1.Agent) {
	origins := map[string]int{}
	total := 0
	for _, agent := range agents {
		if agent.Provider != provider || agent.LiveState != apiv1.LiveStateLive {
			continue
		}
		total++
		origin := agent.Origin
		switch origin {
		case "":
			origin = "unknown"
		case "reconcile":
			origin = "snapshot"
		}
		origins[origin]++
	}
	line := fmt.Sprintf("  registry: %d live agent", total)
	if total != 1 {
		line += "s"
	}
	if total > 0 {
		names := make([]string, 0, len(origins))
		for name := range origins {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s %d", name, origins[name]))
		}
		line += " (origin: " + strings.Join(parts, ", ") + ")"
	}
	fmt.Fprintln(w, line)
}
