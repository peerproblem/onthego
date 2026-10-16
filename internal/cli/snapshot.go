package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/jbaehova/onthego/internal/identity"
	"github.com/jbaehova/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

func (a *app) snapshotCommand() *cobra.Command {
	c := &cobra.Command{Use: "snapshot", Short: "Create or restore an encrypted offline handoff package"}
	var output, recipient string
	var include, secrets []string
	var dry bool
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
		m, err := syncer.Export(c.Context(), root, output, recipient, a.version, include, secrets)
		if err != nil {
			return err
		}
		return a.print(c, map[string]any{"schema_version": 2, "snapshot_id": m.SnapshotID, "output": output}, "Created encrypted snapshot at "+output+"\nFor another machine, pack with its onthego identity encryption recipient.")
	}}
	pack.Flags().StringVarP(&output, "output", "o", "", "new encrypted .otg package path")
	pack.Flags().StringVar(&recipient, "recipient", "", "destination age public key; defaults to this machine")
	pack.Flags().StringSliceVar(&include, "include", nil, "additional project-relative ignored path or glob")
	pack.Flags().StringSliceVar(&secrets, "include-secret", nil, "explicit project-relative secret file")
	pack.Flags().BoolVar(&dry, "dry-run", false, "preview selected files without creating a snapshot")
	var destination, signer string
	var replace bool
	restore := &cobra.Command{Use: "restore <snapshot.otg>", Args: cobra.ExactArgs(1), Short: "Verify, restore, and import native sessions from an offline snapshot", RunE: func(c *cobra.Command, args []string) error {
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
		result, err := syncer.Apply(c.Context(), args[0], root, signer, replace)
		if err != nil {
			return err
		}
		return a.print(c, result, renderTransfer("Restored", result))
	}}
	restore.Flags().StringVarP(&destination, "output", "o", "", "destination project directory")
	restore.Flags().StringVar(&signer, "signing-public", "", "trusted sender public signing key; defaults to this machine")
	restore.Flags().BoolVar(&replace, "replace", false, "replace a conflicting destination while preserving a backup")
	c.AddCommand(pack, restore)
	return c
}

// The peer protocol runs only as an SSH/local subprocess, never as a listening server.
func (a *app) peerCommand() *cobra.Command {
	c := &cobra.Command{Use: "peer", Hidden: true, Short: "Private subprocess protocol for direct device transfer"}
	c.AddCommand(&cobra.Command{Use: "identity", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		v, err := localIdentity()
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(v)
	}})
	var source, recipient string
	export := &cobra.Command{Use: "export", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if !filepath.IsAbs(source) || recipient == "" {
			return errors.New("absolute --root and --recipient are required")
		}
		path, cleanup, err := tempPackage()
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err = syncer.Export(c.Context(), source, path, recipient, a.version, nil, nil); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(c.OutOrStdout(), f)
		return err
	}}
	export.Flags().StringVar(&source, "root", "", "absolute project root")
	export.Flags().StringVar(&recipient, "recipient", "", "receiver age recipient")
	var destination, signer string
	var replace bool
	apply := &cobra.Command{Use: "apply", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if !filepath.IsAbs(destination) || signer == "" {
			return errors.New("absolute --root and --signing-public are required")
		}
		path, cleanup, err := tempPackage()
		if err != nil {
			return err
		}
		defer cleanup()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, c.InOrStdin())
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		result, err := syncer.Apply(c.Context(), path, destination, signer, replace)
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(result)
	}}
	apply.Flags().StringVar(&destination, "root", "", "absolute destination root")
	apply.Flags().StringVar(&signer, "signing-public", "", "trusted sender public key")
	apply.Flags().BoolVar(&replace, "replace", false, "preserve a backup then replace conflicting destination")
	var statusRoot string
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if !filepath.IsAbs(statusRoot) {
			return errors.New("absolute --root is required")
		}
		result, err := syncer.Status(c.Context(), statusRoot)
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(result)
	}}
	status.Flags().StringVar(&statusRoot, "root", "", "absolute project root")
	var ackRoot, snapshotID string
	ack := &cobra.Command{Use: "ack", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if !filepath.IsAbs(ackRoot) || snapshotID == "" {
			return errors.New("absolute --root and --snapshot are required")
		}
		if err := syncer.MarkSynced(c.Context(), ackRoot, snapshotID); err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(map[string]any{"schema_version": 2, "snapshot_id": snapshotID, "acknowledged": true})
	}}
	ack.Flags().StringVar(&ackRoot, "root", "", "absolute source root")
	ack.Flags().StringVar(&snapshotID, "snapshot", "", "successfully received snapshot ID")
	c.AddCommand(export, apply, status, ack)
	return c
}
