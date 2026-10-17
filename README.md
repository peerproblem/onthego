<p align="center">
  <img src="https://raw.githubusercontent.com/peerproblem/onthego/main/assets/onthego-hero.png" alt="Start on your desktop. Finish on your laptop. onthego moves your uncommitted work and your Codex and Claude Code sessions to your other machine." width="100%">
</p>

<h3 align="center">Start on your desktop. Finish on your laptop.</h3>

<p align="center">
  onthego copies your uncommitted work and your Codex and Claude Code sessions<br>
  to your other machine. Then you pick up the same session there.
</p>

<p align="center">
  <a href="https://www.npmjs.com/package/@jbaehova/onthego"><img alt="npm" src="https://img.shields.io/npm/v/%40jbaehova%2Fonthego?style=flat-square&color=1d1d1f&label=npm"></a>
  <a href="https://github.com/peerproblem/onthego/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/peerproblem/onthego/ci.yml?branch=main&style=flat-square&label=ci"></a>
  <img alt="macOS and Linux" src="https://img.shields.io/badge/macOS%20%2B%20Linux-1d1d1f?style=flat-square">
  <img alt="No account, no server" src="https://img.shields.io/badge/no%20account%2C%20no%20server-0071e3?style=flat-square">
  <a href="LICENSE"><img alt="MIT license" src="https://img.shields.io/badge/license-MIT-1d1d1f?style=flat-square"></a>
</p>

<p align="center">
  <a href="https://peerproblem.github.io/onthego/"><b>Website</b></a>
  &nbsp;&nbsp;/&nbsp;&nbsp;
  <a href="#quick-start"><b>Quick start</b></a>
  &nbsp;&nbsp;/&nbsp;&nbsp;
  <a href="docs/tailscale.md"><b>Tailscale</b></a>
  &nbsp;&nbsp;/&nbsp;&nbsp;
  <a href="docs/releases/0.4.0.md"><b>What's new</b></a>
</p>

<br>

```sh
npm install -g @jbaehova/onthego
onthego sync laptop
```

That's the whole idea. Staged, unstaged, and untracked work arrives exactly as you left it, together with this project's agent sessions. On the other device, run `codex resume` or `claude --resume` and keep talking.

## How it works

| | |
| :-- | :-- |
| **Capture** | Staged, unstaged, and untracked files, plus this project's recent agent sessions. Your `.env` stays put. |
| **Encrypt** | Each handoff is encrypted for the receiving device and signed by the sender. |
| **Send** | Over your own SSH. Same Wi-Fi, Tailscale, or a VPS. No account, no relay. |
| **Resume** | Run `codex resume` or `claude --resume` on the other machine. Same conversation. |

Your Git history stays clean, with no "wip" commits. `node_modules`, caches, Git config, hooks, and agent logins stay on each machine.

## Install

```sh
npm install -g @jbaehova/onthego
```

Ships native binaries for macOS and Linux (arm64 and amd64). Requires Git and OpenSSH.

<details>
<summary><b>Other ways to install</b></summary>
<br>

**Without Node.js** (servers, minimal machines). Downloads the release binary, verifies its SHA-256 checksum, and installs to `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/peerproblem/onthego/main/install.sh | sh
```

**From source** with Go 1.26 or newer:

```sh
git clone https://github.com/peerproblem/onthego.git && cd onthego
make build && install -m 755 dist/onthego ~/.local/bin/onthego
```

</details>

You only install it once by hand. `onthego device add <name> --install` copies the exact same version to your other devices over SSH.

## Quick start

**1. On your desktop**, inside a Git project:

```sh
onthego init                                            # once per project; commit .onthego.yaml
onthego device add laptop --host me@laptop.local --install
onthego sync laptop
```

**2. On the laptop**, continue where you stopped. The handoff prints the exact commands:

```sh
cd ~/projects/my-app
codex resume <session-id>                               # or: claude --resume <session-id>
```

**3. Back at the desktop**, bring the work home:

```sh
onthego sync laptop
```

> [!TIP]
> Run every command from the device that can reach the other one. A laptop that can reach a VPS can both send work to it and bring work back, so the VPS never needs to reach the laptop.

## How `sync` decides

<p align="center">
  <img src="https://raw.githubusercontent.com/peerproblem/onthego/main/assets/onthego-sync-decision.png" alt="sync pushes when only this side changed, pulls when only the other side changed, and stops when both changed" width="100%">
</p>

