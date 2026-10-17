# ONTHEGO

Move your unfinished work and your coding-agent conversations between your desktop, laptop, and VPS with one command.

```sh
onthego sync laptop
```

ONTHEGO carries a Git project exactly as you left it: staged, unstaged, and untracked work, plus the project's Codex CLI and Claude Code sessions, so you can `codex resume` or `claude --resume` on the other device and keep going. Each device keeps its own Git settings, hooks, `node_modules`, caches, and agent logins.

It is a small open-source CLI (MIT). There is no account, no hosted service, and no AI feature. Your devices talk to each other directly over SSH, optionally through [Tailscale](#anywhere-with-tailscale).

## Install

```sh
npm install -g @jbaehova/onthego
```

The npm package ships native binaries for macOS and Linux (arm64 and amd64). Git and OpenSSH are required; Node.js is only needed for npm itself.

On a server without Node.js:

```sh
curl -fsSL https://raw.githubusercontent.com/peerproblem/onthego/main/install.sh | sh
```

The script downloads the release binary for the machine, verifies its SHA-256 checksum, and installs it to `~/.local/bin`. Pass `-s -- --version 0.4.0` for a specific version.

You usually do not need to install it on the other devices by hand: `onthego device add <name> --install` copies the exact same version over SSH.

<details>
<summary>Build from source</summary>

```sh
git clone https://github.com/peerproblem/onthego.git
cd onthego
make build            # Go 1.26 or newer
install -m 755 dist/onthego ~/.local/bin/onthego
```

</details>

## Quick start

On your desktop, inside a Git project:

```sh
onthego init                                      # once per project; commit .onthego.yaml
onthego device add laptop --host me@laptop.local --install
onthego sync laptop
```

Then on the laptop:

```sh
cd ~/projects/my-app                              # the same home-relative path by default
codex resume <session-id>                         # or: claude --resume <session-id>
```

The handoff prints the exact `cd` and resume commands. When you are back at the desktop, run `onthego sync laptop` again: the desktop knows it handed the work off, so it pulls the laptop's changes back.

`sync` picks the direction for you:

| Situation | What `sync` does |
| --- | --- |
| The project is not on the other device yet | push |
| Only this device changed since the last handoff | push |
| Only the other device changed | pull |
| You handed the work off and have not touched it since | pull (take it back) |
| Nothing changed | nothing |
| Both sides changed | stops, and tells you to choose `push --replace` or `pull --replace` |

Nothing is ever merged silently. Whenever a handoff replaces work, the replaced state is kept as a Git backup that `onthego undo` restores. Use `onthego sync laptop --dry-run` to see the decision first, or `push` and `pull` to choose the direction yourself.

## Connecting your devices

ONTHEGO uses your normal SSH setup (`~/.ssh/config`, keys, `known_hosts`). The only requirement is that one device can `ssh` into the other without a password prompt. **Run every command from the device that can reach the other one.** A laptop that can reach a VPS can both push to it and pull from it, so the VPS never needs to reach the laptop.

`onthego doctor laptop` checks the whole chain: SSH, the remote onthego version, and the pinned identity.

### Same network

1. Enable SSH on the receiving device.
   - macOS: System Settings > General > Sharing > Remote Login.
   - Linux: `sudo systemctl enable --now ssh`
2. From the device you type on, install your key and accept the host key once:

   ```sh
   ssh-copy-id me@laptop.local
   ssh me@laptop.local true
   ```

3. Pair it: `onthego device add laptop --host me@laptop.local --install`

An alias from `~/.ssh/config` works as the host too (`--host laptop`). `--port` and `--identity-file` are available; private keys are never copied.

### Anywhere with Tailscale

Use Tailscale when your devices are not on the same network, for example a laptop on the road and a desktop at home. Tailscale gives every device a stable name; ONTHEGO looks it up for you.

1. Install Tailscale on each device and sign in with the same account ([download](https://tailscale.com/download); on Linux: `curl -fsSL https://tailscale.com/install.sh | sh && sudo tailscale up`).
2. Enable SSH on each device you pair to, as above. On Linux you can instead turn on Tailscale SSH with `sudo tailscale set --ssh`.
3. List your tailnet and pair by name:

   ```sh
   onthego tailscale
   onthego device add laptop --tailscale me@laptop --install
   ```

`onthego tailscale` shows which devices are online, which are already paired, and what is missing. ONTHEGO only reads `tailscale status`; it never changes Tailscale settings. See the [Tailscale guide](docs/tailscale.md) for MagicDNS, Tailscale SSH "check" mode, and troubleshooting.

### A VPS

```sh
onthego device add vps --host me@vps.example.com --install
onthego sync vps
```

`--install` detects the server's OS and CPU and copies the matching binary to `~/.local/bin/onthego` there. No Node.js or internet access is needed on the server. After you upgrade onthego locally, run `onthego device install vps` to upgrade the server to the same version.

Work on the VPS over SSH or in a terminal multiplexer as usual. When you come back, run `onthego sync vps` from your laptop or desktop.

### Where the project lives on each device

By default the project goes to the same path relative to the home directory (`~/projects/my-app` on macOS becomes `/home/me/projects/my-app` on Linux). Agent session paths are remapped to match. To choose another location:

```sh
onthego device path laptop /Users/me/code/my-app
onthego device list                               # shows each device and this project's path there
```

For the first transfer, the destination directory should not exist yet. To adopt an existing copy, use `push --replace` once (a Git backup of the destination is kept).

## What moves

| Data | Behavior |
| --- | --- |
| Git history | Only missing objects travel, in an incremental bundle. No Git hosting needed. |
| Staged and unstaged work | Preserved separately, including binary files and executable bits. |
| Untracked files | Included, subject to `exclude` rules and the secret policy. |
| Ignored files | Only when you add them with `--include` or project policy. |
| Secret candidates such as `.env`, `.npmrc` | Excluded and reported. `--include-secret` carries a file encrypted, outside Git objects. |
| Codex and Claude sessions | Sessions for this project active in the last 7 days. Paths are remapped; conversation content is not rewritten. |
| Destination Git config, hooks, remotes, stashes | Left as they are. |
| `node_modules`, caches, build output | Left as they are on each device. |
| Agent logins, global databases, running processes | Never transferred. Sign in to the agent on each device. |

Everything travels in one package that is encrypted for the receiving device (age) and signed by the sender (Ed25519). Pairing pins both identities, so a device that changes keys is rejected.

Stop agents and other processes that write to the project before a handoff, so they cannot race with it. Agent transcripts can contain sensitive prompts or tool output; they are encrypted in transit and stored as the agent would store them. Anything already committed to Git travels as recorded, including secrets in history.

## Day to day

```sh
onthego status                     # did anything change since the last handoff? who has the work?
onthego status laptop              # the same, for the laptop's copy
onthego push laptop --dry-run      # preview files and sessions without sending
onthego push laptop --session-days 2
onthego push laptop --session <id> # or --all-sessions, --no-sessions
onthego push laptop --include scratch/notes.txt --include-secret .env
```

After a push, `status` on the sending device reminds you that the work was handed to the other device. Pull (or `sync`) before editing there again.

### Conflicts and recovery

- **Workspace.** When both devices changed, `sync`, `push`, and `pull` stop with exit code 6. `--replace` overwrites the other side after saving a Git backup there. `onthego backups list` shows backups, `onthego undo` restores the latest one, and `onthego backups prune --keep 3` trims them. The newest five are kept automatically.
- **Agent sessions.** Identical sessions are skipped and a longer incoming history updates an older one. A diverged session keeps the destination's copy and saves the incoming one under `~/.onthego/conflicts/`. Change this with `--session-conflict keep-both|theirs|skip`.
- **Partial delivery.** If the workspace arrives but an agent home cannot be written, the sessions are kept privately and `status` says so. Run `onthego sessions recover` once the problem is fixed; later workspace edits are preserved.

## Configuration

`onthego init` writes `.onthego.yaml`. Commit it so every device uses the same project ID:

```yaml
schema_version: 2
project_id: generated-by-onthego-init
include:
  - .onthego.yaml
exclude:
  - .git
  - .onthego.local
  - node_modules
  - vendor
  - dist
  - build
  - .cache
  - __pycache__
agent: auto        # auto, codex, claude, or none (workspace only)
```

Paths passed with `--include` or `--include-secret` stay managed in later handoffs, including deletions. Adding a managed file to `exclude` stops sending it and leaves the receiving copy in place with a warning.

Private state lives in `~/.onthego` (or `$ONTHEGO_HOME`), with `0700` directories and `0600` files: paired devices in `config.yaml`, keys in `identity/`, and per-project paths under `projects/`. ONTHEGO never loads `.env` files.

Shell completion, including device names:

```sh
onthego completion zsh > "${fpath[1]}/_onthego"   # also: bash, fish
```

## Offline handoff

Without a network path, hand off through an encrypted file on a USB drive or any other channel:

```sh
onthego identity                                   # on the receiving device: prints its public keys
onthego snapshot pack --output /tmp/my-app.otg --recipient age1...

# On the receiving device:
onthego snapshot restore /tmp/my-app.otg --output ~/projects/my-app --signing-public <sender-signing-key>
```

## Commands

| Command | Purpose |
| --- | --- |
| `sync <device>` | Push or pull, whichever brings both devices up to date. |
| `push <device>`, `pull <device>` | Hand off in an explicit direction. |
| `status [device]` | Show changes since the last handoff and who has the work. |
| `inspect` | Preview selected files, excluded secrets, and sessions. |
| `init` | Set up the current Git project. |
| `device add/list/path/install/remove` | Pair devices, map project paths, install or upgrade onthego on them. |
| `tailscale` | Check Tailscale and list tailnet devices to pair. |
| `doctor [device]` | Check Git, SSH, Tailscale, agents, and a device connection. |
| `undo`, `backups list/prune` | Restore or manage workspace backups. |
| `sessions recover` | Retry an agent session import after a partial handoff. |
| `snapshot pack/restore` | Offline handoff through an encrypted file. |
| `identity`, `config path`, `completion`, `version` | Keys, settings location, shell completion, build info. |

`--json` prints structured results and errors, `--verbose` prints transfer phases on stderr.

| Exit status | Meaning |
| --- | --- |
| 0 | Success (including a delivered workspace with pending session recovery) |
| 1 | `E_INTERNAL` |
| 2 | `E_INPUT` |
| 3 | `E_PRECONDITION`, `E_UNSUPPORTED_GIT`, `E_SECRET_POLICY`, `E_SESSION_FORMAT` |
| 4 | `E_TARGET_AUTH`, `E_TARGET_UNAVAILABLE` (SSH login, host key, or reachability) |
| 5 | `E_PACKAGE_INVALID`, `E_RESTORE_FAILED` |
| 6 | `E_GENERATION_CONFLICT`, `E_SOURCE_CHANGED` |
| 130 | `E_CANCELED` |

## Upgrading

0.4 keeps the 0.3.1 wire protocol and file formats, so a 0.4 device can hand off with a 0.3.1 device. Keeping every device on the same version is still the simplest: after `npm update -g @jbaehova/onthego`, run `onthego device install <name>` for each SSH device. `onthego remote set` still works but is replaced by `onthego device path`. See the [release notes](docs/releases/0.4.0.md) and [migration notes](docs/migration.md).

## More

- [Tailscale guide](docs/tailscale.md)
- [Agent compatibility](docs/agent-compatibility.md): tested Codex and Claude Code versions and how resume is verified.
- [Architecture](docs/architecture.md): how capture, encryption, in-place application, and receipts work.
- [Development](docs/development.md): tests, demos, end-to-end checks, and benchmarks.

Continuous background synchronization is intentionally out of scope: handoffs are explicit, so you always know which device has the work.

## License

[MIT](LICENSE)
