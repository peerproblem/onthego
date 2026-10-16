# ONTHEGO

Take your project and coding-agent sessions from your desktop to your laptop, or to a VPS you own.

ONTHEGO is an MIT-licensed command-line tool for direct device-to-device workspace handoffs. It transfers Git history, staged and unstaged changes, untracked files, selected ignored files, and project-scoped Codex and Claude session history. Each device keeps its configuration in `~/.onthego/`.

**No ONTHEGO account, central server, hosted database, or OpenAI API key is required.** SSH connects your own devices. Your coding agents keep their own installation and login requirements.

This branch introduces the 0.2 workflow. Previously published 0.1 packages use the old cloud workflow; build this checkout until a 0.2 release is published.

## Install

Build the current checkout with Go 1.25.4 or newer:

```sh
git clone https://github.com/peerproblem/onthego.git
cd onthego
make build
mkdir -p ~/.local/bin
install -m 755 dist/onthego ~/.local/bin/onthego
```

Put `~/.local/bin` on your `PATH`. Install the same ONTHEGO version on both devices. Git and OpenSSH must be installed; Node.js is only needed for the npm launcher.

The release package remains `@jbaehova/onthego` and installs the `onthego` command:

```sh
# After the 0.2 release is published:
npm install -g @jbaehova/onthego@^0.2.0
```

Prebuilt release targets: macOS Apple silicon and Intel, Linux arm64 and amd64. No Windows binary is currently packaged.

## Desktop → laptop

Enable SSH on the destination and establish a trusted SSH connection first. ONTHEGO uses your existing SSH configuration and known-hosts file. An SSH alias can point to a LAN address, a VPN address, or a VPS.

On your desktop, inside the Git project:

```sh
onthego init
onthego device add laptop --host me@my-macbook --path /Users/me/projects/my-app
onthego doctor laptop
onthego push laptop --dry-run

# Stop the project's agents and editors that are writing files, then:
onthego push laptop
```

Use a destination path that does not yet exist for the first transfer. Device registration reads and pins the peer's encryption and signing identities over trusted SSH. `--port`, `--identity-file`, and `--binary` let you select an SSH port, existing SSH private key, and absolute remote ONTHEGO executable path. The private key stays in its existing location; ONTHEGO stores its path.

On the laptop:

```sh
cd /Users/me/projects/my-app
onthego status

# Use a session ID reported by ONTHEGO with your installed agent:
codex resume <session-id>
# or: claude --resume <session-id>
```

These are native session histories, not running processes. Resume targets the installed agent CLI; desktop-app sidebar indexes and databases are not imported. Stop an agent before handing off, and resume it on the destination using that device's own agent login. The desktop can later retrieve laptop changes:

```sh
cd /path/to/original/my-app
onthego pull laptop
# Re-enter the restored directory after replacement:
cd /path/to/original/my-app
```

A handoff refuses conflicting destination edits. `--replace` explicitly replaces the workspace and retains the previous directory as a sibling backup. It does not merge simultaneous work. Use Git branches and merging for concurrent development.

Receiving a handoff replaces the project directory. An already-open shell can still point to the backup directory: run the printed `cd` command before continuing, and reopen editors that were watching the old directory.

## What moves

| Data | Behavior |
| --- | --- |
| Git commits and refs | Included in a Git bundle; GitHub is optional. |
| Index and working tree | Staged and unstaged patches preserve the distinction. |
| Untracked project files | Included subject to exclusions and secret policy. |
| Ignored files | Opt in with `--include` or project `include`. |
| Untracked/ignored secret files such as `.env` | Excluded by default; explicit `--include-secret` is required. |
| Codex and Claude sessions | Project-scoped JSONL history, with workspace paths remapped. |
| Agent logins, auth stores, SSH keys | Excluded from session and supplemental-file capture; authenticate on each device. |
| Running processes, terminal state, agent SQLite databases | Not migrated. |
| Git hooks, local config, reflogs | Not copied; older stashes reachable only through reflogs are not transferred. |

Native session transcripts can contain sensitive prompts, tool output, and pasted secrets. They are transferred intact inside the encrypted snapshot to preserve resumability. Only pair devices you trust. This is not a transcript-redaction service.

