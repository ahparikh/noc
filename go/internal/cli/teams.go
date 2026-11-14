package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"noc.local/noc/internal/attach"
	"noc.local/noc/internal/config"
	"noc.local/noc/internal/localapi"
	"noc.local/noc/internal/proclineage"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	teamPrefix        = "team:"
	noTeamLabel       = "(none)"
	spawnStartTimeout = 60 * time.Second
	// How long, and how often, noc looks for a session Claude just started.
	spawnRosterInterval = 250 * time.Millisecond
	// Claude sets this in every command a session runs.
	claudeSessionEnvironment = "CLAUDE_CODE_SESSION_ID"
	teamNameRule             = "a team name is 1 to 48 characters from A-Za-z0-9._-, not starting with - or ., and not none"
	teamUsage                = `usage: noc team set <name> [<agent-id>...]
       noc team clear [<agent-id>...]
       noc team lead <name> [<agent-id>]

With no agent ID, the command acts on the agent it is run inside.`
	spawnUsage = "usage: noc spawn [--provider claude] [--team name|none] [--name name] [--dir path] <prompt> [-- <provider arguments>]"
)

// spawnRosterWait is how long noc looks for a session Claude just started; a
// variable so tests can shorten it.
var spawnRosterWait = 10 * time.Second

// providerProcessID finds the provider process this command runs under; a
// variable so tests can replace it.
var providerProcessID = func() int { return proclineage.ProviderProcessID("claude", "codex") }

// ---- the team filter ------------------------------------------------------

// teamFilter is the value of --team: unset, a team's name, or "none".
type teamFilter string

func parseTeamFilter(value string) (teamFilter, error) {
	if value == "" || value == apiv1.NoTeam || apiv1.ValidTeamName(value) {
		return teamFilter(value), nil
	}
	return "", fmt.Errorf("invalid team %q: %s", value, teamNameRule)
}

func (f teamFilter) keeps(agent apiv1.Agent) bool {
	switch f {
	case "":
		return true
	case apiv1.NoTeam:
		return agent.Team == ""
	default:
		return agent.Team == string(f)
	}
}

func filterTeam(agents []apiv1.Agent, filter teamFilter) []apiv1.Agent {
	if filter == "" {
		return agents
	}
	kept := make([]apiv1.Agent, 0, len(agents))
	for _, agent := range agents {
		if filter.keeps(agent) {
			kept = append(kept, agent)
		}
	}
	return kept
}

// anyInTeam reports whether a table of these agents needs a TEAM column.
func anyInTeam(agents []apiv1.Agent) bool {
	for _, agent := range agents {
		if agent.Team != "" {
			return true
		}
	}
	return false
}

// teamCell is an agent's TEAM column: the name, with * for the lead.
func teamCell(agent apiv1.Agent) string {
	switch {
	case agent.Team == "":
		return "-"
	case agent.Lead:
		return agent.Team + "*"
	default:
		return agent.Team
	}
}

// ---- the calling agent ----------------------------------------------------

// callerAgentID names the agent this command runs inside, or "" when it runs
// outside one. In order: the agent whose Claude session ID is in
// CLAUDE_CODE_SESSION_ID, which Claude sets for the commands a session runs
// and which is therefore the session nearest to this command; then
// NOC_AGENT_ID, which `noc attach` sets; then the agent whose provider
// process is an ancestor of this process.
func callerAgentID(ctx context.Context, client *localapi.Client) (string, error) {
	session := strings.TrimSpace(os.Getenv(claudeSessionEnvironment))
	named := strings.TrimSpace(os.Getenv(agentIDEnvironment))
	pid := 0
	if named == "" {
		pid = providerProcessID()
	}
	if session == "" && pid <= 0 {
		return named, nil
	}
	list, err := client.ListAgents(ctx, true)
	if err != nil {
		return "", err
	}
	if session != "" {
		for _, agent := range list.Agents {
			if agent.Provider == apiv1.ProviderClaude && agent.NativeSessionID == session {
				return agent.ID, nil
			}
		}
	}
	if named != "" {
		return named, nil
	}
	for _, agent := range list.Agents {
		if pid > 0 && agent.ProcessID == pid && agent.LiveState != apiv1.LiveStateNotLive {
			return agent.ID, nil
		}
	}
	return "", nil
}

// ---- noc team -------------------------------------------------------------

