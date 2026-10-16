package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

func tempPackage() (string, func(), error) {
	base, err := config.DataRoot()
	if err != nil {
		return "", nil, err
	}
	dir, err := fsx.TempDir(base, "transfer-")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, "snapshot.otg"), func() { fsx.ReleaseTemp(dir) }, nil
}
func (a *app) phase(c *cobra.Command, message string) {
	if a.verbose {
		fmt.Fprintln(c.ErrOrStderr(), message)
	}
}
func addSelectionFlags(c *cobra.Command, selection *agentsessions.Selection) {
	c.Flags().StringSliceVar(&selection.IDs, "session", nil, "native session ID to carry (repeatable)")
	c.Flags().BoolVar(&selection.All, "all-sessions", false, "carry all project sessions instead of recent sessions")
	c.Flags().IntVar(&selection.RecentDays, "session-days", 7, "carry sessions active in the last N days")
	c.MarkFlagsMutuallyExclusive("session", "all-sessions")
}
func sessionArgs(selection agentsessions.Selection) []string {
	var args []string
	for _, id := range selection.IDs {
		args = append(args, "--session", id)
	}
	if selection.All {
		args = append(args, "--all-sessions")
	}
	if selection.RecentDays != 0 {
		args = append(args, "--session-days", fmt.Sprint(selection.RecentDays))
	}
	return args
}
func (a *app) pushCommand() *cobra.Command {
	var include, secrets []string
	var replace, dry bool
	var selection agentsessions.Selection
	var policy string
	c := &cobra.Command{Use: "push <device>", Aliases: []string{"pass"}, Args: cobra.ExactArgs(1), Short: "Hand off workspace changes and native agent sessions to a paired device", RunE: func(c *cobra.Command, args []string) error {
		if err := validateSessionPolicy(policy); err != nil {
			return err
		}
		root, cfg, err := loadProject()
		if err != nil {
			return err
		}
		d, err := device(args[0])
		if err != nil {
			return err
		}
		d, err = resolveDevicePath(args[0], d, root, cfg)
		if err != nil {
			return err
		}
		if d.LocalPath != "" && filepath.Clean(d.LocalPath) == filepath.Clean(root) {
			return errors.New("source and destination are the same project directory")
		}
		if dry {
			files, err := syncer.Preview(c.Context(), root, include, secrets)
			if err != nil {
				return err
			}
			return a.print(c, map[string]any{"dry_run": true, "selected_files": files, "device": args[0]}, renderPreview(files, "Destination: "+args[0]))
		}
		files, err := syncer.Preview(c.Context(), root, include, secrets)
		if err != nil {
			return err
		}
		var excluded []string
		for _, file := range files {
			if file.Excluded {
				excluded = append(excluded, file.RelativePath)
			}
		}
		a.phase(c, "Checking peer protocol and current handoff state")
		if _, err = negotiatePeer(c.Context(), d); err != nil {
			return err
		}
		var remote syncer.StatusInfo
		if err = peerJSON(c.Context(), d, []string{"peer", "status", "--root", devicePath(d)}, nil, &remote); err != nil {
			return err
		}
		if remote.SnapshotID != "" {
			if err = syncer.ReconcilePending(c.Context(), root, remote.SnapshotID, args[0]); err != nil {
				return err
			}
		}
		path, cleanup, err := tempPackage()
		if err != nil {
			return err
		}
		defer cleanup()
		a.phase(c, "Capturing Git state and selected sessions; encrypting package")
		m, err := syncer.ExportWithOptions(c.Context(), root, path, d.AgeRecipient, a.version, syncer.ExportOptions{Include: include, IncludeSecret: secrets, Peer: args[0], RecipientTips: remote.GitTips, Selection: selection})
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		keys, err := identity.LoadOrCreate()
		if err != nil {
			return err
		}
		remoteArgs := []string{"peer", "apply", "--root", devicePath(d), "--from", identityName(keys.SigningPublic), "--sessions", policy}
		if replace {
			remoteArgs = append(remoteArgs, "--replace")
		}
		a.phase(c, "Transferring encrypted package; peer verifying and applying handoff")
		var result syncer.Result
		if err = peerJSON(c.Context(), d, remoteArgs, f, &result); err != nil {
			return err
		}
		if result.Manifest.SnapshotID != m.SnapshotID {
			return errors.New("peer acknowledged a different snapshot; local baseline unchanged")
		}
		if err = syncer.MarkSyncedPeer(c.Context(), root, m.SnapshotID, args[0]); err != nil {
			return fmt.Errorf("peer received snapshot, but local handoff receipt could not be saved: %w", err)
		}
		for _, path := range excluded {
			result.Warnings = append(result.Warnings, "Excluded secret candidate: "+path+"; use --include-secret <path> for permitted files")
		}
		return a.print(c, result, renderTransfer("Sent to "+args[0], result))
	}}
	c.Flags().StringSliceVar(&include, "include", nil, "additional ignored project path or glob to carry")
	c.Flags().StringSliceVar(&secrets, "include-secret", nil, "explicitly include a secret file in encrypted handoffs")
	c.Flags().BoolVar(&replace, "replace", false, "replace conflicting managed state after preserving a Git backup")
	c.Flags().BoolVar(&dry, "dry-run", false, "preview selection without changing a baseline")
	c.Flags().StringVar(&policy, "sessions", "keep-both", "divergent session policy: skip, theirs, or keep-both")
	addSelectionFlags(c, &selection)
	return c
}
func (a *app) pullCommand() *cobra.Command {
	var output, policy, remotePath string
	var replace bool
	var selection agentsessions.Selection
	c := &cobra.Command{Use: "pull <device>", Args: cobra.ExactArgs(1), Short: "Receive a workspace and agent-session handoff from a paired device", RunE: func(c *cobra.Command, args []string) error {
		if err := validateSessionPolicy(policy); err != nil {
			return err
		}
		d, err := device(args[0])
		if err != nil {
			return err
		}
		projectRoot, cfg, projectErr := loadProject()
		if output == "" {
			if projectErr != nil {
				return errors.New("use --output /absolute/project for a first pull, or run inside an initialized project")
			}
			output = projectRoot
		}
		output, err = filepath.Abs(output)
		if err != nil {
			return err
		}
		if projectErr == nil {
			d, err = resolveDevicePath(args[0], d, projectRoot, cfg)
			if err != nil {
				return err
			}
		} else if devicePath(d) == "" && remotePath == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(home, output)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return errors.New("first pull outside HOME requires --remote-path")
			}
			if d.Local {
				d.LocalPath = filepath.Join(d.Home, rel)
			} else {
				d.Path = filepath.Join(d.Home, rel)
			}
		}
		if remotePath != "" {
			if !filepath.IsAbs(remotePath) {
				return errors.New("--remote-path must be absolute")
			}
			if d.Local {
				d.LocalPath = remotePath
			} else {
				d.Path = remotePath
			}
		}
		if d.LocalPath != "" && filepath.Clean(d.LocalPath) == output {
			return errors.New("source and destination are the same project directory")
		}
		a.phase(c, "Checking peer protocol")
		if _, err = negotiatePeer(c.Context(), d); err != nil {
			return err
		}
		keys, err := identity.LoadOrCreate()
		if err != nil {
			return err
		}
		path, cleanup, err := tempPackage()
		if err != nil {
			return err
		}
		defer cleanup()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		remoteArgs := []string{"--json", "peer", "export", "--root", devicePath(d), "--from", identityName(keys.SigningPublic)}
		if local, err := syncer.Status(c.Context(), output); err == nil {
			if local.SnapshotID != "" {
				remoteArgs = append(remoteArgs, "--recipient-snapshot", local.SnapshotID)
			}
			for _, tip := range local.GitTips {
				remoteArgs = append(remoteArgs, "--recipient-tip", tip)
			}
		}
		remoteArgs = append(remoteArgs, sessionArgs(selection)...)
		a.phase(c, "Peer capturing and encrypting; receiving package")
		runErr := endpoint(d).Run(c.Context(), remoteArgs, nil, f)
		closeErr := f.Close()
		if runErr != nil {
			return runErr
		}
		if closeErr != nil {
			return closeErr
		}
		a.phase(c, "Verifying package and applying workspace and sessions")
		result, err := syncer.ApplyWithOptions(c.Context(), path, output, d.SigningPublic, syncer.ApplyOptions{Replace: replace, Peer: args[0], Sessions: agentsessions.ImportOptions{ConflictPolicy: policy}})
		if err != nil {
			return err
		}
		var ack map[string]any
		if err = peerJSON(c.Context(), d, []string{"peer", "ack", "--root", devicePath(d), "--snapshot", result.Manifest.SnapshotID, "--from", identityName(keys.SigningPublic)}, nil, &ack); err != nil {
			return fmt.Errorf("received workspace at %s, but source acknowledgement failed: %w", output, err)
		}
		return a.print(c, result, renderTransfer("Received from "+args[0], result))
	}}
	c.Flags().StringVarP(&output, "output", "o", "", "destination project directory")
	c.Flags().StringVar(&remotePath, "remote-path", "", "source project directory for a first pull")
	c.Flags().BoolVar(&replace, "replace", false, "replace conflicting managed state after preserving a Git backup")
	c.Flags().StringVar(&policy, "sessions", "keep-both", "divergent session policy: skip, theirs, or keep-both")
	addSelectionFlags(c, &selection)
	return c
}

func validateSessionPolicy(policy string) error {
	switch policy {
	case "", "skip", "theirs", "keep-both":
		return nil
	}
	return errors.New("--sessions must be skip, theirs, or keep-both")
}
