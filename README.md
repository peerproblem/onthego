# ONTHEGO

Continue your coding-agent conversation on another device, with your unfinished Git workspace intact.

ONTHEGO is an MIT-licensed CLI for explicit handoffs between your own computers. It carries staged and unstaged work together with project-scoped Codex CLI and Claude Code histories. Each destination keeps its Git settings, hooks, local dependencies, and editor directory intact.

No ONTHEGO account or central server is needed. SSH connects your devices. Install and authenticate the coding agent separately on each device. This project targets individual CLI users.

## Install

```sh
npm install -g @jbaehova/onthego@^0.3.0
```

The npm package includes binaries for macOS arm64/amd64 and Linux arm64/amd64. Git and OpenSSH are required. Node.js is needed only for the npm launcher. Install the same ONTHEGO version on both devices.

You can also build from source with Go 1.25.4 or newer:

```sh
git clone https://github.com/peerproblem/onthego.git
cd onthego
make build
mkdir -p ~/.local/bin
install -m 755 dist/onthego ~/.local/bin/onthego
# Or: go install github.com/peerproblem/onthego/cmd/onthego@latest
```

## Desktop to laptop

Enable SSH on the destination and establish a trusted SSH connection first. An existing SSH alias can use a LAN address or your own VPS. Tailscale and other VPNs are optional ways to provide reachability. Both devices must be reachable during a direct transfer.

Inside the project on the desktop:

```sh
onthego init
onthego device add laptop --host me@my-laptop
onthego remote set laptop /Users/me/projects/my-app
onthego doctor laptop
onthego push laptop --dry-run

# Stop agents and other processes that write project files, then:
onthego push laptop --verbose
```

`device add` pairs both public identities through your trusted SSH connection. It discovers the remote executable using a login shell. Use `--binary /absolute/path/onthego` when needed. `--port` and `--identity-file` select an existing SSH connection; private SSH keys are never copied.

Devices store hosts and identities once. Project paths are separate mappings under `~/.onthego/projects/<id>/peers.yaml`. Without a mapping, the path relative to your home directory is used under the remote home. `device add ... --path ...` remains a shortcut for the current project's mapping.

For the first transfer, choose a destination directory that does not exist. On the laptop:

```sh
cd /Users/me/projects/my-app
onthego status
codex resume <session-id>
# Or: claude --resume <session-id>
```

Use the session ID printed by the handoff. These commands resume a native history, not a running process. See the exact tested CLI versions and validation boundaries in [agent compatibility](docs/agent-compatibility.md).

To bring laptop changes back, run this from the desktop project:

```sh
onthego pull laptop --verbose
```

The destination is updated in place. Existing shells, editor watchers, and local dependency directories continue to refer to the same project directory. Stop writers during the transfer so they cannot race with capture or application.

After a successful push, `status` warns that the project and sessions were handed to the other device. Pull before editing or resuming them on the original device.

## What moves

| Data | Behavior |
| --- | --- |
| Git history | Missing objects travel in a negotiated bundle. GitHub is optional. |
| Staged and unstaged work | Temporary Git trees preserve their distinction, binary content, and executable bits. |
| Untracked project files | Included subject to project exclusions and the secret policy. |
| Ignored files | Include explicitly with `--include` or project policy. |
| Secret candidates such as `.env` | Excluded and reported; eligible files require `--include-secret`. |
| Codex and Claude histories | Recent project sessions by default; path values are remapped without rewriting conversation content. |
| Destination Git configuration and hooks | Preserved in place, together with remotes, reflogs, and existing stashes. |
| Destination caches and local dependencies | Left in place when they are outside the managed file set. |
| Agent logins, global databases, and running processes | Not transferred. Authenticate and run the agent separately on each device. |

Native transcripts may contain sensitive prompts or tool output. ONTHEGO preserves them inside an encrypted package for resumability. All tracked Git content and history also travel as recorded, including anything previously committed. Supplemental secret-file rules cannot remove secrets from Git history.

## Sessions and conflicts

The default selection is sessions active in the last seven days. Choose a narrower or broader set explicitly:

```sh
onthego push laptop --session <id>
onthego push laptop --session-days 2
onthego push laptop --all-sessions
onthego pull laptop --sessions skip
```

