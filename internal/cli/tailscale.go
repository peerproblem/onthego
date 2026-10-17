package cli

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/tailscale"
	"github.com/spf13/cobra"
)

const tailscaleSSHHelp = `Tailscale only connects the devices. Each device you pair also needs SSH:
  macOS:  System Settings > General > Sharing > Remote Login
  Linux:  sudo systemctl enable --now ssh
  Then, from this device: ssh-copy-id <user>@<device>
Linux alternative: sudo tailscale set --ssh (set its SSH rule to "accept",
not "check", so handoffs never wait for a browser login).`

func (a *app) tailscaleCommand() *cobra.Command {
	return &cobra.Command{Use: "tailscale", Aliases: []string{"ts"}, Args: cobra.NoArgs, Short: "Check Tailscale and list tailnet devices you can pair", Long: `Tailscale is optional. Use it when your devices are not on the same network,
for example a laptop on the road and a desktop or VPS at home. ONTHEGO only
reads "tailscale status"; it never changes your Tailscale settings.`, RunE: func(c *cobra.Command, _ []string) error {
		st, err := tailscale.Read(c.Context())
		if errors.Is(err, tailscale.ErrNotInstalled) {
			install := "https://tailscale.com/download"
			switch runtime.GOOS {
			case "linux":
				install = "curl -fsSL https://tailscale.com/install.sh | sh"
			case "darwin":
				install = "brew install --cask tailscale   (or https://tailscale.com/download/mac)"
			}
			message := "Tailscale is not installed on this device.\n\n1. Install it on every device:\n   " + install + "\n2. Sign in with the same account on each device\n   (macOS: open the Tailscale app; Linux: sudo tailscale up)\n3. Run onthego tailscale again to list your devices.\n\n" + tailscaleSSHHelp
			return a.print(c, map[string]any{"installed": false}, message)
		}
		if err != nil {
			return err
		}
		paired := map[string]string{}
		if u, err := config.LoadUser(); err == nil {
			for name, d := range u.Devices {
				host := d.Host
				if _, after, ok := strings.Cut(host, "@"); ok {
					host = after
				}
				paired[strings.ToLower(host)] = name
			}
		}
		var b strings.Builder
		if !st.Running() {
			fmt.Fprintf(&b, "Tailscale is installed but not connected (%s).\nSign in: open the Tailscale app (macOS) or run sudo tailscale up (Linux).\n", terminalValue(st.BackendState))
			return a.print(c, map[string]any{"installed": true, "status": st}, strings.TrimSpace(b.String()))
		}
		fmt.Fprintf(&b, "Tailscale is connected as %s", terminalValue(st.Self.Name()))
		if st.Tailnet != "" {
			fmt.Fprintf(&b, " on %s", terminalValue(st.Tailnet))
		}
		b.WriteString(".\n")
		if len(st.Peers) == 0 {
			b.WriteString("\nNo other devices on your tailnet yet. Install Tailscale on them and run tailscale up.\n")
		} else {
			b.WriteString("\nDevices on your tailnet:\n")
			for _, p := range st.Peers {
				state := "offline"
				if p.Online {
					state = "online"
				}
				line := fmt.Sprintf("  %-16s %-8s %-7s %s", terminalValue(p.Name()), terminalValue(p.OS), state, terminalValue(p.Address()))
				if name := paired[strings.ToLower(p.Address())]; name != "" {
					line += "   paired as " + terminalValue(name)
				}
				b.WriteString(line + "\n")
			}
			b.WriteString("\nPair one (run inside your project):\n  onthego device add <name> --tailscale [user@]<device>\n")
		}
		b.WriteString("\n" + tailscaleSSHHelp)
		return a.print(c, map[string]any{"installed": true, "status": st, "paired": paired}, b.String())
	}}
}
