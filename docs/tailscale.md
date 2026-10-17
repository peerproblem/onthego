# Using ONTHEGO with Tailscale

Tailscale is optional. ONTHEGO only needs one device to reach the other over SSH. Tailscale is the easiest way to get that when your devices are on different networks: a laptop on café Wi-Fi, a desktop behind a home router, a VPS in a data center. Every device gets a stable private address and a MagicDNS name such as `laptop.tail1234.ts.net`, and traffic is end-to-end encrypted with WireGuard.

ONTHEGO reads `tailscale status --json` to find devices by name. It never changes Tailscale settings, ACLs, or keys.

## Set up

1. **Install Tailscale on every device** and sign in with the same account.
   - macOS: install the app (`brew install --cask tailscale` or the [download page](https://tailscale.com/download/mac)) and sign in from the menu bar.
   - Linux: `curl -fsSL https://tailscale.com/install.sh | sh`, then `sudo tailscale up`.
2. **Turn on SSH on each device you will pair to.**
   - macOS: System Settings > General > Sharing > Remote Login.
   - Linux with OpenSSH: `sudo systemctl enable --now ssh` (the service is `sshd` on some distributions).
   - Linux with Tailscale SSH (no keys to manage): `sudo tailscale set --ssh`. See [Tailscale SSH](#tailscale-ssh) below.
3. **Let SSH log in without a password prompt** (not needed with Tailscale SSH):

   ```sh
   ssh-copy-id me@laptop
   ssh me@laptop true          # accept the host key once
   ```

4. **Check and pair** from inside your project:

   ```sh
   onthego tailscale
   onthego device add laptop --tailscale me@laptop --install
   onthego sync laptop
   ```

`onthego tailscale` lists every device on your tailnet with its OS, online state, and address, and marks the ones already paired. `--tailscale` accepts the short MagicDNS name, the full name, the machine's host name, or its Tailscale IP. Prefix `user@` when your login name differs between devices. The device is saved with its MagicDNS name, so it keeps working when the device's LAN address changes.

## Tailscale SSH

Tailscale SSH lets Linux devices accept SSH from your tailnet without `authorized_keys`. Two details matter for ONTHEGO:

- **Use "accept", not "check".** ONTHEGO runs SSH non-interactively. In "check" mode Tailscale asks for a browser login every so often, and a handoff would stop waiting for it. In the [access controls](https://login.tailscale.com/admin/acls) editor, set `"action": "accept"` on the SSH rule that covers your own devices, for example:

  ```json
  "ssh": [
    {"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["autogroup:nonroot"]}
  ]
  ```

- **macOS cannot be a Tailscale SSH server** with the App Store or standalone app. Use Remote Login on Macs.

Your local OpenSSH client still checks host keys. If the first connection reports an unknown host key, `onthego device add` offers to confirm it on an interactive terminal, or run `ssh me@laptop true` once yourself.

## A VPS on your tailnet

Install Tailscale on the server and pair it by name. With `--install`, the server does not need Node.js or a public SSH port:

```sh
onthego device add vps --tailscale vps --install
```

After that you can close port 22 on the server's public interface and keep SSH reachable only through Tailscale.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `Tailscale is not installed on this device` | Install it, sign in, and run `onthego tailscale` again. |
| `Tailscale is not connected` | Open the Tailscale app (macOS) or run `sudo tailscale up` (Linux). |
| `<name> is offline on your tailnet` | Wake the device and make sure Tailscale is running there. |
| `refused SSH` | Turn on Remote Login (macOS) or the SSH service (Linux) on that device. |
| `needs a key that works without a password prompt` | Run `ssh-copy-id user@device`, or load your key with `ssh-add`. |
| `is not in known_hosts yet` | Run `ssh user@device true` once and confirm the fingerprint. |
| A handoff hangs at the first SSH step | Tailscale SSH is in "check" mode. Switch the rule to "accept". |

`onthego doctor <device>` runs the full check: SSH login, the onthego version on the device, and the pinned identity.