`sync` compares both sides with their last handoff and only moves work away from the side that has not changed.

| Situation | `sync` does |
| :-- | :-- |
| The project is not on the other device yet | **push** |
| Only this device changed | **push** |
| Only the other device changed | **pull** |
| You handed the work off and have not touched it since | **pull**, to take it back |
| Nothing changed | nothing |
| Both sides changed | **stops** and asks you to pick `push --replace` or `pull --replace` |

Nothing is merged behind your back. Whenever work is replaced, the previous state is kept as a Git backup that `onthego undo` restores. Preview any decision with `onthego sync laptop --dry-run`, or choose the direction yourself with `push` and `pull`.

## Connect your devices

onthego uses your normal SSH setup: `~/.ssh/config`, keys, and `known_hosts`. All it needs is one device that can `ssh` into the other without a password prompt. `onthego doctor <device>` checks the whole chain.

### Same network

```sh
# On the laptop: turn on SSH
#   macOS: System Settings > General > Sharing > Remote Login
#   Linux: sudo systemctl enable --now ssh

# On the desktop:
ssh-copy-id me@laptop.local
onthego device add laptop --host me@laptop.local --install
```

An alias from `~/.ssh/config` also works (`--host laptop`), as do `--port` and `--identity-file`. Private keys are never copied.

### Anywhere, with Tailscale

When your devices are on different networks, Tailscale gives each one a stable name and onthego looks it up for you.

```sh
onthego tailscale                                       # checks Tailscale, lists your devices
onthego device add laptop --tailscale me@laptop --install
```

Install Tailscale on each device and sign in with the same account. Each device you pair to still needs SSH turned on (or Tailscale SSH on Linux). onthego only reads `tailscale status` and never changes your settings. See the [Tailscale guide](docs/tailscale.md) for MagicDNS, Tailscale SSH, and troubleshooting.

### A VPS

```sh
onthego device add vps --host me@vps.example.com --install
onthego sync vps
```

`--install` detects the server's OS and CPU and copies the matching binary to `~/.local/bin/onthego`. The server needs neither Node.js nor internet access. After upgrading locally, `onthego device install vps` brings the server to the same version.

### Where the project lands

By default, the same path relative to your home directory: `~/projects/my-app` on macOS becomes `/home/me/projects/my-app` on Linux, and agent session paths are remapped to match.

```sh
onthego device path laptop /Users/me/code/my-app        # choose another location
onthego device list                                     # devices and this project's path on each
```

The first destination directory should not exist yet. To adopt an existing copy, use `push --replace` once; a Git backup of the destination is kept.

## What moves

| | |
| :-- | :-- |
| **Git history** | Only the objects the other side is missing. No Git hosting needed. |
| **Staged and unstaged work** | Kept separate, including binary files and executable bits. |
| **Untracked files** | Included, subject to `exclude` rules and the secret policy. |
| **Ignored files** | Only when you add them with `--include` or project policy. |
| **Agent sessions** | This project's Codex and Claude sessions from the last 7 days. Paths are remapped; content is not rewritten. |
| **Secrets** such as `.env`, `.npmrc` | Excluded and reported. `--include-secret` carries a file encrypted, outside Git objects. |
| **Stays on each device** | `node_modules`, caches, build output, Git config, hooks, remotes, stashes, agent logins, and running processes. |

