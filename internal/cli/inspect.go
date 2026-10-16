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
		roots, err := agentsessions.DefaultRoots()
		if err != nil {
			return err
		}
		switch cfg.Agent {
		case "none":
			roots = agentsessions.Roots{}
		case "codex":
			roots.ClaudeHome = ""
		case "claude":
			roots.CodexHome = ""
		}
		sessions, err := agentsessions.Discover(root, roots)
		if err != nil {
			return err
		}
		var out strings.Builder
		fmt.Fprintf(&out, "Project: %s\n%s\n", root, renderPreview(files, ""))
		fmt.Fprintf(&out, "Agent sessions: %d\n", len(sessions))
		for _, s := range sessions {
			fmt.Fprintf(&out, "  %s %s\n", s.Agent, s.ID)
		}
		out.WriteString("Ignored files need include rules. Secrets need --include-secret; agent authentication is never imported.")
		return a.print(c, map[string]any{"schema_version": 2, "project_id": cfg.ProjectID, "files": files, "sessions": sessions}, out.String())
	}}
}