All tracked content and Git history move unchanged, including any secrets previously committed. Secret-file exclusions govern supplemental files and session-store selection; they cannot erase credentials already present in Git history.

GitHub remains useful for ordinary code collaboration, pull requests, and backup. ONTHEGO does not require a GitHub token, push private session history to GitHub, or automatically create commits. The Git bundle also makes a private, offline, or unpushed repository portable. The original safe Git origin is restored; credential-bearing remote URLs are not transferred.

## Configuration

Commit the project policy file `.onthego.yaml`:

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
agent: auto
```

Set `agent` to `auto`, `codex`, `claude`, or `none` (`none` performs workspace-only handoffs).

Device addresses and pinned public identities live in `~/.onthego/config.yaml`. Private ONTHEGO encryption/signing keys live in `~/.onthego/identity/keys.json`. Private files use mode `0600` and their directories use `0700`. No project `.env` file is loaded. `ONTHEGO_HOME` optionally changes the personal configuration directory, for example in a test environment.

```sh
onthego config path
onthego device list
onthego identity             # Public identity only
onthego device remove laptop
```

To carry a selected ignored file:

```sh
onthego push laptop --include scratch/notes.txt
onthego push laptop --include-secret .env
```

Secret inclusion requires an explicit initial opt-in. Included paths remain managed in subsequent handoffs, including updates and deletions. Do not opt in destination-specific credentials that should stay local, and never put secret values in `.onthego.yaml`. Managed project content follows the source snapshot, including deletions. Destination-local ignored or ONTHEGO-excluded untracked files such as `.env` and caches are preserved when they were not part of the previous or incoming transfer. Replaced destinations remain in `.onthego-backup-*` sibling directories until you remove them.

## Portable snapshots

For removable storage or any transport outside SSH, use an encrypted `.otg` file:

```sh
# Obtain the destination age recipient and signer public keys using `identity`.
onthego snapshot pack --output /tmp/project.otg --recipient age1...

# On the receiving device, provide the source signing public key:
onthego snapshot restore /tmp/project.otg \
  --output /path/to/new/project \
  --signing-public <source-signing-public-key>
```

Snapshots use age encryption, Ed25519 signatures, and content hashes. Without `--recipient`, packing targets this device's encryption identity. For ordinary cross-device use, `push` and `pull` select the pinned identities automatically.

## Commands

| Command | Purpose |
| --- | --- |
| `init` | Register or migrate the current Git project. |
| `device add/list/remove` | Manage trusted destinations in personal configuration. |
| `push <device>` | Send this workspace and its native agent sessions. |
| `pull <device>` | Retrieve a peer workspace into this directory or `--output`. |
| `status [device]` | Inspect the workspace and optional peer. |
| `inspect` | Preview file selection for a local snapshot. |
| `doctor [device]` | Check local prerequisites and optional SSH access. |
| `snapshot pack/restore` | Create or restore a portable encrypted handoff. |
| `identity`, `config path`, `version` | Inspect public identity, storage location, and build version. |

## Development and validation

```sh
make check
make build
./scripts/demo.sh
make package
```

The demo creates disposable repositories and an isolated `ONTHEGO_HOME`. It exercises an encrypted local-device handoff without any API keys, network service, or access to your own agent sessions.

`make package` builds all four platform binaries and validates the npm package. The release workflow publishes only tagged versions. Creating or publishing a release is a separate maintainer action.

See [architecture and implementation plan](docs/architecture.md), [demo](docs/demo.md), [migration](docs/migration.md), and [implementation status](docs/progress.md).

## Current boundaries

The current workflow is an explicit `push`/`pull` handoff. Automatic continuous two-way synchronization, conflict merging, live process migration, agent database migration, and additional native session adapters are future work. Keep agents stopped while capturing or applying a handoff. Session compatibility depends on the installed agent's session format; use matching recent agent versions on both devices. The current snapshot format limits the expanded payload to 250 MiB and 20,000 archive entries; large repositories or session histories may exceed that limit.

## License

[MIT](LICENSE).
