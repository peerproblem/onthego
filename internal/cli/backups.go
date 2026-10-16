package cli

import (
	"fmt"
	"strings"

	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/restore"
	"github.com/peerproblem/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

func (a *app) backupsCommand() *cobra.Command {
	c := &cobra.Command{Use: "backups", Short: "Inspect or prune Git-object workspace backups"}
	c.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		root, err := loadGitProject(c.Context())
		if err != nil {
			return err
		}
		backups, err := restore.Backups(c.Context(), root)
		if err != nil {
			return err
		}
		var out strings.Builder
		for _, backup := range backups {
			fmt.Fprintf(&out, "%s\t%s\t%s\n", terminalValue(backup.Ref), backup.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), terminalValue(backup.Branch))
		}
		if len(backups) == 0 {
			out.WriteString("No workspace backups.")
		}
		return a.print(c, backups, strings.TrimSpace(out.String()))
	}})
	var keep int
	prune := &cobra.Command{Use: "prune", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if keep < 0 {
			return otgerror.New(otgerror.CodeInput, "backup retention must be zero or greater")
		}
		root, err := loadGitProject(c.Context())
		if err != nil {
			return err
		}
		if err = syncer.PruneBackups(c.Context(), root, keep); err != nil {
			return err
		}
		return a.print(c, map[string]int{"kept": keep}, fmt.Sprintf("Retained the latest %d workspace backups.", keep))
	}}
	prune.Flags().IntVar(&keep, "keep", restore.DefaultBackupLimit, "number of most recent backups to retain")
	c.AddCommand(prune)
	return c
}
func (a *app) undoCommand() *cobra.Command {
	return &cobra.Command{Use: "undo [backup-ref]", Args: cobra.MaximumNArgs(1), Short: "Restore a Git-object workspace backup in place", RunE: func(c *cobra.Command, args []string) error {
		root, err := loadGitProject(c.Context())
		if err != nil {
			return err
		}
		ref := ""
		if len(args) > 0 {
			ref = args[0]
		}
		result, err := syncer.Undo(c.Context(), root, ref)
		if err != nil {
			return err
		}
		return a.print(c, result, fmt.Sprintf("Restored workspace backup in %s.", terminalValue(root)))
	}}
}
