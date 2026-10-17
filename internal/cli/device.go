package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"filippo.io/age"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/peertransport"
	"github.com/peerproblem/onthego/internal/tailscale"
	"github.com/spf13/cobra"
)

func (a *app) deviceCommand() *cobra.Command {
	c := &cobra.Command{Use: "device", Short: "Pair and manage your other devices"}
	var d config.Device
	var tailnetName string
	var install bool
	add := &cobra.Command{Use: "add <name>", Args: cobra.ExactArgs(1), Short: "Pair a device over SSH (or by Tailscale name)", Example: `  onthego device add laptop --host me@laptop.local
  onthego device add vps --host deploy@203.0.113.10 --install
  onthego device add laptop --tailscale laptop`, RunE: func(c *cobra.Command, args []string) error {
		name := args[0]
		if err := config.ValidateDeviceName(name); err != nil {
			return err
		}
		if tailnetName != "" {
			if d.Host != "" || d.LocalPath != "" || d.Local {
				return otgerror.New(otgerror.CodeInput, "use either --tailscale or --host/--local-path")
			}
			host, err := tailnetHost(c.Context(), tailnetName)
			if err != nil {
				return err
			}
			d.Host = host
		}
		if d.LocalPath != "" {
			d.Local = true
		}
		if d.Host == "" && !d.Local {
			return otgerror.New(otgerror.CodeInput, "choose how to reach the device: --host user@host, --tailscale <device>, or --local-path /absolute/path")
		}
		if install && d.Host == "" {
			return otgerror.New(otgerror.CodeInput, "--install needs an SSH device (--host or --tailscale)")
		}
		if err := config.ValidateDevice(d); err != nil {
			return otgerror.Wrap(otgerror.CodeInput, "invalid device settings", err)
		}
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		if _, ok := u.Devices[name]; ok {
			return otgerror.New(otgerror.CodeInput, fmt.Sprintf("device %s already exists; run onthego device remove %s before pairing again", name, name))
		}
		var hello helloResponse
		err = a.withHostKeyConfirmation(c, d, func() error {
			var err error
			switch {
			case d.Host == "":
			case install:
				a.phase(c, "Installing onthego "+a.version+" on "+d.Host)
				if d.Binary, err = a.installOnDevice(c.Context(), d); err != nil {
					return err
				}
			case !c.Flags().Changed("binary"):
				a.phase(c, "Looking for onthego on "+d.Host)
				if d.Binary, err = peertransport.DiscoverBinary(c.Context(), endpoint(d)); err != nil {
					var typed *otgerror.Error
					if errors.As(err, &typed) {
						return err
					}
					return otgerror.Wrap(otgerror.CodeTargetUnavailable, fmt.Sprintf("onthego is not installed on %s (or not on its login PATH); add --install to copy this version there, or pass --binary /absolute/path", d.Host), err)
				}
			}
			hello, err = negotiatePeer(c.Context(), d)
			return err
		})
		if err != nil {
			return err
		}
		d.Home = hello.Home
		if err = a.exchangeIdentities(c.Context(), &d); err != nil {
			return err
		}
		if target := devicePath(d); target != "" {
			_, cfg, err := loadProject()
			if err != nil {
				return fmt.Errorf("path mapping requires an initialized project: %w", err)
			}
			if err = config.SetProjectPeer(cfg.ProjectID, name, target); err != nil {
				return err
			}
		}
		d.Path = ""
		d.LocalPath = ""
		u.Devices[name] = d
		if u.Trusted == nil {
			u.Trusted = map[string]config.TrustedIdentity{}
		}
		u.Trusted[identityName(d.SigningPublic)] = config.TrustedIdentity{AgeRecipient: d.AgeRecipient, SigningPublic: d.SigningPublic}
		if err = config.SaveUser(u); err != nil {
			return err
		}
		message := "Paired " + terminalValue(name) + " (both devices now trust each other)"
		if hello.Version != a.version {
			message += "\nWarning: " + terminalValue(name) + " runs onthego " + terminalValue(hello.Version) + " and this device runs " + terminalValue(a.version) + "; run onthego device install " + terminalValue(name) + " to match"
		}
		message += "\nNext: onthego sync " + terminalValue(name) + " --dry-run   (preview), then onthego sync " + terminalValue(name)
		return a.print(c, map[string]any{"name": name, "device": d, "peer_version": hello.Version}, message)
	}}
	add.Flags().StringVar(&d.Host, "host", "", "SSH host, user@host, or an alias from ~/.ssh/config")
	add.Flags().StringVar(&tailnetName, "tailscale", "", "pair a device on your tailnet by name ([user@]name); see onthego tailscale")
	add.Flags().BoolVar(&install, "install", false, "copy this onthego build to ~/.local/bin on the SSH device first")
	add.Flags().StringVar(&d.Path, "path", "", "absolute project path on the device for the current project")
	add.Flags().StringVar(&d.LocalPath, "local-path", "", "pair a directory on this machine instead of an SSH device")
	add.Flags().BoolVar(&d.Local, "local", false, "pair a local executable independently of a project path")
	add.Flags().StringVar(&d.IdentityFile, "identity-file", "", "SSH private key path; key contents stay in your SSH configuration")
	add.Flags().StringVar(&d.Binary, "binary", "onthego", "onthego executable on the device (absolute path recommended for SSH)")
	add.Flags().IntVar(&d.Port, "port", 0, "SSH port; default uses SSH configuration")
	_ = add.Flags().MarkHidden("local")

	list := &cobra.Command{Use: "list", Aliases: []string{"ls"}, Args: cobra.NoArgs, Short: "List paired devices and this project's paths on them", RunE: func(c *cobra.Command, _ []string) error {
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(u.Devices))
		for n := range u.Devices {
			names = append(names, n)
		}
		sort.Strings(names)
		paths := map[string]string{}
		if root, cfg, err := loadProject(); err == nil {
			for _, n := range names {
				if resolved, err := resolveDevicePath(n, u.Devices[n], root, cfg); err == nil {
					paths[n] = devicePath(resolved)
				}
			}
		}
		var b strings.Builder
		for _, n := range names {
			d := u.Devices[n]
			where := d.Host
			if where == "" {
				where = "this machine"
			}
			fmt.Fprintf(&b, "%s\t%s", terminalValue(n), terminalValue(where))
			if p := paths[n]; p != "" {
				fmt.Fprintf(&b, "\t%s", terminalValue(p))
			}
			b.WriteString("\n")
		}
		if len(names) == 0 {
			b.WriteString("No devices paired yet. Use onthego device add <name> --host user@host (or --tailscale <name>).")
		}
		type listed struct {
			config.Device
			ProjectPath string `json:"project_path,omitempty"`
		}
		value := map[string]listed{}
		for _, n := range names {
			value[n] = listed{u.Devices[n], paths[n]}
		}
		return a.print(c, value, strings.TrimSpace(b.String()))
	}}

	remove := &cobra.Command{Use: "remove <name>", Aliases: []string{"rm"}, Args: cobra.ExactArgs(1), ValidArgsFunction: completeDevices, Short: "Forget a device on this machine (the device itself is not changed)", RunE: func(c *cobra.Command, args []string) error {
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		if _, ok := u.Devices[args[0]]; !ok {
			return otgerror.New(otgerror.CodeInput, fmt.Sprintf("unknown device %q", args[0]))
		}
		removed := u.Devices[args[0]]
		delete(u.Devices, args[0])
		stillUsed := false
		for _, other := range u.Devices {
			if other.SigningPublic == removed.SigningPublic {
				stillUsed = true
			}
		}
		if !stillUsed {
			delete(u.Trusted, identityName(removed.SigningPublic))
		}
		if err = config.SaveUser(u); err != nil {
			return err
		}
		return a.print(c, map[string]string{"removed": args[0]}, "Removed "+terminalValue(args[0]))
	}}

	installCmd := &cobra.Command{Use: "install <name>", Args: cobra.ExactArgs(1), ValidArgsFunction: completeDevices, Short: "Install or upgrade onthego on a paired SSH device to this version", RunE: func(c *cobra.Command, args []string) error {
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		d, err := device(args[0])
		if err != nil {
			return err
		}
		if d.Host == "" {
			return otgerror.New(otgerror.CodeInput, "install works only for SSH devices")
		}
		var hello helloResponse
		err = a.withHostKeyConfirmation(c, d, func() error {
			var err error
			a.phase(c, "Installing onthego "+a.version+" on "+d.Host)
			if d.Binary, err = a.installOnDevice(c.Context(), d); err != nil {
				return err
			}
			hello, err = negotiatePeer(c.Context(), d)
			return err
		})
		if err != nil {
			return err
		}
		var pub publicIdentity
		if err = peerJSON(c.Context(), d, []string{"peer", "identity"}, nil, &pub); err != nil {
			return err
		}
		if pub.AgeRecipient != d.AgeRecipient || pub.SigningPublic != d.SigningPublic {
			return otgerror.New(otgerror.CodeTargetAuth, "the device identity changed; verify the machine, then remove and pair it again")
		}
		u.Devices[args[0]] = d
		if err = config.SaveUser(u); err != nil {
			return err
		}
		return a.print(c, map[string]any{"name": args[0], "binary": d.Binary, "version": hello.Version}, "Installed onthego "+terminalValue(hello.Version)+" on "+terminalValue(args[0])+" at "+terminalValue(d.Binary))
	}}

	pathCmd := &cobra.Command{Use: "path <name> [absolute-path]", Args: cobra.RangeArgs(1, 2), ValidArgsFunction: completeDevices, Short: "Show or set where this project lives on a device", RunE: func(c *cobra.Command, args []string) error {
		return a.devicePath(c, args)
	}}
	c.AddCommand(add, list, remove, installCmd, pathCmd)
	return c
}

