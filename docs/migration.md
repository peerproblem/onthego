# Migrating from 0.1

Version 0.2 changes ONTHEGO from a cloud workload service to a personal workspace handoff tool. The old `login`, cloud `pass`, hosted status, receiver/server, and workload APIs are no longer the product interface. `push`/`pull` now transfer a workspace to a device you own.

## Project settings

Run `onthego init` in an existing project. The schema-1 project ID and applicable include/exclude/agent policy are retained in schema 2; cloud targets, workload configuration, LLM summaries, and provider settings are removed from the saved marker. Device hosts and identities belong in `~/.onthego/config.yaml` and are managed with `onthego device`.

The CLI does not load project `.env` files. Existing application `.env` files are left in place. Untracked or ignored `.env` files are excluded by default and need explicit `--include-secret` if you want to hand them to another trusted device. Already tracked files and full Git history transfer unchanged, including any committed secrets.

The following former ONTHEGO credentials and settings are unnecessary for 0.2:

- Daytona API/JWT credentials.
- ONTHEGO control-plane URL and TLS certificate pin.
- ONTHEGO login/bootstrap/session credentials.
- OpenAI key used only for ONTHEGO status summaries.
- DNS/provider credentials used only for hosted service operations.

Remove unused credentials from your own configuration and retire old servers separately when appropriate. Updating this checkout does not shut down or delete any existing VPS, revoke provider credentials, or modify their billing. Agent-provider credentials that Codex/Claude themselves need are separate from ONTHEGO.

## Pair the machines

Install this version on each machine and confirm trusted SSH access. Register the destination from the source project:

```sh
onthego init
onthego device add laptop --host me@my-macbook --path /Users/me/projects/my-app
onthego doctor laptop
onthego push laptop --dry-run
onthego push laptop
```

Choose an absent destination for the first transfer. ONTHEGO refuses an unrelated initialized project even with `--replace`. If you intentionally replace another existing destination, preserve needed work and read the conflict output before using `--replace`; the prior directory is retained as a sibling backup.

## Old artifacts

Legacy summary/workload artifacts are not native agent sessions. Keep any legacy data you need, but create a fresh 0.2 snapshot from the source project for a native-session handoff. Old Cursor/Hermes summary collection does not imply native resume support; current native session adapters cover Codex and Claude.

Do not copy your entire `.codex` or `.claude` directory between devices. ONTHEGO imports the project-scoped session records and leaves each device's authentication and machine-specific state separate.