Each handoff is one package, encrypted for the receiving device with [age](https://age-encryption.org) and signed by the sender with Ed25519. Pairing pins both identities, so a device whose keys change is rejected.

> [!NOTE]
> Stop agents and other processes that write to the project before a handoff. Agent transcripts can contain sensitive prompts or tool output; they are encrypted in transit and stored as the agent would store them. Anything already committed to Git travels as recorded.

## Everyday commands

```sh
onthego status                       # changed since the last handoff? which device has the work?
onthego status laptop                # the same, for the laptop's copy
onthego sync laptop --dry-run        # show the decision without moving anything
onthego push laptop --dry-run        # preview files and sessions
onthego push laptop --session-days 2 # or --session <id>, --all-sessions, --no-sessions
onthego push laptop --include scratch/notes.txt --include-secret .env
```

### When something goes wrong

- **Both devices changed.** `sync`, `push`, and `pull` stop with exit code 6. `--replace` overwrites the other side after saving a Git backup there.
- **Undo a handoff.** `onthego backups list` shows backups and `onthego undo` restores the latest. The newest five are kept automatically; `onthego backups prune --keep 3` trims them.
- **Diverged agent session.** The destination keeps its copy and the incoming one is saved under `~/.onthego/conflicts/`. Change this with `--session-conflict keep-both|theirs|skip`.
- **Sessions could not be written.** The workspace still arrives, the sessions are kept privately, and `status` tells you. Fix the cause and run `onthego sessions recover`.
- **SSH trouble.** Errors name the problem and the fix: a missing key, an unknown host key, a refused or unreachable host, or onthego missing on the device.

## Reference

<details>
<summary><b>Configuration</b></summary>
<br>

`onthego init` writes `.onthego.yaml`. Commit it so every device shares the same project ID.

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

Private state lives in `~/.onthego` (or `$ONTHEGO_HOME`) with `0700` directories and `0600` files: paired devices in `config.yaml`, keys in `identity/`, and per-project paths under `projects/`. onthego never loads `.env` files.

Shell completion, including device names:

```sh
onthego completion zsh > "${fpath[1]}/_onthego"         # also: bash, fish
```

</details>

<details>
<summary><b>Offline handoff</b></summary>
<br>

Without a network path, hand off through an encrypted file on a USB drive or any other channel.

```sh
onthego identity                                        # on the receiving device: prints its public keys
onthego snapshot pack --output /tmp/my-app.otg --recipient age1...

# On the receiving device:
onthego snapshot restore /tmp/my-app.otg --output ~/projects/my-app --signing-public <sender-signing-key>
```

</details>

<details>
<summary><b>All commands</b></summary>
<br>

| Command | Purpose |
| :-- | :-- |
| `sync <device>` | Push or pull, whichever brings both devices up to date. |
| `push <device>`, `pull <device>` | Hand off in an explicit direction. |
| `status [device]` | Changes since the last handoff, and which device has the work. |
| `inspect` | Preview selected files, excluded secrets, and sessions. |
| `init` | Set up the current Git project. |
| `device add / list / path / install / remove` | Pair devices, map project paths, install or upgrade onthego on them. |
| `tailscale` | Check Tailscale and list tailnet devices to pair. |
| `doctor [device]` | Check Git, SSH, Tailscale, agents, and a device connection. |
| `undo`, `backups list / prune` | Restore or manage workspace backups. |
| `sessions recover` | Retry an agent session import after a partial handoff. |
| `snapshot pack / restore` | Offline handoff through an encrypted file. |
| `identity`, `config path`, `completion`, `version` | Keys, settings location, shell completion, build info. |

Every command accepts `--json` for structured output and errors, and `--verbose` for transfer phases on stderr.

</details>

<details>
<summary><b>Exit codes</b></summary>
<br>

| Exit | Meaning |
| :-: | :-- |
| 0 | Success, including a delivered workspace with pending session recovery |
| 1 | `E_INTERNAL` |
| 2 | `E_INPUT` |
| 3 | `E_PRECONDITION`, `E_UNSUPPORTED_GIT`, `E_SECRET_POLICY`, `E_SESSION_FORMAT` |
| 4 | `E_TARGET_AUTH`, `E_TARGET_UNAVAILABLE` (SSH login, host key, or reachability) |
| 5 | `E_PACKAGE_INVALID`, `E_RESTORE_FAILED` |
| 6 | `E_GENERATION_CONFLICT`, `E_SOURCE_CHANGED` |
| 130 | `E_CANCELED` |

</details>

## Upgrading

0.4 keeps the 0.3.1 wire protocol and file formats, so 0.4 and 0.3.1 devices can hand off to each other. Keeping every device on one version is still simplest:

```sh
npm update -g @jbaehova/onthego
onthego device install laptop                           # repeat for each SSH device
```

`onthego remote set` still works but is replaced by `onthego device path`. See the [0.4.0 release notes](docs/releases/0.4.0.md) and the [migration notes](docs/migration.md).

## Documentation

| | |
| :-- | :-- |
| [Tailscale guide](docs/tailscale.md) | Pairing across networks, Tailscale SSH, troubleshooting. |
| [Agent compatibility](docs/agent-compatibility.md) | Tested Codex and Claude Code versions, and how resume is verified. |
| [Architecture](docs/architecture.md) | Capture, encryption, in-place application, and receipts. |
| [Development](docs/development.md) | Tests, demos, end-to-end checks, benchmarks, and releasing. |

Handoffs are explicit on purpose: you always know which device has the work. Continuous background synchronization is out of scope.

## License

[MIT](LICENSE)
