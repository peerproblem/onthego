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
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/packageotg"
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
	c.Flags().BoolVar(&selection.None, "no-sessions", false, "exclude native agent sessions from the handoff")
	c.PreRunE = func(c *cobra.Command, _ []string) error {
		if c.Flags().Changed("session") && len(selection.IDs) == 0 {
			return otgerror.New(otgerror.CodeInput, "session ID must not be blank")
		}
		return nil
	}
}
func sessionArgs(selection agentsessions.Selection) []string {
	var args []string
	if selection.None {
		return []string{"--no-sessions"}
	}
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
		if err := validateTransferOptions(selection, policy, include, secrets); err != nil {
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
			return otgerror.New(otgerror.CodeInput, "source and destination are the same project directory")
		}
		if dry {
			return a.printDryRun(c, root, cfg, selection, include, secrets, args[0])
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
		result.Warnings = append(result.Warnings, m.Warnings...)
		for _, path := range excluded {
			result.Warnings = append(result.Warnings, "Excluded secret candidate: "+path+"; use --include-secret <path> for permitted files")
		}
		return a.print(c, result, renderTransfer("Sent to "+args[0], result))
	}}
	c.Flags().StringSliceVar(&include, "include", nil, "additional ignored project path or glob to carry")
	c.Flags().StringSliceVar(&secrets, "include-secret", nil, "explicitly include a secret file in encrypted handoffs")
	c.Flags().BoolVar(&replace, "replace", false, "replace conflicting managed state after preserving a Git backup")
	c.Flags().BoolVar(&dry, "dry-run", false, "preview selection without changing a baseline")
	addSessionPolicyFlags(c, &policy)
	addSelectionFlags(c, &selection)
	return c
}
func (a *app) pullCommand() *cobra.Command {
	var output, policy, remotePath string
	var replace bool
	var selection agentsessions.Selection
	c := &cobra.Command{Use: "pull <device>", Args: cobra.ExactArgs(1), Short: "Receive a workspace and agent-session handoff from a paired device", RunE: func(c *cobra.Command, args []string) error {
		if err := validateTransferOptions(selection, policy, nil, nil); err != nil {
			return err
		}
		d, err := device(args[0])
		if err != nil {
			return err
		}
		projectRoot, cfg, projectErr := loadProject()
		if output == "" {
			if projectErr != nil {
				return otgerror.New(otgerror.CodeInput, "use --output /absolute/project for a first pull, or run inside an initialized project")
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
				return otgerror.New(otgerror.CodeInput, "first pull outside HOME requires --remote-path")
			}
			if d.Local {
				d.LocalPath = filepath.Join(d.Home, rel)
			} else {
				d.Path = filepath.Join(d.Home, rel)
			}
		}
		if remotePath != "" {
			if !filepath.IsAbs(remotePath) {
				return otgerror.New(otgerror.CodeInput, "--remote-path must be absolute")
			}
			if d.Local {
				d.LocalPath = remotePath
			} else {
				d.Path = remotePath
			}
		}
		if d.LocalPath != "" && filepath.Clean(d.LocalPath) == output {
			return otgerror.New(otgerror.CodeInput, "source and destination are the same project directory")
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
	addSessionPolicyFlags(c, &policy)
	addSelectionFlags(c, &selection)
	return c
}

func validateSessionPolicy(policy string) error {
	switch policy {
	case "", "skip", "theirs", "keep-both":
		return nil
	}
	return otgerror.New(otgerror.CodeInput, "--session-conflict must be skip, theirs, or keep-both")
}

func addSessionPolicyFlags(c *cobra.Command, policy *string) {
	c.Flags().StringVar(policy, "session-conflict", "keep-both", "divergent session policy: skip, theirs, or keep-both")
	c.Flags().StringVar(policy, "sessions", "keep-both", "deprecated alias for --session-conflict")
	_ = c.Flags().MarkDeprecated("sessions", "use --session-conflict instead; use --no-sessions to exclude histories")
	_ = c.Flags().MarkHidden("sessions")
}

func validateTransferOptions(selection agentsessions.Selection, policy string, include, secrets []string) error {
	if err := agentsessions.ValidateSelection(selection); err != nil {
		return err
	}
	if err := validateSessionPolicy(policy); err != nil {
		return err
	}
	for _, group := range []struct {
		name  string
		paths []string
		glob  bool
	}{{"--include", include, true}, {"--include-secret", secrets, false}} {
		for _, raw := range group.paths {
			normalized := filepath.ToSlash(filepath.Clean(raw))
			if raw == "" {
				normalized = ""
			}
			if _, err := packageotg.ProjectPath(normalized); err != nil {
				return otgerror.Wrap(otgerror.CodeInput, fmt.Sprintf("invalid %s path %q", group.name, raw), err)
			}
			if group.glob {
				if _, err := filepath.Match(normalized, ""); err != nil {
					return otgerror.Wrap(otgerror.CodeInput, fmt.Sprintf("invalid %s pattern %q", group.name, raw), err)
				}
			}
		}
	}
	return nil
}

func (a *app) printDryRun(c *cobra.Command, root string, cfg config.Config, selection agentsessions.Selection, include, secrets []string, destination string) error {
	files, err := syncer.Preview(c.Context(), root, include, secrets)
	if err != nil {
		return err
	}
	roots, err := syncer.SessionRoots(cfg)
	if err != nil {
		return err
	}
	sessions, err := agentsessions.DiscoverWithOptions(root, roots, selection)
	if err != nil {
		return err
	}
	warnings, err := syncer.PreviewWarnings(root)
	if err != nil {
		return err
	}
	warnings = append(warnings, sessions.Warnings...)
	value := map[string]any{"dry_run": true, "selected_files": files, "sessions": sessions.Sessions, "warnings": warnings, "not_checked": []string{"peer", "destination_conflict"}}
	heading := ""
	if destination != "" {
		value["device"] = destination
		heading = "Destination: " + terminalValue(destination)
	}
	message := renderPreview(files, heading) + renderSessionPreview(sessions.Sessions)
	for _, warning := range warnings {
		message += "\nWarning: " + terminalValue(warning)
	}
	message += "\nNot checked: peer connection and destination conflicts."
	return a.print(c, value, message)
}
