# Handoff demo

Run the deterministic local demo from this checkout:

```sh
make build
./scripts/demo.sh
```

The script uses temporary directories and its own `ONTHEGO_HOME`. It creates a Git project, makes staged and unstaged edits, adds an untracked file and an explicitly included ignored file, transfers an encrypted snapshot to a local peer, and checks the restored content. It never connects to a hosted service or reads your own agent session directories. All temporary data is removed on exit.

For the real desktop/laptop flow, install the same ONTHEGO version and Git on both machines, authenticate Codex/Claude on each, and establish trusted SSH access:

```sh
cd /path/to/project
onthego init
onthego device add laptop --host my-laptop --path /absolute/path/to/project
onthego doctor laptop
onthego push laptop --dry-run

# Stop the project's coding agent and other active writers before handing off.
onthego push laptop
```

On the laptop, inspect `onthego status` and resume the relevant reported session ID with `codex resume <id>` or `claude --resume <id>`. After stopping the laptop's agent, run `onthego pull laptop` from the original project on the desktop to return the work.

For local testing only, `device add copy --local-path /absolute/absent/path` selects a directory on this machine instead of SSH. This verifies packaging, encryption, conflict checks, and restoration without proving network or native agent version compatibility.
