# 0.3 implementation and validation

The source requirements are `.hov/CLAUDE_FEEDBACK.md` dated 2026-10-16. The owner selected the 0.3 Git redesign. Tailscale remains optional, the target is CLI users, and continuous synchronization or user-owned asynchronous storage is future work.

Implementation uses `v2-fan` with separate ownership for Git storage, session adapters, filesystem lifecycle, peer CLI, compatibility evidence, and validation scripts. Integration and publication remain root-owned.

## Feedback coverage

| Review item | Implemented behavior | Evidence |
| --- | --- | --- |
| 1 | Canonical root aliases on macOS; project-internal symlink checks remain. | fsx path tests and macOS CI. |
| 2 | Kernel flock, SIGINT/SIGTERM context cancellation, private leased temporary storage and cleanup. | fsx process tests, main process tests, `scripts/sigint-e2e.py`. |
| 3 | In-place Git application preserves local config, hooks, tracking, remotes, reflogs, and stashes. | restore and syncer preservation regressions. |
| 4 | Unmanaged files stay in place; bounded Git object backups with list/prune/undo. | inode regressions and `scripts/benchmark.py`. |
| 5 | Truncated tail warns; malformed complete records exclude only that session. | session and integrated handoff regressions. |
| 6 | Divergent sessions are preserved separately; newer local histories are retained. | keep-both/theirs/skip adapter regressions. |
| 7 | Narrow secret patterns and explicit excluded-file output. | capture false-positive regression and CLI preview tests. |
| 8 | Shared agent-home imports preserve the source. | same-home adapter and local handoff tests. |
| 9 | Claude directories derive from mapped CWD, including subdirectories and long paths. | adapter tests and verified CLI fixture. |
| 10 | Session staging stays in private temporary storage and is removed after import. | syncer plaintext-cleanup assertions and lifecycle E2E. |
| 11 | Token-level path substitution preserves other JSONL bytes. | byte-exact adapter tests and genuine native fixtures. |
| 12 | Pending exports support offline return ancestry, bounded retention, and TTL. | offline roundtrip and pending-state tests. |
| 13 | Exact apply replay is idempotent; authenticated recipient state recovers lost receipts. | replay and reconciliation regressions. |
| 14 | Negotiated incremental bundles; Git history has a separate size budget. | incremental capture tests and full benchmark. |
| 15 | Mutual pairing and locally pinned sender lookup. | unpaired apply and key-substitution CLI tests. |
| 16 | Go module matches `github.com/peerproblem/onthego`. | module build/install validation. |
| 17 | WIP Git ancestry replaces transitive snapshot ID lists. | bounded-manifest multi-device rotation tests. |
| 18 | JSON results and errors share stable structured fields. | main JSON-error tests and peer protocol tests. |
| 19 | Capture avoids redundant outer fingerprints; session metadata/hash cache avoids repeated full reads. | cache invalidation tests and benchmark measurements. |

## Structural changes

- Git WIP trees preserve staged and unstaged content; existing repositories are updated in place.
- Per-peer receipts and active-device guidance support multiple devices.
- Device identity and project path mappings are separate.
- `peer hello` negotiates protocol version and required capabilities.
- The CLI has command-specific modules, phase progress, and session selection controls.
- Snapshot schema 2 removes obsolete cloud metadata and patch fields.
- Shared filesystem helpers replace duplicate lock and path implementations.
- CI exercises macOS and Linux. Release packaging produces four native binaries and the npm launcher.

## Verification record

Before changes, local macOS `make check` reproduced all 15 reported syncer failures due to `/var` symlink rejection.

Local validation on macOS arm64 passed against implementation commit `74e5299` on 2026-10-16:

- `make check`, including formatting, vet, and all package tests.
- `go test -race ./...`.
- The demo roundtrip and real OpenSSH pairing, push, remote edit, and pull.
- SIGINT during capture: exit 130, successful retry, and no retained plaintext staging.
- Four native platform builds and npm package allowlist/version verification.

Native CLI compatibility evidence is maintained separately in [agent compatibility](agent-compatibility.md). The [CI workflow](https://github.com/peerproblem/onthego/actions/workflows/ci.yml) runs the expanded checks on macOS and Ubuntu. The [release workflow](https://github.com/peerproblem/onthego/actions/workflows/release.yml) repeats these gates for the release tag before publication. A configured matrix alone is not evidence of a successful run.

## Performance baseline

Measured with `onthego 0.3.0 (74e5299)` on macOS arm64. The full fixture contains 100,000 ignored files on each device, 1 GiB of random historical Git content removed from the active working tree, and 500 synthetic Codex transcripts of 4 KiB each. These are local-process transfers and one-run measurements; SSH transport and native resume are separate checks.

| Transfer | Elapsed | Net allocated disk growth |
| --- | ---: | ---: |
| Initial push | 28.7752 s | 1,080,311,808 bytes |
| Incremental push | 2.6628 s | 57,344 bytes |
| Return pull | 3.4435 s | 65,536 bytes |

All 200,000 ignored-file inodes and all four project/Git-directory inodes were preserved. The destination retained its local Git configuration and hook. All 500 sessions arrived. Source Git allocation increased by 86,016 bytes across the full run. The complete fixture used 2,994,552,832 allocated bytes before automatic removal. These results show bounded incremental growth for this fixture, not a universal speed guarantee.

Reproduce with:

```sh
make build
python3 scripts/benchmark.py --profile full --output /tmp/onthego-benchmark.json
```

## Review and release

Implementation was separated into [runtime foundations](https://github.com/peerproblem/onthego/pull/1), [native session adapters](https://github.com/peerproblem/onthego/pull/2), and [Git handoff integration](https://github.com/peerproblem/onthego/pull/3). The release gate and documentation are reviewed separately. Publication evidence belongs to the tagged workflow and the [0.3.0 release](https://github.com/peerproblem/onthego/releases/tag/v0.3.0).
