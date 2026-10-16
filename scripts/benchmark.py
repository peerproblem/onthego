#!/usr/bin/env python3
"""Measure real local handoffs with large ignored trees, history and sessions.

Default full profile: 100,000 ignored files per device, 1 GiB of incompressible
Git history, 500 synthetic Codex sessions. Use --profile smoke for CI. Each size
can be overridden. JSON includes observed timings, allocated disk bytes, session
counts and preservation of every ignored file's inode. This is a performance
baseline, not a claimed speed threshold or real-agent resume validation.
"""
import argparse
import json
import os
from pathlib import Path
import platform
import tempfile
import time

import sys
sys.dont_write_bytecode = True

from e2e_support import binary, environment, init_project, peer_wrapper, run, session_fixture


def disk(path):
    allocated = logical = files = 0
    for directory, _, names in os.walk(path):
        for name in names:
            info = (Path(directory) / name).lstat()
            allocated += info.st_blocks * 512
            logical += info.st_size
            files += 1
    return {'allocated_bytes': allocated, 'logical_bytes': logical, 'files': files}


def ignored_files(root, count):
    directory = root / 'node_modules'
    directory.mkdir()
    inodes = {}
    for index in range(count):
        subdirectory = directory / f'pkg-{index // 1000:05d}'
        subdirectory.mkdir(exist_ok=True)
        path = subdirectory / f'{index:06d}.js'
        path.write_text(f'export default {index};\n')
        inodes[str(path.relative_to(root))] = path.stat().st_ino
    return inodes


def preserved(root, inodes):
    result = sum((root / path).exists() and (root / path).stat().st_ino == inode for path, inode in inodes.items())
    if result != len(inodes):
        raise AssertionError(f'Ignored file inodes changed: preserved {result}/{len(inodes)} at {root}')
    return result


