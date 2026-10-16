#!/bin/sh
# Fresh loopback SSH keys and isolated application homes. Linux CI uses a temporary OS account.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
exec python3 "$repo_root/scripts/ssh-e2e.py" "$@"
