# Direct workspace handoff architecture

## Product contract

ONTHEGO is an open-source tool installed on the user's machines. A machine can be a desktop, laptop, or privately managed VPS. There is no ONTHEGO-operated service, login, cloud runtime, database, or LLM API dependency.

The initial 0.2 scope is a reliable, explicit handoff: stop writing, capture a project, transfer it, restore it, and resume the native coding agent. This is the foundation for later continuous synchronization, not a claim that concurrent filesystem writes or running agent processes can already move safely.

## Data flow

1. Resolve the current Git worktree and its stable project ID in `.onthego.yaml`.
2. Capture Git refs/history, index and working-tree patches, allowed untracked files, selected ignored files, and supported project-scoped native sessions.
3. Build a manifest describing the files, hashes, project identity, source paths, and agent session information.
4. Sign the snapshot with the sender's Ed25519 identity and encrypt it to the recipient's age identity.
5. Stream the snapshot over the user's existing SSH connection to the peer ONTHEGO binary, or save a portable `.otg` file.
6. Verify the pinned sender, decrypt, check manifest paths and hashes, and restore into a staged destination.
7. Enforce project identity and workspace conflict checks before applying; preserve an existing destination as a sibling backup.
8. Import supported native sessions with paths mapped to the new workspace and report session IDs for explicit resume.

All required work happens on the two participating machines. Both must be reachable during a direct SSH transfer. A portable encrypted snapshot supports deferred or offline delivery.

## Storage and trust

| Location | Role |
| --- | --- |
| Project `.onthego.yaml` | Trackable project ID, inclusion/exclusion policy, agent selection. |
| Project `.onthego.local/` | Restored handoff context; excluded from the next snapshot and Git. |
| `~/.onthego/projects/<project-id>/` | Per-workspace synchronization baselines, transfer state, and temporary capture data. |
| `~/.onthego/config.yaml` | Personal named devices, transport settings, and pinned public identities. |
| `~/.onthego/identity/keys.json` | Local private age and Ed25519 identities. |
| Existing SSH config and keys | User-managed host discovery and authentication. |
| Sibling `.onthego-backup-*` directory | Previous destination preserved on replacement. |

Personal directories are mode `0700`; private files are `0600`. `ONTHEGO_HOME` is an optional override. ONTHEGO never automatically loads `.env` files. Config contains an SSH identity-file path, not a copy of its key.

`device add` queries a peer's public identity through an already trusted SSH connection. SSH host checking remains enabled. Peer identities are pinned in the user's personal configuration and used for later encrypted transfers; identity changes require deliberate re-pairing. There is no device registration endpoint or central identity authority.

`--local-path` exercises the same snapshot creation and restore boundaries on one machine without SSH. It is useful for local backups and test fixtures; it is not a network transport.

## Git and GitHub

Git owns code history. ONTHEGO carries a Git bundle so no remote service, access token, or previously pushed commit is required for a handoff. The snapshot also contains index and working-tree patches, because a Git push alone cannot move an unfinished workspace. Hooks, local Git configuration, and reflogs are not copied. A stash tip reachable from a bundled ref can travel, but older stash entries accessible only through the stash reflog do not.

GitHub is optional and stays the normal channel for pull requests and collaboration. Agent sessions and ignored files are never uploaded to GitHub by ONTHEGO. A safe source origin can be restored on the destination; embedded credentials and machine-local source paths must not become destination remotes.

The current snapshot format allows at most 250 MiB of expanded payload and 20,000 archive entries. Capture rejects workspaces that exceed the limit; large Git histories or agent logs may require a smaller project scope or a later incremental protocol.

The first implementation prioritizes correctness and portability over incremental transfer size. Large Git histories are captured in full bundles. A future protocol may negotiate missing Git objects against a remote, while preserving the ability to work without GitHub.

## Session adapters

Codex and Claude adapters discover sessions tied to the project directory, preserve their native JSONL records, and map source workspace paths to the destination directory. They do not import authentication stores, provider keys, unrelated projects, global state, or SQLite databases. The destination installs and authenticates its own agent. The supported import target is native CLI history; desktop-app sidebar indexes are not rebuilt or synchronized.