def history(root, size, env):
    remaining = size
    generation = 0
    blob = root / 'history.bin'
    while remaining:
        length = min(64 * 1024 * 1024, remaining)
        with blob.open('wb') as file:
            unwritten = length
            while unwritten:
                chunk = min(8 * 1024 * 1024, unwritten)
                file.write(os.urandom(chunk))
                unwritten -= chunk
        run(['git', 'add', 'history.bin'], cwd=root, env=env)
        run(['git', '-c', 'gc.auto=0', 'commit', '-qm', f'History fixture {generation}'], cwd=root, env=env)
        remaining -= length
        generation += 1
    if size:
        run(['git', 'rm', '-q', 'history.bin'], cwd=root, env=env)
        run(['git', 'commit', '-qm', 'Remove large working file; retain history'], cwd=root, env=env)
    # History blobs are random and intentionally share no useful deltas. Avoid
    # spending the fixture setup comparing a GiB of unrelated random data.
    run(['git', '-c', 'pack.threads=1', '-c', 'pack.window=0', 'gc', '--prune=now'], cwd=root, env=env)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--profile', choices=['full', 'smoke'], default='full')
    parser.add_argument('--files', type=int, help='ignored files per device')
    parser.add_argument('--history-mib', type=int)
    parser.add_argument('--sessions', type=int)
    parser.add_argument('--session-bytes', type=int, default=4096)
    parser.add_argument('--output', type=Path, help='write measured JSON result outside the temporary fixture')
    args = parser.parse_args()
    defaults = {'full': (100000, 1024, 500), 'smoke': (200, 4, 8)}[args.profile]
    count, history_mib, sessions = [value if value is not None else fallback for value, fallback in zip((args.files, args.history_mib, args.sessions), defaults)]
    if min(count, history_mib, sessions, args.session_bytes) < 0:
        parser.error('fixture sizes must be nonnegative')
    cli = binary()
    print(f'Preparing {count} ignored files per device, {history_mib} MiB of Git history and {sessions} synthetic sessions.', flush=True)
    with tempfile.TemporaryDirectory(prefix='onthego-benchmark-') as temporary:
        base = Path(temporary).resolve()
        source, target = base / 'source', base / 'target'
        env, remote_env = environment(base / 'local'), environment(base / 'remote')
        wrapper = peer_wrapper(base / 'remote' / 'peer', cli, remote_env)
        setup_started = time.monotonic()
        init_project(source, cli, env, sessions=True)
        history(source, history_mib * 1024 * 1024, env)
        for index in range(sessions):
            session_fixture(env['CODEX_HOME'], source, index, args.session_bytes)
        source_inodes = ignored_files(source, count)
        run([cli, 'device', 'add', 'benchmark', '--local-path', target, '--binary', wrapper], cwd=source, env=env)
        result = {'profile': args.profile, 'platform': platform.platform(),
                  'version': run([cli, 'version'], env=env),
                  'fixture': {'ignored_files_per_device': count, 'git_history_input_bytes': history_mib * 1024 * 1024,
                              'synthetic_sessions': sessions, 'session_body_bytes': args.session_bytes},
                  'setup_seconds': round(time.monotonic() - setup_started, 4),
                  'source_git_before': disk(source / '.git'), 'transfers': {}}

        def transfer(name, command):
            before = disk(base)
            started = time.monotonic()
            run([cli, command, 'benchmark', '--all-sessions'], cwd=source, env=env)
            elapsed = time.monotonic() - started
            after = disk(base)
            result['transfers'][name] = {'seconds': round(elapsed, 4), 'before': before, 'after': after,
                'allocated_delta_bytes': after['allocated_bytes'] - before['allocated_bytes']}
            print(f'{name}: {elapsed:.3f}s, allocated delta {after["allocated_bytes"] - before["allocated_bytes"]} bytes', flush=True)

        transfer('initial_push', 'push')
        target_inodes = ignored_files(target, count)
        roots = {str(path): path.stat().st_ino for path in [source, source / '.git', target, target / '.git']}
        run(['git', 'config', 'core.hooksPath', '.husky'], cwd=target, env=remote_env)
        run(['git', 'config', 'onthego.benchmark', 'destination-local'], cwd=target, env=remote_env)
        hooks = target / '.git' / 'hooks'
        hooks.mkdir(exist_ok=True)
        (hooks / 'benchmark-marker').write_text('keep local hook\n')
        (source / 'work.txt').write_text('base\nstaged benchmark\n')
        run(['git', 'add', 'work.txt'], cwd=source, env=env)
        with (source / 'work.txt').open('a') as file:
            file.write('unstaged benchmark\n')
        transfer('incremental_push', 'push')
        if run(['git', 'config', '--get', 'core.hooksPath'], cwd=target, env=remote_env) != '.husky':
            raise AssertionError('Destination Git configuration was lost')
        if not (hooks / 'benchmark-marker').is_file():
            raise AssertionError('Destination local hook was lost')
        if (source / 'work.txt').read_bytes() != (target / 'work.txt').read_bytes():
            raise AssertionError('Incremental push did not apply worktree changes')
        if run(['git', 'show', ':work.txt'], cwd=source, env=env) != run(['git', 'show', ':work.txt'], cwd=target, env=remote_env):
            raise AssertionError('Incremental push did not preserve staged changes')
        (target / 'returned.txt').write_text('return handoff\n')
        transfer('return_pull', 'pull')
        if (source / 'returned.txt').read_text() != 'return handoff\n':
            raise AssertionError('Pull did not return the remote file')
        if not all(Path(path).stat().st_ino == inode for path, inode in roots.items()):
            raise AssertionError('A project root or .git directory inode changed')
        imported_sessions = len(list(Path(remote_env['CODEX_HOME']).rglob('rollout-*.jsonl')))
        if imported_sessions != sessions:
            raise AssertionError(f'Expected {sessions} imported sessions; observed {imported_sessions}')
        result['preservation'] = {'source_ignored_inodes': preserved(source, source_inodes),
                                  'target_ignored_inodes': preserved(target, target_inodes),
                                  'project_and_git_directory_inodes': len(roots),
                                  'destination_git_config_and_hook': True,
                                  'imported_sessions': imported_sessions}
        result['final_disk'] = {'source': disk(source), 'target': disk(target), 'all_fixture_data': disk(base)}
        result['fixture_removed_after_run'] = True
    rendered = json.dumps(result, indent=2) + '\n'
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(rendered)
    print(rendered, end='')


if __name__ == '__main__':
    main()
