package cli

import (
	"fmt"
	"strings"

	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

func (a *app) inspectCommand() *cobra.Command {
	return &cobra.Command{Use: "inspect", Args: cobra.NoArgs, Short: "Preview selected extra files and project-scoped native sessions", RunE: func(c *cobra.Command, _ []string) error {
		root, cfg, err := loadProject()
		if err != nil {
			return err
		}
		files, err := syncer.Preview(c.Context(), root, nil, nil)
		if err != nil {
			return err
		}
		roots, err := syncer.SessionRoots(cfg)
		if err != nil {
			return err
		}
		discovery, err := agentsessions.DiscoverWithOptions(root, roots, agentsessions.Selection{})
		if err != nil {
			return err
		}
		warnings, err := syncer.PreviewWarnings(root)
		if err != nil {
			return err
		}
		sessions := discovery.Sessions
		var out strings.Builder
		fmt.Fprintf(&out, "Project: %s\n%s\n", terminalValue(root), renderPreview(files, ""))
		fmt.Fprintf(&out, "Agent sessions: %d\n", len(sessions))
		for _, s := range sessions {
			fmt.Fprintf(&out, "  %s %s\n", s.Agent, terminalValue(s.ID))
		}
		for _, warning := range append(warnings, discovery.Warnings...) {
			fmt.Fprintf(&out, "Warning: %s\n", terminalValue(warning))
		}
		out.WriteString("Ignored files need include rules. Secrets need --include-secret; agent authentication is never imported.")
		return a.print(c, map[string]any{"schema_version": 2, "project_id": cfg.ProjectID, "files": files, "sessions": sessions, "session_warnings": discovery.Warnings, "warnings": warnings}, out.String())
	}}
}
