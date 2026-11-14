package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"noc.local/noc/internal/config"
	"noc.local/noc/internal/localapi"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// searchQuery is what `noc search` filters by. Every part must hold.
type searchQuery struct {
	// Lower-cased words; each must occur somewhere in the agent's text.
	terms []string
	// Bounds on the time since the agent's last activity; zero is unset.
	olderThan time.Duration
	newerThan time.Duration
	// Lower-cased team names from "team:<name>" words; the agent must be in
	// each (so two different names match nothing). "none" is no team.
	teams []string
}

const (
	olderPrefix = "older:"
	newerPrefix = "newer:"
)

// parseAge reads an age such as "14", "14d", "2w" or "36h". A bare number
// is days.
func parseAge(value string) (time.Duration, error) {
	text := strings.ToLower(strings.TrimSpace(value))
	unit := 24 * time.Hour
	switch {
	case strings.HasSuffix(text, "w"):
		unit, text = 7*24*time.Hour, strings.TrimSuffix(text, "w")
	case strings.HasSuffix(text, "d"):
		text = strings.TrimSuffix(text, "d")
	case strings.HasSuffix(text, "h"):
		unit, text = time.Hour, strings.TrimSuffix(text, "h")
	}
	count, err := strconv.Atoi(text)
	if err != nil || count <= 0 || count > 100000 {
		return 0, fmt.Errorf("invalid age %q (want a number of days such as 14, or 14d, 2w, 36h)", value)
	}
	return time.Duration(count) * unit, nil
}

// parseSearchQuery builds the query from the flags and the words. A word
// "older:<age>" or "newer:<age>" sets a bound, so one typed line can carry
// the whole query; a word that looks like one but does not parse is matched
// as text. A flag that does not parse is an error.
func parseSearchQuery(words []string, older, newer string) (searchQuery, error) {
	var query searchQuery
	var err error
	if older != "" {
		if query.olderThan, err = parseAge(older); err != nil {
			return query, err
		}
	}
	if newer != "" {
		if query.newerThan, err = parseAge(newer); err != nil {
			return query, err
		}
	}
	for _, argument := range words {
		for _, word := range strings.Fields(strings.ToLower(argument)) {
			if rest, ok := strings.CutPrefix(word, olderPrefix); ok {
				if age, err := parseAge(rest); err == nil {
					query.olderThan = age
					continue
				}
			}
			if rest, ok := strings.CutPrefix(word, newerPrefix); ok {
				if age, err := parseAge(rest); err == nil {
					query.newerThan = age
					continue
				}
			}
			if rest, ok := strings.CutPrefix(word, teamPrefix); ok && rest != "" {
				query.teams = append(query.teams, rest)
				continue
			}
			query.terms = append(query.terms, word)
		}
	}
	if query.olderThan > 0 && query.newerThan > 0 && query.olderThan > query.newerThan {
		return query, fmt.Errorf("no agent is both older than %s and newer than %s",
			formatAge(query.olderThan), formatAge(query.newerThan))
	}
	return query, nil
}

// agentSearchText is everything about an agent that text can match.
func agentSearchText(agent apiv1.Agent) string {
	parts := []string{
		agent.ID, agent.UserName, agent.Name, agent.Team, string(agent.Provider),
		agent.NativeSessionID, agent.NativeShortID, agent.Model,
		string(agent.DisplayState), string(agent.Bucket),
		agent.Summary, agent.Attention.Detail,
	}
	if agent.InitialRequest != nil {
		parts = append(parts, agent.InitialRequest.Text)
	}
	if agent.Workspace != nil {
		parts = append(parts, agent.Workspace.Path, agent.Workspace.RootPath, agent.Workspace.Branch)
	}
	return strings.ToLower(strings.Join(parts, "\n"))
}

func (q searchQuery) matches(agent apiv1.Agent, now time.Time) bool {
	age := now.Sub(agentRecency(agent))
	if q.olderThan > 0 && age < q.olderThan {
		return false
	}
	if q.newerThan > 0 && age > q.newerThan {
		return false
	}
	for _, team := range q.teams {
		if team == apiv1.NoTeam {
			team = ""
		}
		if strings.ToLower(agent.Team) != team {
			return false
		}
	}
	if len(q.terms) == 0 {
		return true
	}
	text := agentSearchText(agent)
	for _, term := range q.terms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func searchAgents(agents []apiv1.Agent, query searchQuery, now time.Time) []apiv1.Agent {
	kept := make([]apiv1.Agent, 0, len(agents))
	for _, agent := range agents {
		if query.matches(agent, now) {
			kept = append(kept, agent)
		}
	}
	return kept
}

// formatAge is the largest whole unit: 3w, 12d, 5h, 40m.
func formatAge(age time.Duration) string {
	switch {
	case age < 0:
		return "0m"
	case age >= 14*24*time.Hour:
		return fmt.Sprintf("%dw", int(age/(7*24*time.Hour)))
	case age >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
	case age >= time.Hour:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	}
}

func renderSearchResults(w io.Writer, agents []apiv1.Agent, now time.Time) {
	if len(agents) == 0 {
		fmt.Fprintln(w, "No agents match.")
		return
	}
	table := newTable(w)
	teams := anyInTeam(agents)
	if teams {
		fmt.Fprintln(table, "ID\tTEAM\tPROVIDER\tBUCKET\tSTATE\tAGE\tWORKSPACE\tNAME")
	} else {
		fmt.Fprintln(table, "ID\tPROVIDER\tBUCKET\tSTATE\tAGE\tWORKSPACE\tNAME")
	}
	for _, agent := range agents {
		fmt.Fprintf(table, "%s\t", agent.ID)
		if teams {
			fmt.Fprintf(table, "%s\t", teamCell(agent))
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", agent.Provider, agent.Bucket,
			agent.DisplayState, formatAge(now.Sub(agentRecency(agent))),
			dash(agentWorkspacePath(agent)), dash(agentDisplayName(agent)))
	}
	_ = table.Flush()
}

// runSearch lists every agent, live or retained, that matches the query.
// Its JSON is the same document `noc ls --json` writes.
func runSearch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc search", stderr)
	asJSON := fs.Bool("json", false, "write JSON")
	older := fs.String("older-than", "", "last active at least this long ago: days, or 14d, 2w, 36h")
	newer := fs.String("newer-than", "", "last active within this long: days, or 14d, 2w, 36h")
	bucketFlag := fs.String("bucket", "", "comma-separated buckets: blocked, working, review, quiet")
	teamFlag := fs.String("team", "", "keep one team's agents, or none for agents in no team")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	buckets, err := parseBuckets(*bucketFlag)
	if err != nil {
		fmt.Fprintf(stderr, "noc search: %v\n", err)
		return 2
	}
	team, err := parseTeamFilter(*teamFlag)
	if err != nil {
		fmt.Fprintf(stderr, "noc search: %v\n", err)
		return 2
	}
	query, err := parseSearchQuery(fs.Args(), *older, *newer)
	if err != nil {
		fmt.Fprintf(stderr, "noc search: %v\n", err)
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
	now := time.Now()
	list.Agents = searchAgents(filterTeam(filterBuckets(list.Agents, buckets), team), query, now)
	sortByBucket(list.Agents)
	if *asJSON {
		return writeJSON(stdout, stderr, list)
	}
	renderSearchResults(stdout, list.Agents, now)
	return 0
}