func runTeam(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, teamUsage)
		return 2
	}
	verb := args[0]
	fs := newFlagSet("noc team "+verb, stderr)
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args[1:]); stop {
		return code
	}
	rest := fs.Args()

	team := ""
	switch verb {
	case "set", "lead":
		if len(rest) == 0 || (verb == "lead" && len(rest) > 2) {
			fmt.Fprintln(stderr, teamUsage)
			return 2
		}
		team, rest = rest[0], rest[1:]
		if !apiv1.ValidTeamName(team) {
			fmt.Fprintf(stderr, "noc team %s: invalid team %q: %s\n", verb, team, teamNameRule)
			return 2
		}
	case "clear":
	default:
		fmt.Fprintf(stderr, "noc team: unknown command %q\n%s\n", verb, teamUsage)
		return 2
	}

	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	ids := rest
	if len(ids) == 0 {
		caller, err := callerAgentID(callCtx, client)
		if err != nil {
			return printServerError(stderr, err)
		}
		if caller == "" {
			fmt.Fprintf(stderr, "noc team %s: no agent ID was given and this is not running inside an agent\n", verb)
			return 2
		}
		ids = []string{caller}
	}

	code := 0
	for _, id := range ids {
		switch verb {
		case "set":
			if _, err := client.SetAgentTeam(callCtx, id, team); err != nil {
				printServerError(stderr, err)
				code = 1
				continue
			}
			fmt.Fprintf(stdout, "%s is in team %s\n", id, team)
		case "clear":
			if _, err := client.SetAgentTeam(callCtx, id, ""); err != nil {
				printServerError(stderr, err)
				code = 1
				continue
			}
			fmt.Fprintf(stdout, "%s is in no team\n", id)
		case "lead":
			agent, err := client.GetAgent(callCtx, id)
			if err != nil {
				return printServerError(stderr, err)
			}
			if agent.Team != team {
				fmt.Fprintf(stderr, "noc: %s is not in team %s\n", id, team)
				return 1
			}
			if _, err := client.SetTeamLead(callCtx, id, team); err != nil {
				return printServerError(stderr, err)
			}
			fmt.Fprintf(stdout, "%s leads team %s\n", id, team)
		}
	}
	return code
}

// ---- noc teams ------------------------------------------------------------

// summarizeTeams counts agents by team: every team by name, then, when any
// agent is in no team, a last row with an empty name.
func summarizeTeams(agents []apiv1.Agent) []apiv1.Team {
	byName := map[string]*apiv1.Team{}
	for _, agent := range agents {
		team, ok := byName[agent.Team]
		if !ok {
			team = &apiv1.Team{Name: agent.Team}
			byName[agent.Team] = team
		}
		team.Agents++
		switch agent.Bucket {
		case apiv1.BucketBlocked:
			team.Blocked++
		case apiv1.BucketWorking:
			team.Working++
		case apiv1.BucketReview:
			team.Review++
		default:
			team.Quiet++
		}
		if agent.Lead && agent.Team != "" && team.LeadID == "" {
			team.LeadID = agent.ID
			team.LeadName = agentDisplayName(agent)
		}
	}
	teams := make([]apiv1.Team, 0, len(byName))
	for _, team := range byName {
		teams = append(teams, *team)
	}
	sort.Slice(teams, func(i, j int) bool {
		if (teams[i].Name == "") != (teams[j].Name == "") {
			return teams[j].Name == ""
		}
		return teams[i].Name < teams[j].Name
	})
	return teams
}

func renderTeams(w io.Writer, teams []apiv1.Team) {
	if len(teams) == 0 {
		fmt.Fprintln(w, "No agents.")
		return
	}
	table := newTable(w)
	fmt.Fprintln(table, "TEAM\tLEAD\tAGENTS\tBLOCKED\tWORKING\tREVIEW\tQUIET")
	for _, team := range teams {
		name, lead := team.Name, "-"
		if name == "" {
			name = noTeamLabel
		}
		if team.LeadID != "" {
			lead = team.LeadName
			if lead == "" {
				lead = team.LeadID
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%d\t%d\t%d\t%d\t%d\n", name, lead,
			team.Agents, team.Blocked, team.Working, team.Review, team.Quiet)
	}
	_ = table.Flush()
}

func runTeams(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc teams", stderr)
	asJSON := fs.Bool("json", false, "write JSON")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "noc teams: unexpected positional arguments")
		return 2
	}
	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	list, err := client.ListAgents(callCtx, true)
	if err != nil {
		return printServerError(stderr, err)
	}
	teams := summarizeTeams(list.Agents)
	if *asJSON {
		return writeJSON(stdout, stderr, apiv1.TeamList{
			SchemaVersion: apiv1.SchemaVersion,
			GeneratedAt:   list.GeneratedAt,
			Teams:         teams,
		})
	}
	renderTeams(stdout, teams)
	return 0
}

// ---- noc spawn ------------------------------------------------------------