// remoteCommand keeps the 0.3 spelling working.
func (a *app) remoteCommand() *cobra.Command {
	c := &cobra.Command{Use: "remote", Hidden: true, Short: "Deprecated: use onthego device path"}
	c.AddCommand(&cobra.Command{Use: "set <device> <path>", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, args []string) error {
		fmt.Fprintln(c.ErrOrStderr(), "Warning: onthego remote set is deprecated; use onthego device path <device> <path>")
		return a.devicePath(c, args)
	}})
	return c
}

func (a *app) devicePath(c *cobra.Command, args []string) error {
	root, cfg, err := loadProject()
	if err != nil {
		return err
	}
	d, err := device(args[0])
	if err != nil {
		return err
	}
	if len(args) == 1 {
		resolved, err := resolveDevicePath(args[0], d, root, cfg)
		if err != nil {
			return err
		}
		return a.print(c, map[string]string{"device": args[0], "path": devicePath(resolved)}, terminalValue(devicePath(resolved)))
	}
	if !filepath.IsAbs(args[1]) {
		return otgerror.New(otgerror.CodeInput, "device path must be absolute")
	}
	if err = config.SetProjectPeer(cfg.ProjectID, args[0], args[1]); err != nil {
		return err
	}
	return a.print(c, map[string]string{"device": args[0], "path": args[1]}, "This project lives at "+terminalValue(args[1])+" on "+terminalValue(args[0]))
}

