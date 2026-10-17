# Migrating from 0.1

Since 0.3, ONTHEGO is a direct handoff CLI. The retired 0.1 cloud workflow is no longer supported. There is no ONTHEGO control plane, server deployment, provider key, or Daytona dependency to configure.

Install the current release on every device (`onthego device add <name> --install` copies it over SSH). Run `onthego init` inside each source Git project to write current project policy. Pair the destination with `device add`, then choose a per-project path with `device path` if the default home-relative location does not fit. Personal device configuration belongs under `~/.onthego`, not in the Git repository.

Existing `.onthego.yaml` project IDs can be retained by initialization. Schema 2 `.otg` packages use Git WIP objects and are not compatible with development packages from the former patch-based snapshot implementation. Create a new package with the current CLI. Old baseline state is not treated as evidence that a current handoff was delivered.

For the first handoff, use an absent destination directory. To adopt an existing matching project, inspect its work and use `--replace` if appropriate. This retains a bounded Git backup of managed state. `onthego backups list` and `onthego undo` expose recovery; local dependency directories and destination Git settings remain in place.

Old `.onthego-backup-*` directories from the directory-replacement implementation are not deleted automatically. Review them before removing them. They may contain local credentials or session transcripts. New handoffs no longer create those whole-directory backups.

Codex and Claude must be installed and authenticated independently on the destination. ONTHEGO transfers project histories, not agent authentication or global databases. Run `onthego doctor` to see installed versions and compatibility warnings. See [agent compatibility](agent-compatibility.md) for reproducible resume checks.

Tailscale remains optional. Any trusted reachable SSH connection is sufficient; see the [Tailscale guide](tailscale.md) when devices are on different networks. For offline delivery, use an encrypted package and authenticate the sender's public signing key separately.
