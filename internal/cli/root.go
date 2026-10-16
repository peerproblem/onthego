package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
	"github.com/jbaehova/onthego/internal/agentsessions"
	"github.com/jbaehova/onthego/internal/capture"
	"github.com/jbaehova/onthego/internal/config"
	"github.com/jbaehova/onthego/internal/gitx"
	"github.com/jbaehova/onthego/internal/identity"
	"github.com/jbaehova/onthego/internal/otgerror"
	"github.com/jbaehova/onthego/internal/peertransport"
	"github.com/jbaehova/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

type app struct {
	version, commit, builtAt string
	json                     bool
}
type publicIdentity struct {
	SchemaVersion int    `json:"schema_version"`
	AgeRecipient  string `json:"age_recipient"`
	SigningPublic string `json:"signing_public"`
}

// Execute deliberately never reads a project .env. Each machine owns its configuration.
func Execute(version, commit, builtAt string) error {
	a := &app{version: version, commit: commit, builtAt: builtAt}
	root := a.rootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.SetIn(os.Stdin)
	return root.Execute()
}
func ExitCode(err error) int {
	var e *otgerror.Error
	if !errors.As(err, &e) {
		return 1
	}
	switch e.Code {
	case otgerror.CodeInput:
		return 2
	case otgerror.CodePrecondition, otgerror.CodeUnsupportedGit, otgerror.CodeSecretPolicy, otgerror.CodeSessionFormat:
		return 3
	case otgerror.CodeTargetAuth, otgerror.CodeTargetUnavailable:
		return 4
	case otgerror.CodePackageInvalid, otgerror.CodeRestoreFailed:
		return 5
	case otgerror.CodeGenerationConflict, otgerror.CodeSourceChanged:
		return 6
	}
	return 1
}
func (a *app) rootCommand() *cobra.Command {
	c := &cobra.Command{Use: "onthego", Short: "Sync workspaces and agent sessions between your own machines", SilenceUsage: true, SilenceErrors: true}
	c.CompletionOptions.DisableDefaultCmd = true
	c.PersistentFlags().BoolVar(&a.json, "json", false, "write JSON output")
	c.AddCommand(a.initCommand(), a.deviceCommand(), a.pushCommand(), a.pullCommand(), a.statusCommand(), a.inspectCommand(), a.doctorCommand(), a.snapshotCommand(), a.identityCommand(), a.configCommand(), a.peerCommand())
	c.AddCommand(&cobra.Command{Use: "version", Args: cobra.NoArgs, Short: "Show build information", RunE: func(c *cobra.Command, _ []string) error {
		return a.print(c, map[string]any{"version": a.version, "commit": a.commit, "built_at": a.builtAt}, fmt.Sprintf("onthego %s (%s)", a.version, a.commit))
	}})
	return c
}
func (a *app) print(c *cobra.Command, v any, message string) error {
	if a.json {
		return json.NewEncoder(c.OutOrStdout()).Encode(v)
	}
	_, err := fmt.Fprintln(c.OutOrStdout(), message)
	return err
}
func loadProject() (string, config.Config, error) {
	root, err := config.FindRoot(".")
	if err != nil {
		return "", config.Config{}, err
	}
	cfg, err := config.Load(root)
	if os.IsNotExist(err) {
		return "", cfg, errors.New("run onthego init in this project first")
	}
	return root, cfg, err
}
func (a *app) initCommand() *cobra.Command {
	return &cobra.Command{Use: "init", Args: cobra.NoArgs, Short: "Initialize this Git project and private machine settings", RunE: func(c *cobra.Command, _ []string) error {
		root, err := config.FindRoot(".")
		if err != nil {
			return err
		}
		if err = gitx.Validate(c.Context(), root); err != nil {
			return err
		}
		cfg, err := config.Load(root)
		if os.IsNotExist(err) {
			cfg = config.Default()
		} else if err != nil {
			return err
		}
		if err = config.Save(root, cfg); err != nil {
			return err
		}
		if _, err = config.EnsureGitignore(root); err != nil {
			return err
		}
		if _, err = identity.LoadOrCreate(); err != nil {
			return err
		}
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		if err = config.SaveUser(u); err != nil {
			return err
		}
		path, err := config.UserConfigPath()
		if err != nil {
			return err
		}
		return a.print(c, map[string]any{"schema_version": 2, "project_id": cfg.ProjectID, "root": root, "config": path}, "Initialized project "+cfg.ProjectID+"\nMachine settings: "+path+"\nAdd a device with onthego device add.")
	}}
}
func localIdentity() (publicIdentity, error) {
	k, err := identity.LoadOrCreate()
	return publicIdentity{2, k.AgeRecipient, k.SigningPublic}, err
}
func (a *app) identityCommand() *cobra.Command {
	return &cobra.Command{Use: "identity", Args: cobra.NoArgs, Short: "Show this machine's public encryption and signing keys", RunE: func(c *cobra.Command, _ []string) error {
		v, err := localIdentity()
		if err != nil {
			return err
		}
		return a.print(c, v, "Encryption recipient: "+v.AgeRecipient+"\nSigning public key: "+v.SigningPublic)
	}}
}
func (a *app) configCommand() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Locate private machine configuration"}
	c.AddCommand(&cobra.Command{Use: "path", Args: cobra.NoArgs, Short: "Show ~/.onthego/config.yaml location", RunE: func(c *cobra.Command, _ []string) error {
		p, err := config.UserConfigPath()
		if err != nil {
			return err
		}
		return a.print(c, map[string]string{"path": p}, p)
	}})
	return c
}
func endpoint(d config.Device) peertransport.Endpoint {
	return peertransport.Endpoint{Host: d.Host, Path: devicePath(d), Port: d.Port, IdentityFile: d.IdentityFile, Binary: d.Binary}
}
func devicePath(d config.Device) string {
	if d.LocalPath != "" {
		return d.LocalPath
	}
	return d.Path
}
func device(name string) (config.Device, error) {
	u, err := config.LoadUser()
	if err != nil {
		return config.Device{}, err
	}
	d, ok := u.Devices[name]
	if !ok {
		return d, fmt.Errorf("device %q is not configured; run onthego device add", name)
	}
	if err = config.ValidateDevice(d); err != nil {
		return d, err
	}
	if d.AgeRecipient == "" || d.SigningPublic == "" {
		return d, errors.New("device public keys are missing; remove and add this device again")
	}
	return d, nil
}
func peerJSON(ctx context.Context, d config.Device, args []string, input io.Reader, result any) error {
	var b bytes.Buffer
	if err := endpoint(d).Run(ctx, args, input, &b); err != nil {
		return err
	}
	if err := json.Unmarshal(b.Bytes(), result); err != nil {
		return fmt.Errorf("invalid peer response (install the same onthego version on both machines): %w", err)
	}
	return nil
}
func (a *app) deviceCommand() *cobra.Command {
	c := &cobra.Command{Use: "device", Short: "Manage direct connections in private machine settings"}
	var d config.Device
	add := &cobra.Command{Use: "add <name>", Args: cobra.ExactArgs(1), Short: "Pair a project destination over trusted SSH or a local path", RunE: func(c *cobra.Command, args []string) error {
		if err := config.ValidateDeviceName(args[0]); err != nil {
			return err
		}
		if err := config.ValidateDevice(d); err != nil {
			return err
		}
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		if _, ok := u.Devices[args[0]]; ok {
			return errors.New("device already exists; remove it before pairing again")
		}
		var pub publicIdentity
		if err = peerJSON(c.Context(), d, []string{"peer", "identity"}, nil, &pub); err != nil {
			return err
		}
		if pub.SchemaVersion != 2 {
			return errors.New("unsupported peer protocol; install the same onthego version on both machines")
		}
		if _, err = age.ParseX25519Recipient(pub.AgeRecipient); err != nil {
			return err
		}
		if _, err = identity.ParsePublic(pub.SigningPublic); err != nil {
			return err
		}
		d.AgeRecipient = pub.AgeRecipient
		d.SigningPublic = pub.SigningPublic
		u.Devices[args[0]] = d
		if err = config.SaveUser(u); err != nil {
			return err
		}
		return a.print(c, map[string]any{"name": args[0], "device": d}, "Paired "+args[0]+" → "+devicePath(d))
	}}
	add.Flags().StringVar(&d.Host, "host", "", "SSH host or user@host from ~/.ssh/config")
	add.Flags().StringVar(&d.Path, "path", "", "absolute project path on the SSH peer")
	add.Flags().StringVar(&d.LocalPath, "local-path", "", "absolute destination path on this machine (no SSH)")
	add.Flags().StringVar(&d.IdentityFile, "identity-file", "", "SSH private key path; key contents stay in your SSH configuration")
	add.Flags().StringVar(&d.Binary, "binary", "onthego", "onthego executable on the peer (absolute path recommended for SSH)")
	add.Flags().IntVar(&d.Port, "port", 0, "SSH port; default uses SSH configuration")
	c.AddCommand(add, &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List paired devices", RunE: func(c *cobra.Command, _ []string) error {
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(u.Devices))
		for n := range u.Devices {
			names = append(names, n)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, n := range names {
			d := u.Devices[n]
			fmt.Fprintf(&b, "%s\t%s\t%s\n", n, d.Host, devicePath(d))
		}
		if len(names) == 0 {
			b.WriteString("No devices configured. Use onthego device add.")
		}
		return a.print(c, u.Devices, strings.TrimSpace(b.String()))
	}}, &cobra.Command{Use: "remove <name>", Args: cobra.ExactArgs(1), Short: "Remove a local pairing without modifying the peer", RunE: func(c *cobra.Command, args []string) error {
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		if _, ok := u.Devices[args[0]]; !ok {
			return fmt.Errorf("unknown device %q", args[0])
		}
		delete(u.Devices, args[0])
		if err = config.SaveUser(u); err != nil {
			return err
		}
		return a.print(c, map[string]string{"removed": args[0]}, "Removed "+args[0])
	}})
	return c
}
func tempPackage() (string, func(), error) {
	base, err := config.DataRoot()
	if err != nil {
		return "", nil, err
	}
	if err = config.EnsurePrivateDir(base); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, "transfer-*")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, "snapshot.otg"), func() { _ = os.RemoveAll(dir) }, nil
}
func (a *app) pushCommand() *cobra.Command {
	var include, secrets []string
	var replace, dry bool
	c := &cobra.Command{Use: "push <device>", Aliases: []string{"pass"}, Args: cobra.ExactArgs(1), Short: "Send workspace changes and native agent sessions to a paired device", RunE: func(c *cobra.Command, args []string) error {
		root, _, err := loadProject()
		if err != nil {
			return err
		}
		d, err := device(args[0])
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
		path, cleanup, err := tempPackage()
		if err != nil {
			return err
		}
		defer cleanup()
		m, err := syncer.Export(c.Context(), root, path, d.AgeRecipient, a.version, include, secrets)
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
		remoteArgs := []string{"peer", "apply", "--root", devicePath(d), "--signing-public", keys.SigningPublic}
		if replace {
			remoteArgs = append(remoteArgs, "--replace")
		}
		var result syncer.Result
		if err = peerJSON(c.Context(), d, remoteArgs, f, &result); err != nil {
			return err
		}
		if result.Manifest.SnapshotID != m.SnapshotID {
			return errors.New("peer acknowledged a different snapshot; local baseline unchanged")
		}
		if err = syncer.MarkSynced(c.Context(), root, m.SnapshotID); err != nil {
			return fmt.Errorf("peer received snapshot, but local sync receipt could not be saved: %w", err)
		}
		return a.print(c, result, renderTransfer("Sent to "+args[0], result))
	}}
	c.Flags().StringSliceVar(&include, "include", nil, "additional ignored project path or glob to carry")
	c.Flags().StringSliceVar(&secrets, "include-secret", nil, "add a secret file to encrypted handoffs; remains managed until removed and synchronized")
	c.Flags().BoolVar(&replace, "replace", false, "replace a conflicting destination after preserving its backup")
	c.Flags().BoolVar(&dry, "dry-run", false, "preview selection without transferring or changing a baseline")
	return c
}
func (a *app) pullCommand() *cobra.Command {
	var output string
	var replace bool
	c := &cobra.Command{Use: "pull <device>", Args: cobra.ExactArgs(1), Short: "Receive workspace changes and agent sessions from a paired device", RunE: func(c *cobra.Command, args []string) error {
		d, err := device(args[0])
		if err != nil {
			return err
		}
		if output == "" {
			output, _, err = loadProject()
			if err != nil {
				return errors.New("use --output /absolute/project for a first pull, or run inside an initialized project")
			}
		}
		output, err = filepath.Abs(output)
		if err != nil {
			return err
		}
		if d.LocalPath != "" && filepath.Clean(d.LocalPath) == output {
			return errors.New("source and destination are the same project directory")
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
		runErr := endpoint(d).Run(c.Context(), []string{"peer", "export", "--root", devicePath(d), "--recipient", keys.AgeRecipient}, nil, f)
		closeErr := f.Close()
		if runErr != nil {
			return runErr
		}
		if closeErr != nil {
			return closeErr
		}
		result, err := syncer.Apply(c.Context(), path, output, d.SigningPublic, replace)
		if err != nil {
			return err
		}
		var ack map[string]any
		if err = peerJSON(c.Context(), d, []string{"peer", "ack", "--root", devicePath(d), "--snapshot", result.Manifest.SnapshotID}, nil, &ack); err != nil {
			return fmt.Errorf("received workspace at %s, but source acknowledgement failed (source baseline unchanged): %w", output, err)
		}
		return a.print(c, result, renderTransfer("Received from "+args[0], result))
	}}
	c.Flags().StringVarP(&output, "output", "o", "", "destination project directory (defaults to current project)")
	c.Flags().BoolVar(&replace, "replace", false, "replace a conflicting destination after preserving its backup")
	return c
}
func renderTransfer(action string, r syncer.Result) string {
	msg := action + " → " + r.Root
	if r.Backup != "" {
		msg += "\nPrevious workspace preserved at " + r.Backup
	}
	root := "'" + strings.ReplaceAll(r.Root, "'", "'\\''") + "'"
	msg += "\nOn the receiving machine, re-enter the restored workspace: cd " + root
	msg += fmt.Sprintf("\nAgent histories: %d imported, %d updated, %d unchanged.", r.Sessions.Imported, r.Sessions.Updated, r.Sessions.Skipped)
	for _, file := range r.Manifest.Files {
		if file.Secret {
			msg += "\nManaged secret file: " + file.RelativePath + " (included in subsequent handoffs)"
		}
	}
	seen := map[string]bool{}
	for _, session := range r.Sessions.Sessions {
		if session.ID == "" || seen[session.Agent+session.ID] {
			continue
		}
		seen[session.Agent+session.ID] = true
		id := "'" + strings.ReplaceAll(session.ID, "'", "'\\''") + "'"
		if session.Agent == "codex" {
			msg += "\nResume: codex resume " + id
		}
		if session.Agent == "claude" {
			msg += "\nResume: claude --resume " + id
		}
	}
	if len(r.Sessions.Sessions) > 0 {
		msg += "\nRun resume commands from the restored project. Desktop sidebar databases are not transferred."
	}
	return msg
}
func (a *app) statusCommand() *cobra.Command {
	return &cobra.Command{Use: "status [device]", Args: cobra.MaximumNArgs(1), Short: "Inspect local state or a paired device without an API call", RunE: func(c *cobra.Command, args []string) error {
		var status syncer.StatusInfo
		if len(args) > 0 {
			d, err := device(args[0])
			if err != nil {
				return err
			}
			if err = peerJSON(c.Context(), d, []string{"peer", "status", "--root", devicePath(d)}, nil, &status); err != nil {
				return err
			}
		} else {
			root, _, err := loadProject()
			if err != nil {
				return err
			}
			status, err = syncer.Status(c.Context(), root)
			if err != nil {
				return err
			}
		}
		state := "unchanged since last transfer"
		if !status.HasBaseline {
			state = "no sync baseline yet"
		} else if status.Changed {
			state = "local changes since last transfer"
		}
		return a.print(c, status, status.Root+"\n"+state)
	}}
}
func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{Use: "doctor [device]", Args: cobra.MaximumNArgs(1), Short: "Check Git, private configuration, and optionally a peer connection", RunE: func(c *cobra.Command, args []string) error {
		checks := map[string]string{}
		v, err := gitx.Version(c.Context())
		if err != nil {
			return err
		}
		checks["git"] = v
		path, err := config.UserConfigPath()
		if err != nil {
			return err
		}
		if _, err = config.LoadUser(); err != nil {
			return err
		}
		checks["config"] = path
		if _, err = identity.LoadOrCreate(); err != nil {
			return err
		}
		checks["identity"] = "ready"
		if root, err := config.FindRoot("."); err == nil {
			if _, err := config.Load(root); err != nil {
				return fmt.Errorf("project configuration (run onthego init): %w", err)
			}
			if err = gitx.Validate(c.Context(), root); err != nil {
				return err
			}
			checks["project"] = root
		}
		if len(args) > 0 {
			d, err := device(args[0])
			if err != nil {
				return err
			}
			if d.Host != "" {
				if _, err = exec.LookPath("ssh"); err != nil {
					return err
				}
			}
			var pub publicIdentity
			if err = peerJSON(c.Context(), d, []string{"peer", "identity"}, nil, &pub); err != nil {
				return err
			}
			if pub.AgeRecipient != d.AgeRecipient || pub.SigningPublic != d.SigningPublic {
				return errors.New("peer identity changed; verify the machine and re-pair explicitly")
			}
			checks["peer"] = args[0] + " connected and identity verified"
		}
		return a.print(c, checks, "Ready. Git and local identity are available; no ONTHEGO account or API key is required.")
	}}
}

