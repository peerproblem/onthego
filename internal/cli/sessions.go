package cli

import (
	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

func (a *app) sessionsCommand() *cobra.Command {
	c := &cobra.Command{Use: "sessions", Short: "Recover native agent histories from a preserved handoff"}
	var policy string
	recover := &cobra.Command{Use: "recover", Args: cobra.NoArgs, Short: "Retry only native session import without changing workspace files", RunE: func(c *cobra.Command, _ []string) error {
		if err := validateSessionPolicy(policy); err != nil {
			return err
		}
		root, err := loadGitProject(c.Context())
		if err != nil {
			return err
		}
		result, err := syncer.RecoverSessions(c.Context(), root, agentsessions.ImportOptions{ConflictPolicy: policy})
		if err != nil {
			return err
		}
		return a.print(c, result, renderTransfer("Recovered sessions", result))
	}}
	addSessionPolicyFlags(recover, &policy)
	c.AddCommand(recover)
	return c
}