Native resume needs native history. Unlike the former summary workflow, these records are not rewritten into an LLM-generated handoff or claimed to be redacted. An encrypted transcript can still contain sensitive text when decrypted by its intended recipient. This is a trusted-personal-device feature.

A live process, in-memory model state, terminal attachment, pending tool call, or server-side session is not transferred. Stop the source agent before capture. Resume a reported session ID with `codex resume` or `claude --resume`; native format compatibility must be checked against the actual installed agent versions.

## Inclusion and secret policy

Tracked content is part of Git history, so secret hygiene in the repository remains the user's responsibility. Ignored files require an explicit include, and recognizable secret files such as `.env` additionally require explicit `--include-secret`. Private keys and agent authentication material stay out of the supplemental file/session capture. Once explicitly included, supplemental paths remain managed across subsequent handoffs so updates and deletions round-trip. Machine-specific secrets should stay outside this managed set. Configuration never contains secret values.

Generated dependencies, caches, and ONTHEGO's own local state are excluded by default. Inclusion rules are project policy, not a promise to transfer every byte under the directory. Dry-run/status output lets a user inspect the scope before sending.

## Conflicts, recovery, and consistency

The destination is staged and verified before replacing the working directory. An absent destination can be initialized from the snapshot. Existing destinations must belong to the same project and satisfy the synchronization baseline, or the caller must request `--replace`. A mismatched project ID is still rejected. Existing content is retained in a sibling backup even when replacement is allowed. Destination-local ignored or ONTHEGO-excluded untracked files, such as `.env` and caches, are also preserved in the active workspace when they were not managed by the previous or incoming transfer.

There is no automatic three-way file merge in this delivery. Two devices editing the same project independently can diverge; users should preserve and reconcile that work with Git. Native session histories must not be treated as a CRDT or blindly concatenated.

Agents and other processes must be quiescent during capture and application. Backups provide recovery after workspace replacement; they are retained until the user removes them. They can contain sensitive files and should receive the same protection as the project itself.

## Implementation plan and delivery status

The 0.2 implementation is organized into these work packages:

1. **Remove the hosted-service architecture.** Delete the central server, Daytona transport/workload flow, LLM status summary, automatic `.env` loader, provider SDK dependencies, and deployment scripts.
2. **Move personal settings out of repositories.** Introduce versioned `~/.onthego/` configuration, persistent private identities, named peers, and migration of the old project marker while retaining its ID.
3. **Preserve a complete unfinished Git workspace.** Package history, index/worktree changes, supplemental files, and an integrity manifest; restore into a fresh staged repository.
4. **Carry native session history.** Add project-scoped Codex/Claude export and import, destination path mapping, and explicit resume guidance.
5. **Add direct peer transport and conflict protection.** Use trusted SSH, pinned recipient/signer identities, encrypted snapshots, baseline checks, and preserved destination backups.
6. **Ship a self-contained CLI.** Replace service commands with device, push/pull, snapshot, inspection, identity, and diagnostic commands; package macOS/Linux binaries for arm64/amd64.
7. **Verify the actual workflow.** Test encryption/signature failures, unsafe paths, scope isolation, Git state fidelity, secret selection, conflicts, and local end-to-end handoffs. Validate the package and document any real-device limitations.

Implementation and verification are tracked in [progress.md](progress.md). The initial plan is intentionally bounded: complete explicit handoffs first, then build unattended synchronization on verified conflict and session semantics.

## Later work

- Continuous watching with debouncing, snapshots, disconnect recovery, and clear ownership of the active workspace.
- Negotiated incremental Git objects and supplemental-file deltas.
- User-facing conflict resolution instead of forcing replacement or manual Git reconciliation.
- Additional native agent formats and compatibility fixtures for real released versions.
- Platform-specific agent indexes or databases only after safe supported import methods are established.
- Optional adapters for user-owned storage or encrypted relays when devices cannot be online together.
- Windows support and additional package managers.

A hosted ONTHEGO service, mandatory provider key, or dependency on a particular VPS vendor is not part of the target architecture.
