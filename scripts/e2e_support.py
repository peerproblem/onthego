"""Isolated test helpers. No user keys, agent homes or Git configuration are read."""
import json
import os
from pathlib import Path
import shlex
import subprocess

REPO = Path(__file__).resolve().parent.parent


def binary():
    path = Path(os.environ.get('ONTHEGO_BIN', REPO / 'dist/onthego')).resolve()
    if not path.is_file():
        raise RuntimeError('Build the CLI first with make build, or set ONTHEGO_BIN')
    return path


def environment(base):
    base = Path(base)
    values = dict(os.environ)
    for key, suffix in [('HOME', 'home'), ('ONTHEGO_HOME', 'settings'), ('CODEX_HOME', 'codex'), ('CLAUDE_CONFIG_DIR', 'claude')]:
        directory = base / suffix
        directory.mkdir(parents=True, exist_ok=True)
        values[key] = str(directory)
    values.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM='1')
    values.pop('ONTHEGO_DATA_HOME', None)
    values.pop('GIT_DIR', None)
    values.pop('GIT_WORK_TREE', None)
    values.pop('GIT_INDEX_FILE', None)
    return values


def run(args, cwd=None, env=None, **kwargs):
    result = subprocess.run([str(arg) for arg in args], cwd=cwd, env=env, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, **kwargs)
    if result.returncode:
        raise RuntimeError(f'{shlex.join(str(arg) for arg in args)} failed ({result.returncode})\n{result.stdout}{result.stderr}')
    return result.stdout.strip()


def init_project(path, cli, env, sessions=False):
    path.mkdir(parents=True)
    run(['git', 'init', '-q', '-b', 'main'], cwd=path, env=env)
    run(['git', 'config', 'user.name', 'ONTHEGO automated fixture'], cwd=path, env=env)
    run(['git', 'config', 'user.email', 'fixture@onthego.invalid'], cwd=path, env=env)
    (path / '.gitignore').write_text('node_modules/\n.env\n')
    (path / 'work.txt').write_text('base\n')
    run(['git', 'add', '.'], cwd=path, env=env)
    run(['git', 'commit', '-qm', 'Initial fixture'], cwd=path, env=env)
    run([cli, 'init'], cwd=path, env=env)
    policy = path / '.onthego.yaml'
    if not sessions:
        policy.write_text('\n'.join('agent: none' if line.startswith('agent:') else line for line in policy.read_text().splitlines()) + '\n')
    run(['git', 'add', '.'], cwd=path, env=env)
    run(['git', 'commit', '-qm', 'Configure fixture'], cwd=path, env=env)


def peer_wrapper(path, cli, env):
    keys = ['HOME', 'ONTHEGO_HOME', 'CODEX_HOME', 'CLAUDE_CONFIG_DIR', 'GIT_CONFIG_GLOBAL', 'GIT_CONFIG_NOSYSTEM']
    path.write_text('#!/bin/sh\n' + '\n'.join(f'export {key}={shlex.quote(env[key])}' for key in keys) +
                    '\nunset ONTHEGO_DATA_HOME GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE\n' +
                    f'exec {shlex.quote(str(cli))} "$@"\n')
    path.chmod(0o755)
    return path


def session_fixture(home, source, index, body_bytes=4096):
    directory = Path(home) / 'sessions' / '2026' / '10' / '16'
    directory.mkdir(parents=True, exist_ok=True)
    session_id = f'00000000-0000-4000-8000-{index:012d}'
    data = [{'type': 'session_meta', 'payload': {'id': session_id, 'cwd': str(source)}},
            {'type': 'event_msg', 'payload': {'type': 'user_message', 'message': 'x' * body_bytes}}]
    (directory / f'rollout-{session_id}.jsonl').write_text(''.join(json.dumps(row) + '\n' for row in data))
