package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/syncer"
	"github.com/peerproblem/onthego/internal/tailscale"
	"github.com/spf13/cobra"
)

func (a *app) statusCommand() *cobra.Command {
	return &cobra.Command{Use: "status [device]", Args: cobra.MaximumNArgs(1), Short: "Show whether this workspace (or a device's copy) changed since the last handoff", RunE: func(c *cobra.Command, args []string) error {
		var status syncer.StatusInfo
		if len(args) > 0 {
			d, err := device(args[0])
			if err != nil {
				return err
			}
			root, cfg, err := loadProject()
			if err != nil {
				return err
			}
			d, err = resolveDevicePath(args[0], d, root, cfg)
			if err != nil {
				return err
			}
			if _, err = negotiatePeer(c.Context(), d); err != nil {
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
			state = "no handoff baseline yet"
		} else if status.Changed {
			state = "local changes since last transfer"
		}

		for _, warning := range status.Warnings {
			state += "\nWarning: " + terminalValue(warning)
		}
		if len(args) == 0 && status.ActiveDevice != "" {
			state += "\nNext: onthego sync " + terminalValue(status.ActiveDevice)
		}
		return a.print(c, status, terminalValue(status.Root)+"\n"+state)
	}}
}
func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{Use: "doctor [device]", Args: cobra.MaximumNArgs(1), Short: "Check prerequisites and, optionally, the connection to a device", RunE: func(c *cobra.Command, args []string) error {
		checks := map[string]string{}
		var warnings []string
		for agent, verified := range map[string]string{"codex": "0.163.0", "claude": "2.1.301"} {
			path, err := exec.LookPath(agent)
			if err != nil {
				checks[agent] = "not installed"
				continue
			}
			ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
			output, err := exec.CommandContext(ctx, path, "--version").Output()
			cancel()
			if err != nil {
				checks[agent] = "version check failed"
				warnings = append(warnings, agent+" version could not be checked")
				continue
			}
			version := strings.TrimSpace(string(output))
			checks[agent] = version
			found := false
			for _, token := range strings.Fields(version) {
				if strings.TrimPrefix(token, "v") == verified {
					found = true
				}
			}
			if !found {
				warnings = append(warnings, agent+" "+version+" is outside verified version "+verified+"; see docs/agent-compatibility.md")
			}
		}
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
		if _, err := exec.LookPath("ssh"); err != nil {
			checks["ssh"] = "not installed"
			warnings = append(warnings, "OpenSSH client not found; SSH devices need it")
		} else {
			checks["ssh"] = "available"
		}
		if st, err := tailscale.Read(c.Context()); err == nil {
			if st.Running() {
				checks["tailscale"] = "connected as " + st.Self.Name()
			} else {
				checks["tailscale"] = "installed, not connected (" + st.BackendState + ")"
			}
		} else if errors.Is(err, tailscale.ErrNotInstalled) {
			checks["tailscale"] = "not installed (optional)"
		} else {
			checks["tailscale"] = "status unavailable"
		}
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
			hello, err := negotiatePeer(c.Context(), d)
			if err != nil {
				return err
			}
			if hello.Version != a.version {
				warnings = append(warnings, args[0]+" runs onthego "+hello.Version+" and this device runs "+a.version+"; run onthego device install "+args[0]+" to match")
			}
			var pub publicIdentity
			if err = peerJSON(c.Context(), d, []string{"peer", "identity"}, nil, &pub); err != nil {
				return err
			}
			if pub.AgeRecipient != d.AgeRecipient || pub.SigningPublic != d.SigningPublic {
				return errors.New("peer identity changed; verify the machine and re-pair explicitly")
			}
			checks["peer"] = args[0] + " reachable, onthego " + hello.Version + ", identity verified"
		}
		message := "Ready."
		for _, key := range []string{"git", "ssh", "tailscale", "project", "codex", "claude", "peer"} {
			if value, ok := checks[key]; ok {
				message += fmt.Sprintf("\n  %-10s %s", key, terminalValue(value))
			}
		}
		for _, warning := range warnings {
			message += "\nWarning: " + terminalValue(warning)
		}
		return a.print(c, map[string]any{"checks": checks, "warnings": warnings}, message)
	}}
}