func renderPreview(files []capture.PreviewFile, destination string) string {
	var out strings.Builder
	if destination != "" {
		fmt.Fprintln(&out, destination)
	}
	fmt.Fprintf(&out, "Would capture Git history, staged/unstaged changes, and project-scoped native sessions.\nSelected extra files: %d\n", len(files))
	for _, file := range files {
		label := file.Policy
		if file.Secret {
			label = "managed secret, encrypted"
		}
		fmt.Fprintf(&out, "  %s (%s)\n", file.RelativePath, label)
	}
	out.WriteString("Preview only; no files transferred or synchronization baseline changed.")
	return out.String()
}

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
		fmt.Fprintf(&out, "Project: %s\nGit history and staged/unstaged changes are included.\nExtra files: %d\n", root, len(files))
		for _, f := range files {
			fmt.Fprintf(&out, "  %s (%s)\n", f.RelativePath, f.Policy)
		}
		fmt.Fprintf(&out, "Agent sessions: %d\n", len(sessions))
		for _, s := range sessions {
			fmt.Fprintf(&out, "  %s %s\n", s.Agent, s.ID)
		}
		out.WriteString("Ignored files need include rules. Secrets need --include-secret; agent authentication is never imported.")
		return a.print(c, map[string]any{"schema_version": 2, "project_id": cfg.ProjectID, "files": files, "sessions": sessions}, out.String())
	}}
}
