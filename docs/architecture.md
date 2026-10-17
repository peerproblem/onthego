# ONTHEGO architecture

ONTHEGO transfers unfinished work and native coding-agent histories directly between personal devices. Git owns workspace state. SSH provides reachability and authenticated pairing. No ONTHEGO service or relay participates.

## Workspace representation

Capture uses temporary Git indexes to create trees for the staged index, tracked working files, and explicitly managed supplemental files. Explicitly selected supplemental secrets are excluded from these Git trees and travel as separate signed package entries. A WIP commit references the trees and the original HEAD. A previous handoff WIP commit joins the ancestry graph, so multi-device history is expressed by Git ancestry instead of an ever-growing list of snapshot IDs.

The snapshot manifest uses schema 3. Its signed metadata names the WIP objects and selected files, exact payload sizes, and supplemental paths whose receiver copies must remain after management ends. Schema 2 packages remain readable. The workspace's normal index, branch, and working files are unchanged by capture. Internal refs keep required WIP objects reachable.

Before SSH transfers, the recipient reports its known Git tips. Capture excludes objects reachable from known tips. An offline package remains self-contained. History has a separate transport limit from workspace and transcript data, so a large repository history does not consume the supplemental payload budget. Git bundles and session data are encrypted together using age and authenticated with an Ed25519-signed manifest.

## In-place application

The receiver authenticates the manifest and signature from the first two archive entries before writing payloads into a private temporary directory. Signed names and sizes gate extraction; object identity, project identity, hashes and paths are checked before workspace writes. Incoming refs live below `refs/onthego/incoming/`. Existing destination branches and local Git configuration are not replaced by a clone.

Git ancestry and a saved workspace fingerprint gate replacement. Only managed leaves are updated or removed. Retained paths leave the managed set with a warning and keep their receiving bytes. Filesystem probes detect case and normalization aliases before mutation; individual replacement files and symlinks are staged beside their destination and renamed atomically. Unmanaged dependency directories and caches are not traversed to copy or back them up. The project directory and `.git` retain their identities, including linked-worktree metadata.

Before mutation, a Git backup records the destination HEAD, branch, index, working files, and managed supplemental files. Backups are bounded to five by default and exposed through `backups list/prune` and `undo`. A replacement failure can use the retained state for recovery. Selected supplemental secrets have separate age-encrypted backup artifacts under private ONTHEGO state. Their plaintext never enters a Git object. Undo decrypts those artifacts in leased temporary storage, and pruning removes artifacts with their corresponding refs. Session histories remain outside ordinary Git workspace backups.

## Receipts and active devices

Private state is keyed by canonical project path and project ID. The current baseline records the workspace last imported or acknowledged. Per-peer records preserve what each recipient received. A successful push marks that recipient as the active device and tells the source user to pull before editing or resuming sessions.

Exports first create pending receipts. The authenticated recipient's status can promote a pending receipt after an acknowledgment was lost. Reapplying the same snapshot is idempotent, even if the destination has since been edited. Offline return transfers can use pending WIP ancestors. Pending receipts expire after 30 days and are limited to 64 per workspace. Corresponding `refs/onthego/pending/<snapshotID>` refs keep WIP objects reachable until acknowledgment or expiry, and never enter bundle ref advertisements.

`sync` reads the local receipt and the device's receipt through `peer status` and only moves work away from a side that has not changed since their shared snapshot. When the snapshots differ, the active-device marker tells which side handed its work off. Two changed sides are never merged; the user chooses `push --replace` or `pull --replace`.

Workspace fingerprints and session fingerprints have different purposes. Session changes are shown in status but do not make a workspace transfer fail. Session conflicts have their own preservation policy.

## Native histories

The Codex and Claude adapters select project-scoped JSONL records. Recent activity is the default scope. Explicit IDs and all-history selection are available. Missing explicit IDs fail before packing. `--no-sessions` bypasses discovery and omits the native session payload. Adapter discovery errors become warnings for that adapter; imports validate and lock only included agent homes. Discovery caches hashes using file identity, size, ctime, and mtime; the cache never retains plaintext transcripts.

The adapter preserves native bytes except for recognized filesystem path string values. Message bodies and opaque tool content are not rewritten. Claude storage paths use the remapped session working directory, including subdirectories and long-path encoding.

An incomplete final line is omitted with a warning. A malformed complete line excludes that session. Identical imports are skipped; incoming append-only extensions update older histories. Newer local histories are retained. Divergence saves a private incoming conflict copy and leaves the local history in place by default.

Failed session imports retain authenticated files in private recovery storage and record `session-recovery.json` alongside the current receipt. Replaying the package or running `sessions recover` imports only sessions, updates the session fingerprint, and clears completed recovery data. Workspace edits and their saved fingerprint are preserved.

Agent authentication, global databases, and unrelated projects never travel. The recipient authenticates its installed agent independently. Supported versions and actual CLI resume evidence are documented in [agent compatibility](agent-compatibility.md).

## Pairing and protocol

A device stores host, transport settings, and pinned public identity independently from projects. Project mappings live in `~/.onthego/projects/<id>/peers.yaml`; the default path preserves the source's home-relative location.

Pairing queries `peer hello` and public identity over trusted SSH. The peer pins the sender as part of the same pairing operation. `peer apply --from` resolves its signer locally and does not accept a caller-supplied replacement key. Protocol major version and capabilities are checked before transfer, including `snapshot-v3` and `session-scope`. Errors contain a stable code, message, and optional preserved path.

`device add --install` and `device install` run `uname` over SSH, pick the matching binary (the npm package carries all four platforms next to the running executable), stream it to `~/.local/bin/onthego`, check that it runs, and save that absolute path. SSH failures are classified into `E_TARGET_AUTH` and `E_TARGET_UNAVAILABLE` with the next step in the message. Tailscale pairing reads `tailscale status --json` and stores the MagicDNS name as the SSH host.

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
| `~/.onthego/projects/<id>/handoff/` | Current, peer and pending receipts plus session recovery metadata. |
| `~/.onthego/tmp/` | Leased temporary data removed after use. |
| `~/.onthego/conflicts/` | Preserved incoming divergent histories. |
| `~/.onthego/session-recovery-*/` | Private recovery copies when an adapter cannot import. |
| `~/.onthego/backups/` | Encrypted supplemental-secret backup artifacts. |
| `refs/onthego/` | WIP ancestry, incoming objects, and bounded backups. |

A local-path device runs the same protocol without SSH. Shared agent homes are detected so a backup handoff cannot overwrite its own source history.

## Boundaries

Stop writers during capture and application. ONTHEGO does not merge simultaneous workspace changes. Tailscale is an optional connectivity choice. Continuous synchronization, additional agents, and asynchronous user-owned storage adapters are out of scope.
