# ONTHEGO 0.3 architecture

ONTHEGO transfers unfinished work and native coding-agent histories directly between personal devices. Git owns workspace state. SSH provides reachability and authenticated pairing. No ONTHEGO service or relay participates.

## Workspace representation

Capture uses temporary Git indexes to create trees for the staged index, tracked working files, and explicitly managed supplemental files. Explicitly selected supplemental secrets are excluded from these Git trees and travel as separate signed package entries. A WIP commit references the trees and the original HEAD. A previous handoff WIP commit joins the ancestry graph, so multi-device history is expressed by Git ancestry instead of an ever-growing list of snapshot IDs.

The snapshot manifest uses schema 2. Its signed metadata names the WIP objects and selected files. The workspace's normal index, branch, and working files are unchanged by capture. Internal refs keep required WIP objects reachable.

Before SSH transfers, the recipient reports its known Git tips. Capture excludes objects reachable from known tips. An offline package remains self-contained. History has a separate transport limit from workspace and transcript data, so a large repository history does not consume the supplemental payload budget. Git bundles and session data are encrypted together using age and authenticated with an Ed25519-signed manifest.

## In-place application

The receiver decrypts into a private temporary directory and validates signatures, object identity, project identity, hashes, and paths. Incoming refs live below `refs/onthego/incoming/`. Existing destination branches and local Git configuration are not replaced by a clone.

Git ancestry and a saved workspace fingerprint gate replacement. Only managed leaves are updated or removed. Unmanaged dependency directories and caches are not traversed to copy or back them up. The project directory and `.git` retain their identities, including linked-worktree metadata.

Before mutation, a Git backup records the destination HEAD, branch, index, working files, and managed supplemental files. Backups are bounded to five by default and exposed through `backups list/prune` and `undo`. A replacement failure can use the retained state for recovery. Selected supplemental secrets have separate age-encrypted backup artifacts under private ONTHEGO state. Their plaintext never enters a Git object. Undo decrypts those artifacts in leased temporary storage, and pruning removes artifacts with their corresponding refs. Session histories remain outside ordinary Git workspace backups.

## Receipts and active devices

Private state is keyed by canonical project path and project ID. The current baseline records the workspace last imported or acknowledged. Per-peer records preserve what each recipient received. A successful push marks that recipient as the active device and tells the source user to pull before editing or resuming sessions.

Exports first create pending receipts. The authenticated recipient's status can promote a pending receipt after an acknowledgment was lost. Reapplying the same snapshot is idempotent, even if the destination has since been edited. Offline return transfers can use pending WIP ancestors. Pending receipts expire after 30 days and are limited to 64 per workspace.

Workspace fingerprints and session fingerprints have different purposes. Session changes are shown in status but do not make a workspace transfer fail. Session conflicts have their own preservation policy.

## Native histories

The Codex and Claude adapters select project-scoped JSONL records. Recent activity is the default scope. Explicit IDs and all-history selection are available. Discovery caches hashes using file identity, size, ctime, and mtime; the cache never retains plaintext transcripts.

The adapter preserves native bytes except for recognized filesystem path string values. Message bodies and opaque tool content are not rewritten. Claude storage paths use the remapped session working directory, including subdirectories and long-path encoding.

An incomplete final line is omitted with a warning. A malformed complete line excludes that session. Identical imports are skipped; incoming append-only extensions update older histories. Newer local histories are retained. Divergence saves a private incoming conflict copy and leaves the local history in place by default.

Agent authentication, global databases, and unrelated projects never travel. The recipient authenticates its installed agent independently. Supported versions and actual CLI resume evidence are documented in [agent compatibility](agent-compatibility.md).

## Pairing and protocol

A device stores host, transport settings, and pinned public identity independently from projects. Project mappings live in `~/.onthego/projects/<id>/peers.yaml`; the default path preserves the source's home-relative location.

Pairing queries `peer hello` and public identity over trusted SSH. The peer pins the sender as part of the same pairing operation. `peer apply --from` resolves its signer locally and does not accept a caller-supplied replacement key. Protocol major version and capabilities are checked before transfer. Errors contain a stable code, message, and optional preserved path.

SSH host-key checks remain enabled. Existing private SSH keys remain in their original location. Remote executable discovery uses the login shell and saves an absolute executable path.

## Filesystem boundaries and lifecycle

User-selected root aliases are canonicalized once, including macOS `/tmp` and `/var`. Project-internal symlink parents remain prohibited for file application. Shared filesystem helpers implement canonicalization, parent checks, regular-file copying, hashing, and locks.

Kernel `flock` releases locks when a process exits. Lock files retain a stable inode and are not used as stale-presence sentinels. SIGINT and SIGTERM cancel the command context so deferred cleanup can run. Plaintext staging directories live under `~/.onthego/tmp`, have private permissions, and hold leases. Startup cleanup removes old inactive staging directories while preserving active leases.

## Storage

| Location | Contents |
| --- | --- |
| `.onthego.yaml` | Trackable project identity and selection policy. |
| `~/.onthego/config.yaml` | Devices and paired identities. |
| `~/.onthego/identity/keys.json` | Local private signing and encryption keys. |
| `~/.onthego/projects/<id>/peers.yaml` | Project path mappings. |
| `~/.onthego/projects/<id>/handoff/` | Current, peer, and pending receipts. |
| `~/.onthego/tmp/` | Leased temporary data removed after use. |
| `~/.onthego/conflicts/` | Preserved incoming divergent histories. |
| `~/.onthego/session-recovery-*/` | Private recovery copies when an adapter cannot import. |
| `~/.onthego/backups/` | Encrypted supplemental-secret backup artifacts. |
| `refs/onthego/` | WIP ancestry, incoming objects, and bounded backups. |

A local-path device runs the same protocol without SSH. Shared agent homes are detected so a backup handoff cannot overwrite its own source history.

## Boundaries

Stop writers during capture and application. ONTHEGO does not merge simultaneous workspace changes. Tailscale is an optional connectivity choice. Continuous synchronization, additional agents, and asynchronous user-owned storage adapters are outside the 0.3 release.