Malformed completed JSONL lines exclude that session with a warning. An incomplete final line is discarded with a warning. These histories do not stop the workspace handoff.

Identical sessions are skipped. Incoming extensions update an older destination, while a newer destination is kept. Divergent sessions keep the destination and save the incoming history in private `~/.onthego/conflicts/` storage by default. `--sessions keep-both`, `--sessions theirs`, and `--sessions skip` make the policy explicit. Conflict copies remain until you review and remove them. Plaintext staging copies are removed after import. If an adapter cannot write its destination, the workspace still arrives and its authenticated inbound histories are retained in a private `session-recovery-*` directory under ONTHEGO state. The warning prints its location.

Concurrent workspace edits require a deliberate decision. ONTHEGO checks Git handoff ancestry and the destination's captured workspace state. `--replace` applies incoming managed files after retaining a Git backup; a different project ID is always rejected.

```sh
onthego backups list
onthego undo
onthego backups prune --keep 3
```

The newest five Git backups are retained automatically. Explicitly included supplemental secrets stay outside Git objects; their backup contents are encrypted under the private ONTHEGO state directory. Unmanaged `node_modules`, caches, and local environment files are not copied into backups. Undo restores the backed-up managed state and clears the delivery baseline, since restoring old work does not prove that another device has received it. Use Git to reconcile intentional concurrent development.

## Configuration

Commit the project policy `.onthego.yaml`:

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

`agent` accepts `auto`, `codex`, `claude`, or `none`. `none` performs workspace-only handoffs.

Personal configuration and pinned identities live in `~/.onthego/config.yaml`. Private ONTHEGO keys live in `~/.onthego/identity/keys.json`. Private directories use mode `0700` and files use `0600`. `ONTHEGO_HOME` optionally selects a different state directory. ONTHEGO never automatically loads `.env` files.

```sh
onthego config path
onthego device list
onthego identity
onthego push laptop --include scratch/notes.txt
onthego push laptop --include-secret .env
```

Explicitly selected files remain managed across subsequent handoffs, including deletions. Do not opt in credentials that must stay device-local. Names such as `tokenizer.ts`, `docs-secret-santa.md`, and `design.key` are ordinary project files, not secret candidates.

## Portable snapshots

For removable storage or another delivery channel, create an encrypted `.otg` file:

```sh
onthego snapshot pack --output /tmp/project.otg --recipient age1...

# On the receiving device:
onthego snapshot restore /tmp/project.otg \
  --output /path/to/project \
  --signing-public <trusted-source-public-key>
```

Use the recipient and signing public keys printed by `onthego identity`. Without `--recipient`, packing targets the local device. SSH handoffs resolve both identities from pairing; offline restores require you to authenticate the source signing key.

Offline exports retain pending receipts for up to 30 days, bounded to 64 per workspace. A returning descendant can acknowledge this lineage without `--replace`. Retrying an already applied snapshot succeeds without overwriting later local edits.

## Commands

| Command | Purpose |
| --- | --- |
| `init` | Initialize project policy. |
| `device add/list/remove` | Pair and manage devices. |
| `remote set` | Manage project paths on those devices. |
| `push`, `pull` | Hand off a workspace and selected histories. |
| `status [device]` | Inspect changes and active-device guidance. |
| `inspect` | Preview file selection and excluded secret candidates. |
| `doctor [device]` | Check prerequisites, CLI versions, and optional peer access. |
| `snapshot pack/restore` | Create or receive an offline package. |
| `backups list/prune`, `undo` | Inspect, limit, or restore Git backups. |
| `identity`, `config path`, `version` | Inspect public identity and installation. |

`--json` emits structured results and errors. `--verbose` reports transfer phases on stderr.

## Development

```sh
make check
go test -race ./...
make build
./scripts/demo.sh
./scripts/ssh-e2e.sh
make package
```

The tests and demos isolate project and agent homes. The SSH script uses a real temporary OpenSSH server. See [validation and feedback coverage](docs/progress.md), [architecture](docs/architecture.md), and [migration](docs/migration.md).

This is an explicit handoff tool. Continuous synchronization and user-owned asynchronous storage adapters remain future work.

## License

[MIT](LICENSE).
