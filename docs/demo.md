# Reproduce a handoff

Build the CLI and run the isolated local demo:

```sh
make build
./scripts/demo.sh
```

The demo creates disposable repositories and private ONTHEGO and agent homes. It checks unfinished Git state, secret selection, native histories, a destination edit, and the return handoff. It does not read your real agent sessions or credentials.

For a real SSH subprocess roundtrip:

```sh
./scripts/ssh-e2e.sh
```

The script starts a temporary OpenSSH server with isolated keys and host verification. It pairs a device, sends staged and unstaged work, edits the destination over SSH, and pulls the changes back. Linux may require permission to create the isolated SSH account. A supported rootless sshd configuration is used on macOS.

Verify interrupt recovery and performance separately:

```sh
python3 scripts/sigint-e2e.py
python3 scripts/benchmark.py --profile smoke --output /tmp/onthego-smoke.json
python3 scripts/benchmark.py --profile full --output /tmp/onthego-full.json
```

The full profile creates 100,000 ignored dependency files per device, a 1 GiB Git history, and 500 sessions. It measures initial and incremental transfers and a return handoff. It checks that destination roots, Git directories, and ignored file inodes stay unchanged. It takes substantially more disk space and time than the smoke profile.

Real CLI resume checks are described in [agent compatibility](agent-compatibility.md). Their local provider fixture isolates transcript-format compatibility from model-service availability.
