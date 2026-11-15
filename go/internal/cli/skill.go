package cli

import (
	"context"
	"fmt"
	"io"

	"noc.local/noc/internal/config"
	"noc.local/noc/internal/localapi"
	"noc.local/noc/internal/skill"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	setupSkillWord  = "skill"
	setupSkillUsage = "usage: noc setup skill [--check|--uninstall] [--json] [claude|codex]"
)

// skillResult is one provider's outcome of `noc setup skill`.
type skillResult struct {
	Provider apiv1.Provider `json:"provider"`
	Path     string         `json:"path"`
	Status   skill.Status   `json:"status,omitempty"`
	Changed  bool           `json:"changed,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// skillReport is what `noc setup skill --json` writes.
type skillReport struct {
	SchemaVersion int           `json:"schema_version"`
	Action        string        `json:"action"`
	Results       []skillResult `json:"results"`
}

// failed reports whether the command should exit 1: any error, or, for a
// check, any provider whose skill is not this binary's.
func (r skillReport) failed() bool {
	for _, result := range r.Results {
		if result.Error != "" || (r.Action == "check" && result.Status != skill.StatusCurrent) {
			return true
		}
	}
	return false
}

// withoutSkillWord removes the first bare "skill" from the arguments of
// `noc setup` and reports whether it was there, so the word may come before
// or after the flags.
func withoutSkillWord(args []string) ([]string, bool) {
	for i, arg := range args {
		if arg == setupSkillWord {
			rest := append([]string{}, args[:i]...)
			return append(rest, args[i+1:]...), true
		}
	}
	return args, false
}

func runSetupSkill(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("noc setup skill", stderr)
	check := fs.Bool("check", false, "report whether the skill is installed, without changing anything")
	uninstall := fs.Bool("uninstall", false, "remove the skill noc installed")
	asJSON := fs.Bool("json", false, "write JSON")
	// Flags may follow the provider, as in `noc setup skill claude --check`.
	var providers []apiv1.Provider
	for len(args) > 0 {
		if code, stop := parseFlags(fs, args); stop {
			return code
		}
		if fs.NArg() == 0 {
			break
		}
		switch fs.Arg(0) {
		case "claude":
			providers = append(providers, apiv1.ProviderClaude)
		case "codex":
			providers = append(providers, apiv1.ProviderCodex)
		default:
			fmt.Fprintf(stderr, "noc setup skill: unsupported provider %q\n", fs.Arg(0))
			return 2
		}
		args = fs.Args()[1:]
	}
	if *check && *uninstall {
		fmt.Fprintln(stderr, "noc setup skill: --check and --uninstall are mutually exclusive")
		return 2
	}
	if len(providers) > 1 {
		fmt.Fprintln(stderr, setupSkillUsage)
		return 2
	}
	if len(providers) == 0 {
		providers = []apiv1.Provider{apiv1.ProviderClaude, apiv1.ProviderCodex}
	}
	paths, err := skill.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}

	report := skillReport{SchemaVersion: apiv1.SchemaVersion, Action: "install"}
	switch {
	case *check:
		report.Action = "check"
	case *uninstall:
		report.Action = "uninstall"
	}
	for _, provider := range providers {
		result := skillResult{Provider: provider, Path: paths.For(provider)}
		var err error
		switch report.Action {
		case "install":
			result.Changed, err = skill.Install(result.Path)
		case "uninstall":
			result.Changed, err = skill.Uninstall(result.Path)
		}
		if err == nil {
			result.Status, err = skill.Inspect(result.Path)
		}
		if err != nil {
			result.Error = err.Error()
		}
		report.Results = append(report.Results, result)
	}

	if *asJSON {
		if code := writeJSON(stdout, stderr, report); code != 0 {
			return code
		}
	} else {
		renderSkillReport(stdout, report)
	}
	if report.failed() {
		return 1
	}
	return 0
}

func renderSkillReport(w io.Writer, report skillReport) {
	fmt.Fprintf(w, "noc setup skill %s\n", report.Action)
	for _, result := range report.Results {
		p := result.Provider
		switch {
		case result.Error != "":
			fmt.Fprintf(w, "  %s: error — %s (%s)\n", p, result.Error, result.Path)
		case report.Action == "uninstall" && result.Changed:
			fmt.Fprintf(w, "  %s: removed %s\n", p, result.Path)
		case report.Action == "uninstall":
			fmt.Fprintf(w, "  %s: no noc skill found at %s\n", p, result.Path)
		case report.Action == "install" && result.Changed:
			fmt.Fprintf(w, "  %s: installed in %s\n", p, result.Path)
		case report.Action == "install":
			fmt.Fprintf(w, "  %s: already installed in %s\n", p, result.Path)
		case result.Status == skill.StatusCurrent:
			fmt.Fprintf(w, "  %s: installed\n", p)
		case result.Status == skill.StatusOutdated:
			fmt.Fprintf(w, "  %s: outdated; run noc setup skill to update %s\n", p, result.Path)
		case result.Status == skill.StatusForeign:
			fmt.Fprintf(w, "  %s: %s was not installed by noc\n", p, result.Path)
		default:
			fmt.Fprintf(w, "  %s: not installed; run noc setup skill\n", p)
		}
	}
}

// ---- noc whoami -----------------------------------------------------------

// runWhoami shows the agent this command is run inside.
func runWhoami(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, err := config.ResolvePaths()
	if err != nil {
		return printError(stderr, err)
	}
	fs := newFlagSet("noc whoami", stderr)
	asJSON := fs.Bool("json", false, "write JSON")
	socket := fs.String("socket", paths.SocketPath, "server socket")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: noc whoami [--json]")
		return 2
	}
	client := localapi.NewClient(*socket)
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	id, err := callerAgentID(callCtx, client)
	if err != nil {
		return printServerError(stderr, err)
	}
	if id == "" {
		fmt.Fprintln(stderr, "noc whoami: this is not running inside an agent that noc knows")
		return 1
	}
	agent, err := client.GetAgent(callCtx, id)
	if err != nil {
		return printServerError(stderr, err)
	}
	if *asJSON {
		return writeJSON(stdout, stderr, agent)
	}
	renderAgent(stdout, agent)
	return 0
}