func (a *app) exchangeIdentities(ctx context.Context, d *config.Device) error {
	var pub publicIdentity
	if err := peerJSON(ctx, *d, []string{"peer", "identity"}, nil, &pub); err != nil {
		return err
	}
	if pub.SchemaVersion != 2 {
		return errors.New("unsupported peer protocol; install the same onthego version on both machines")
	}
	if _, err := age.ParseX25519Recipient(pub.AgeRecipient); err != nil {
		return err
	}
	if _, err := identity.ParsePublic(pub.SigningPublic); err != nil {
		return err
	}
	d.AgeRecipient = pub.AgeRecipient
	d.SigningPublic = pub.SigningPublic
	local, err := localIdentity()
	if err != nil {
		return err
	}
	var trust map[string]any
	return peerJSON(ctx, *d, []string{"peer", "trust", "--name", identityName(local.SigningPublic), "--age-recipient", local.AgeRecipient, "--signing-public", local.SigningPublic}, nil, &trust)
}

// withHostKeyConfirmation retries once after letting OpenSSH ask the user to
// confirm an unknown host key on an interactive terminal.
func (a *app) withHostKeyConfirmation(c *cobra.Command, d config.Device, run func() error) error {
	err := run()
	if err == nil || !errors.Is(err, peertransport.ErrUnknownHostKey) || a.json || !interactive(c.InOrStdin()) {
		return err
	}
	fmt.Fprintln(c.ErrOrStderr(), "First SSH connection to "+terminalValue(d.Host)+". Check the fingerprint below before you answer yes.")
	if confirmErr := peertransport.ConfirmHostKey(c.Context(), endpoint(d), c.InOrStdin(), c.ErrOrStderr(), c.ErrOrStderr()); confirmErr != nil {
		return err
	}
	return run()
}

func interactive(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func tailnetHost(ctx context.Context, value string) (string, error) {
	user, name, hasUser := strings.Cut(value, "@")
	if !hasUser {
		name, user = value, ""
	}
	st, err := tailscale.Read(ctx)
	if errors.Is(err, tailscale.ErrNotInstalled) {
		return "", otgerror.New(otgerror.CodePrecondition, "Tailscale is not installed on this device; install it from https://tailscale.com/download, run tailscale up, then retry")
	}
	if err != nil {
		return "", otgerror.Wrap(otgerror.CodePrecondition, "could not read Tailscale status; run onthego tailscale for details", err)
	}
	if !st.Running() {
		return "", otgerror.New(otgerror.CodePrecondition, "Tailscale is not connected on this device ("+st.BackendState+"); run tailscale up, then retry")
	}
	peer, err := st.Resolve(name)
	if err != nil {
		return "", err
	}
	if user != "" {
		return user + "@" + peer.Address(), nil
	}
	return peer.Address(), nil
}

// installOnDevice copies the onthego build for the device's platform. The npm
// package ships every supported platform next to the running executable, so
// the exact same version reaches the device without Node.js or internet access.
func (a *app) installOnDevice(ctx context.Context, d config.Device) (string, error) {
	platform, err := peertransport.RemotePlatform(ctx, endpoint(d))
	if err != nil {
		return "", err
	}
	binary, err := localBuildFor(platform)
	if err != nil {
		return "", err
	}
	f, err := os.Open(binary)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return peertransport.InstallBinary(ctx, endpoint(d), f)
}

func localBuildFor(platform string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if runtime.GOOS+"-"+runtime.GOARCH == platform {
		return exe, nil
	}
	sibling := filepath.Join(filepath.Dir(exe), "onthego-"+platform)
	if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() {
		return sibling, nil
	}
	return "", otgerror.New(otgerror.CodePrecondition, "this onthego installation has no build for "+platform+"; install the npm package (which includes every platform), or on the device run: curl -fsSL https://raw.githubusercontent.com/peerproblem/onthego/main/install.sh | sh")
}
