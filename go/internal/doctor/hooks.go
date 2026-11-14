package doctor

import (
	"fmt"
	"strings"

	"noc.local/noc/internal/hookconfig"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// hookCheck inspects provider hook configuration without modifying it.
func hookCheck(p apiv1.Provider, opts Options) Check {
	id := "hooks." + string(p)
	paths := hookconfig.Paths{
		ClaudeSettings: filepathJoin(opts.ClaudeConfigDir, "settings.json"),
		CodexHooks:     filepathJoin(opts.CodexHome, "hooks.json"),
		CodexConfig:    filepathJoin(opts.CodexHome, "config.toml"),
	}
	insp, err := hookconfig.Inspect(p, opts.Executable, paths)
	if err != nil {
		return Check{
			ID:          id,
			Status:      StatusWarning,
			Summary:     fmt.Sprintf("%s hooks are not fully configured", p),
			Detail:      err.Error(),
			Remediation: hookRemediation(p, opts.Executable),
		}
	}
	check := Check{ID: id, Detail: hookDetail(p, insp)}
	if insp.Complete() {
		check.Status = StatusPass
		check.Summary = fmt.Sprintf("%s hooks cover the registry lifecycle", p)
		return check
	}
	check.Status = StatusWarning
	check.Summary = fmt.Sprintf("%s hooks are not fully configured", p)
	check.Remediation = hookRemediation(p, opts.Executable)
	return check
}

func hookRemediation(p apiv1.Provider, exe string) string {
	return fmt.Sprintf("Run `noc setup %s`; expected executable: %s", p, exe)
}

func hookDetail(p apiv1.Provider, insp hookconfig.Inspection) string {
	files := "none"
	if len(insp.Files) > 0 {
		files = strings.Join(insp.Files, ", ")
	}
	parts := []string{
		"inspected " + strings.Join(insp.Paths, ", "),
		"hook files: " + files,
	}
	if p == apiv1.ProviderCodex {
		parts = append(parts, "configuration check only; Codex trust approval is not externally verifiable")
	}
	if len(insp.Errors) > 0 {
		parts = append(parts, "errors: "+strings.Join(insp.Errors, "; "))
	}
	if insp.HooksDisabled {
		parts = append(parts, "Codex hooks are disabled in [features]")
	}
	if len(insp.MissingEvents) > 0 {
		parts = append(parts, "missing events: "+strings.Join(insp.MissingEvents, ", "))
	} else if len(insp.MismatchedExecutableEvents) > 0 {
		parts = append(parts, "events not bound to this executable: "+strings.Join(insp.MismatchedExecutableEvents, ", "))
	}
	return strings.Join(parts, "; ")
}
