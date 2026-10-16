package cli

import (
	"errors"
	"path/filepath"

	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

func (a *app) snapshotCommand() *cobra.Command {
	c := &cobra.Command{Use: "snapshot", Short: "Create or restore an encrypted offline handoff package"}
	var output, recipient string
	var include, secrets []string
	var dry bool
	var selection agentsessions.Selection
	pack := &cobra.Command{Use: "pack", Args: cobra.NoArgs, Short: "Capture Git state and native agent histories into an encrypted file", RunE: func(c *cobra.Command, _ []string) error {
		root, _, err := loadProject()
		if err != nil {
			return err
		}
		if dry {
			files, err := syncer.Preview(c.Context(), root, include, secrets)
			if err != nil {
				return err
			}
			return a.print(c, map[string]any{"dry_run": true, "selected_files": files}, renderPreview(files, ""))
		}
		if output == "" {
			return errors.New("--output is required; choose a new .otg path outside the project")
		}
		output, err = filepath.Abs(output)
		if err != nil {
			return err
		}
		if recipient == "" {
			k, err := identity.LoadOrCreate()
			if err != nil {
				return err
			}
			recipient = k.AgeRecipient
		}
		a.phase(c, "Capturing Git state and selected sessions; encrypting offline package")
		m, err := syncer.ExportWithOptions(c.Context(), root, output, recipient, a.version, syncer.ExportOptions{Include: include, IncludeSecret: secrets, Selection: selection})
		if err != nil {
			return err
		}
		return a.print(c, map[string]any{"schema_version": 2, "snapshot_id": m.SnapshotID, "output": output}, "Created encrypted snapshot at "+output+"\nFor another machine, pack with its onthego identity encryption recipient.")
	}}
	addSelectionFlags(pack, &selection)
	pack.Flags().StringVarP(&output, "output", "o", "", "new encrypted .otg package path")
	pack.Flags().StringVar(&recipient, "recipient", "", "destination age public key; defaults to this machine")
	pack.Flags().StringSliceVar(&include, "include", nil, "additional project-relative ignored path or glob")
	pack.Flags().StringSliceVar(&secrets, "include-secret", nil, "explicit project-relative secret file")
	pack.Flags().BoolVar(&dry, "dry-run", false, "preview selected files without creating a snapshot")
	var destination, signer string
	var replace bool
	var policy string
	restore := &cobra.Command{Use: "restore <snapshot.otg>", Args: cobra.ExactArgs(1), Short: "Verify, restore, and import native sessions from an offline snapshot", RunE: func(c *cobra.Command, args []string) error {
		if err := validateSessionPolicy(policy); err != nil {
			return err
		}
		if destination == "" {
			return errors.New("--output is required")
		}
		if signer == "" {
			k, err := identity.LoadOrCreate()
			if err != nil {
				return err
			}
			signer = k.SigningPublic
		}
		root, err := filepath.Abs(destination)
		if err != nil {
			return err
		}
		a.phase(c, "Verifying offline package; applying workspace and sessions")
		result, err := syncer.ApplyWithOptions(c.Context(), args[0], root, signer, syncer.ApplyOptions{Replace: replace, Sessions: agentsessions.ImportOptions{ConflictPolicy: policy}})
		if err != nil {
			return err
		}
		return a.print(c, result, renderTransfer("Restored", result))
	}}
	restore.Flags().StringVar(&policy, "sessions", "keep-both", "divergent session policy: skip, theirs, or keep-both")
	restore.Flags().StringVarP(&destination, "output", "o", "", "destination project directory")
	restore.Flags().StringVar(&signer, "signing-public", "", "trusted sender public signing key; defaults to this machine")
	restore.Flags().BoolVar(&replace, "replace", false, "replace a conflicting destination while preserving a backup")
	c.AddCommand(pack, restore)
	return c
}