// splitProviderArguments cuts the command line at the first "--": what is
// before it is noc's, what is after it goes to the provider unchanged.
func splitProviderArguments(args []string) (own, passed []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// claudeSpawnArguments is the command that starts a background Claude
// session. The caller's own arguments come last so that they override the
// permission mode set here.
func claudeSpawnArguments(name, prompt string, passed []string) []string {
	arguments := []string{"--bg", "--permission-mode", "auto"}
	if name != "" {
		arguments = append(arguments, "--name", name)
	}
	arguments = append(arguments, prompt)
	return append(arguments, passed...)
}

// startedClaudeSession finds, in Claude's roster, the background session
// whose job ID Claude printed when it started it. Claude chooses the session
// ID itself, so the roster is the only place to learn it.
func startedClaudeSession(output string, roster []attach.ClaudeSession) (attach.ClaudeSession, bool) {
	printed := map[string]bool{}
	for _, word := range strings.FieldsFunc(output, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) {
		printed[word] = true
	}
	for _, entry := range roster {
		id := strings.TrimSpace(entry.ID)
		if entry.Kind == "background" && id != "" && printed[id] && strings.TrimSpace(entry.SessionID) != "" {
			return entry, true
		}
	}
	return attach.ClaudeSession{}, false
}

// awaitClaudeSession reads the roster until the started session is listed.
func awaitClaudeSession(ctx context.Context, claudeBin, output string) (attach.ClaudeSession, error) {
	deadline := time.Now().Add(spawnRosterWait)
	for {
		roster, err := attach.ReadClaudeRoster(ctx, claudeBin)
		if err == nil {
			if session, ok := startedClaudeSession(output, roster); ok {
				return session, nil
			}
			err = errors.New("Claude lists no background session for it")
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return attach.ClaudeSession{}, err
		}
		time.Sleep(spawnRosterInterval)
	}
}

func runSpawn(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	own, passed := splitProviderArguments(args)
	fs := newFlagSet("noc spawn", stderr)
	providerFlag := fs.String("provider", string(apiv1.ProviderClaude), "provider: claude")
	team := fs.String("team", "", "team to join, or none; the default is the calling agent's team")
	name := fs.String("name", "", "a name for the agent")
	dir := fs.String("dir", "", "directory to start in; the default is the current one")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, own); stop {
		return code
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		fmt.Fprintln(stderr, spawnUsage)
		return 2
	}
	if _, err := parseTeamFilter(*team); err != nil {
		fmt.Fprintf(stderr, "noc spawn: %v\n", err)
		return 2
	}
	switch apiv1.Provider(*providerFlag) {
	case apiv1.ProviderClaude:
	case apiv1.ProviderCodex:
		fmt.Fprintln(stderr, "noc spawn: codex is not supported yet")
		return 1
	default:
		fmt.Fprintf(stderr, "noc spawn: unknown provider %q (want claude)\n", *providerFlag)
		return 2
	}

	directory := *dir
	if directory == "" {
		if directory, err = os.Getwd(); err != nil {
			return printError(stderr, fmt.Errorf("find the current directory: %w", err))
		}
	}
	if directory, err = filepath.Abs(directory); err != nil {
		return printError(stderr, fmt.Errorf("resolve directory: %w", err))
	}
	if info, err := os.Stat(directory); err != nil {
		return printError(stderr, fmt.Errorf("open directory %q: %w", directory, err))
	} else if !info.IsDir() {
		return printError(stderr, fmt.Errorf("%q is not a directory", directory))
	}
	binary := providerBinaries().Claude
	program, err := exec.LookPath(binary)
	if err != nil {
		return printError(stderr, fmt.Errorf("find claude executable %q: %w", binary, err))
	}

	// The server is asked first, so that nothing is started when it is down.
	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	parent, err := callerAgentID(callCtx, client)
	if err != nil {
		return printServerError(stderr, err)
	}
	if parent != "" {
		// A caller the registry no longer knows is not a parent.
		if _, err := client.GetAgent(callCtx, parent); err != nil {
			if errors.Is(err, localapi.ErrConnection) {
				return printServerError(stderr, err)
			}
			parent = ""
		}
	} else if _, err := client.Health(callCtx); err != nil {
		return printServerError(stderr, err)
	}

	startCtx, cancelStart := context.WithTimeout(ctx, spawnStartTimeout)
	defer cancelStart()
	start := exec.CommandContext(startCtx, program, claudeSpawnArguments(strings.TrimSpace(*name), prompt, passed)...)
	start.Dir = directory
	output, err := start.CombinedOutput()
	if err != nil {
		_, _ = stderr.Write(output)
		return printError(stderr, fmt.Errorf("start Claude in the background: %w", err))
	}
	session, err := awaitClaudeSession(startCtx, program, string(output))
	if err != nil {
		_, _ = stderr.Write(output)
		return printError(stderr, fmt.Errorf("Claude started a session, but noc could not identify it, so it has no team or parent: %w", err))
	}

	registerCtx, cancelRegister := context.WithTimeout(ctx, requestTimeout)
	defer cancelRegister()
	agent, err := client.RegisterSpawn(registerCtx, apiv1.AgentSpawn{
		Provider:        apiv1.ProviderClaude,
		NativeSessionID: strings.TrimSpace(session.SessionID),
		Team:            *team,
		ParentID:        parent,
		Name:            strings.TrimSpace(*name),
		Directory:       directory,
		Prompt:          prompt,
	})
	if err != nil {
		fmt.Fprintf(stderr, "noc: Claude started session %s, but it has no team or parent:\n", strings.TrimSpace(session.ID))
		return printServerError(stderr, err)
	}
	fmt.Fprintln(stdout, agent.ID)
	return 0
}
