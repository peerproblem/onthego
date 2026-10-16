#!/bin/sh
# A self-contained encrypted handoff. It never uses a hosted API or personal sessions.
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
if [ -n "${ONTHEGO_BIN:-}" ]; then
  case "$ONTHEGO_BIN" in
    /*) ;;
    *) ONTHEGO_BIN="$(command -v "$ONTHEGO_BIN")" ;;
  esac
else
  ONTHEGO_BIN="$repo_root/dist/onthego"
fi
if [ ! -x "$ONTHEGO_BIN" ]; then
  make -C "$repo_root" build
fi

scratch_root=$(mktemp -d "${TMPDIR:-/tmp}/onthego-demo.XXXXXX")
scratch_root=$(CDPATH='' cd -- "$scratch_root" && pwd -P)
trap 'rm -rf "$scratch_root"' EXIT HUP INT TERM
export ONTHEGO_HOME="$scratch_root/settings"
source_dir="$scratch_root/desktop"
target_dir="$scratch_root/laptop"
mkdir -p "$source_dir"

run() {
  printf '\n$ %s\n' "$*"
  "$@"
}

cd "$source_dir"
git init -q
git config user.name 'ONTHEGO Demo'
git config user.email 'demo@onthego.invalid'
printf 'base\n' > work.txt
printf 'scratch/\n' > .gitignore
git add work.txt .gitignore
git commit -qm 'Initial project'
run "$ONTHEGO_BIN" init
# This demo tests workspace transfer. Disable session discovery rather than
# touching the user's installed agent directories or environment.
awk '
  /^include:/ { print; print "    - scratch/notes.txt"; next }
  /^agent:/ { print "agent: none"; next }
  { print }
' .onthego.yaml > .onthego.yaml.tmp
mv .onthego.yaml.tmp .onthego.yaml
git add .onthego.yaml .gitignore
git commit -qm 'Configure portable project policy'

printf 'base\nstaged\n' > work.txt
git add work.txt
printf 'unstaged\n' >> work.txt
printf 'untracked work\n' > draft.txt
mkdir -p scratch
printf 'ignored notes\n' > scratch/notes.txt
printf 'DEMO_SECRET=local-only-demo-value\n' > .env

run "$ONTHEGO_BIN" device add laptop --local-path "$target_dir" --binary "$ONTHEGO_BIN"
run "$ONTHEGO_BIN" push laptop --dry-run
run "$ONTHEGO_BIN" push laptop
cmp work.txt "$target_dir/work.txt"
cmp draft.txt "$target_dir/draft.txt"
cmp scratch/notes.txt "$target_dir/scratch/notes.txt"
test ! -e "$target_dir/.env"
test "$(git rev-parse HEAD)" = "$(git -C "$target_dir" rev-parse HEAD)"
test "$(git show :work.txt)" = "$(git -C "$target_dir" show :work.txt)"
test -n "$(git -C "$target_dir" diff --cached --name-only)"
test -n "$(git -C "$target_dir" diff --name-only)"

printf 'laptop work\n' >> "$target_dir/draft.txt"
# A successful push records the desktop baseline, so the return handoff can
# apply without forcing replacement while preserving the local ignored .env.
run "$ONTHEGO_BIN" pull laptop
cd "$source_dir"
cmp draft.txt "$target_dir/draft.txt"
test -f .env
run "$ONTHEGO_BIN" status
printf '\nPASS: Git history, index, working tree, untracked and selected ignored files round-tripped.\n'
printf 'The .env stayed on the desktop and all demo data will now be removed.\n'
