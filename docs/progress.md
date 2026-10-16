# 0.2 implementation record

Updated 2026-10-16.

This implementation replaces the former hosted-service architecture with the explicit direct-device handoff described in [architecture.md](architecture.md). This document describes the current checkout; it does not claim that 0.2 has been published to npm or GitHub Releases.

## Implemented

- Project schema 2 and personal `~/.onthego/` configuration.
- Trusted SSH peers with pinned public encryption/signing identities.
- Encrypted signed workspace snapshots and local-device transport for offline testing.
- Git history, staged/unstaged changes, allowed untracked files, and selected ignored files.
- Native project-scoped Codex/Claude session history and destination path mapping.
- Conflict detection, explicit replacement, and preserved workspace backups.
- CLI diagnostics, inspection, portable snapshot commands, and public identity output.
- Removal of central-server, Daytona, LLM summary, deployment, and automatic `.env` requirements.
- Four-platform binary packaging and a standalone local handoff demo.

## Validation

Completed checks for the 0.2 implementation:

| Check | Result |
| --- | --- |
| `make check` | Formatting, Go vet, and full Go test suite passed. |
| `go test -race ./...` | Full Go test suite passed with the race detector. |
| `make build` and `./scripts/demo.sh` | Encrypted local-device push/pull passed; Git history, staged/unstaged changes, untracked files, selected ignored files, and destination-local `.env` preservation verified. |
| Isolated real OpenSSH handoff | Pair, push, remote edit, and pull passed through an unprivileged loopback SSH server with pinned host keys, temporary credentials, and spaces/apostrophes in paths. Binary and selected ignored files were included. |
| `make package` | macOS/Linux arm64/amd64 binaries cross-compiled; executable format and exact eight-file npm package allowlist validated. |
| Local npm tarball install | Offline installation into a temporary prefix succeeded; installed Linux amd64 `onthego version` and `--help` worked. |
| Shell, JavaScript, YAML, and diff checks | Demo shell syntax, launcher/verifier JavaScript syntax, workflow/project YAML parsing, and whitespace checks passed. |

Tests cover native session fixtures, project scoping, path remapping, encryption/signature verification, unsafe inputs, secret selection, conflicts, and workspace restoration. Cross-compilation does not establish runtime validation on macOS or Linux arm64. No package was published and no hosted infrastructure was contacted for these checks.

## Not delivered by this scope

- Continuous unattended two-way sync or automatic conflict merging.
- Running process, terminal, or in-memory agent migration.
- Global agent SQLite databases or authentication replication.
- Native Cursor/Hermes resume adapters.
- A published 0.2 release, package-manager registry upload, or changes to existing remote VPS deployments.

Real desktop/laptop SSH and actual installed Codex/Claude resume should be validated against the user's device and agent versions. Local fixture tests establish format and file-transfer behavior, not universal compatibility with all future agent releases.
