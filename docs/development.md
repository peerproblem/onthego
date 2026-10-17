# Development

Requirements: Go 1.26 or newer (the build selects the toolchain in `go.mod`), Git, OpenSSH, Python 3, and Node.js 18 or newer for packaging.

```sh
make check            # gofmt, go vet, and all package tests
go test -race ./...
make build            # dist/onthego
make e2e              # real OpenSSH, interrupt, and npm launcher checks
make package          # four platform binaries plus npm pack --dry-run
```

Tests and scripts isolate ONTHEGO state, agent homes, and Git configuration in temporary directories. They never read your real sessions, keys, or credentials.

## Local demo

```sh
make build
./scripts/demo.sh
```

The demo creates disposable repositories and walks through init, pairing, a dry run, a push with staged, unstaged, untracked, and selected ignored files, a destination edit, and the return pull. It checks that `.env` stays on the source.

## End-to-end checks

```sh
./scripts/ssh-e2e.sh
python3 scripts/sigint-e2e.py
python3 scripts/npm-signal-e2e.py
```

`ssh-e2e.sh` starts a temporary OpenSSH server with isolated host and client keys, pairs a device, pushes unfinished work, edits the destination over SSH, and pulls it back. On Linux with root or passwordless sudo it creates a dedicated account with its own home and also checks `device add --install`, `sync`, and `device install` against that account. On macOS it runs an unprivileged sshd as the current user and skips the install step so your real `~/.local/bin` is never touched.

`sigint-e2e.py` interrupts a capture and checks exit status 130, a clean retry, and that no plaintext staging remains. `npm-signal-e2e.py` checks that the npm launcher forwards signals without leaving the native process behind.

## Agent compatibility

`scripts/agent-compatibility/verify.py` runs the real Codex CLI and Claude Code binaries against a local fixture provider, moves the generated transcripts through the production adapter, and resumes them by ID. See [agent compatibility](agent-compatibility.md) for the tested versions and how to reproduce.

## Benchmarks

```sh
python3 scripts/benchmark.py --profile smoke --output /tmp/onthego-smoke.json
python3 scripts/benchmark.py --profile full --output /tmp/onthego-full.json
```

The full profile builds 100,000 ignored dependency files on each device, 1 GiB of Git history, and 500 synthetic Codex transcripts. It measures an initial push, an incremental push, and a return pull, and checks that destination roots, Git directories, and ignored file inodes are untouched. It needs several GiB of free disk space.

One local run with 0.3.0 on macOS arm64 measured:

| Transfer | Elapsed | Net disk growth |
| --- | ---: | ---: |
| Initial push | 28.8 s | 1.08 GB |
| Incremental push | 2.7 s | 57 KB |
| Return pull | 3.4 s | 66 KB |

These are single local-process measurements, not a speed guarantee.

## Releasing

1. Update `VERSION` in the `Makefile`, `version` in `package.json`, and the development version in `.github/workflows/release.yml`.
2. Add `docs/releases/<version>.md`. The release workflow fails without it.
3. Merge to `main` with CI green, then push a `v<version>` tag.

The tag runs the full test matrix again, builds the four binaries with SHA-256 files, publishes `@jbaehova/onthego` to npm with provenance, and creates the GitHub release that `install.sh` downloads from.
