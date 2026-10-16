#!/usr/bin/env python3
"""Interrupt a real CLI while Git is busy, then retry without deleting locks.

A temporary git wrapper pauses only `git bundle create` before executing the
real Git command. This makes signal timing reproducible without a large fixture
or application test hooks. The CLI, locking and cancellation paths are real.
"""
import os
import hashlib
import json
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time

import sys
sys.dont_write_bytecode = True

from e2e_support import binary, environment, init_project, run


def assert_no_temporary_data(base, env):
    staging = list(base.rglob('.onthego-stage-*')) + list(base.rglob('native-sessions'))
    temp_home = Path(env['ONTHEGO_HOME']) / 'tmp'
    temporary_payloads = [path for path in temp_home.iterdir() if path.name not in {'.cleanup.lock', '.leases'}] if temp_home.exists() else []
    leases = temp_home / '.leases'
    if leases.exists():
        temporary_payloads.extend(leases.iterdir())
    if staging or temporary_payloads:
        raise AssertionError(f'Cancellation or retry left temporary data: {staging + temporary_payloads}')


def check_peer_input_cancellation(cli, base, source, env):
    public = json.loads(run([cli, 'peer', 'identity'], env=env))
    sender = 'device-' + hashlib.sha256(public['signing_public'].encode()).hexdigest()[:32]
    run([cli, 'peer', 'trust', '--name', sender, '--age-recipient', public['age_recipient'],
         '--signing-public', public['signing_public']], env=env)
    for data in (b'', b'incomplete encrypted package\n'):
        for sig in (signal.SIGINT, signal.SIGTERM):
            process = subprocess.Popen([str(cli), 'peer', 'apply', '--root', str(source), '--from', sender],
                                       cwd=source, env=env, stdin=subprocess.PIPE,
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                if data:
                    process.stdin.write(data)
                    process.stdin.flush()
                deadline = time.monotonic() + 5
                while True:
                    packages = list((Path(env['ONTHEGO_HOME']) / 'tmp').glob('transfer-*/snapshot.otg'))
                    if packages and packages[0].stat().st_size == len(data):
                        break
                    if process.poll() is not None:
                        raise AssertionError(f'peer apply exited before reading stdin: {process.stderr.read()!r}')
                    if time.monotonic() > deadline:
                        raise AssertionError('peer apply never reached its stdin copy')
                    time.sleep(0.01)
                started = time.monotonic()
                process.send_signal(sig)
                code = process.wait(timeout=1)
                elapsed = time.monotonic() - started
                if code != 130 or elapsed >= 1:
                    raise AssertionError(f'peer apply cancellation exited {code} after {elapsed:.3f}s')
                error = json.loads(process.stderr.read())
                if error.get('code') != 'E_CANCELED':
                    raise AssertionError(f'Unexpected cancellation error: {error}')
                assert_no_temporary_data(base, env)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
                process.stdin.close()
                process.stdout.close()
                process.stderr.close()
    print('PASS: peer apply with empty and partial stdin exits 130 within one second on SIGINT/SIGTERM; no temporary data remains.')


def main():
    cli = binary()
    git = shutil.which('git')
    with tempfile.TemporaryDirectory(prefix='onthego-sigint-e2e-') as temporary:
        base = Path(temporary).resolve()
        env = environment(base / 'isolated')
        source = base / 'project'
        init_project(source, cli, env)
        fixture_bin = base / 'bin'
        fixture_bin.mkdir()
        marker = base / 'bundle-started'
        wrapper = fixture_bin / 'git'
        wrapper.write_text('#!/usr/bin/env python3\nimport os,sys,time\n'
            'if os.environ.get("ONTHEGO_E2E_PAUSE") and "bundle" in sys.argv and "create" in sys.argv:\n'
            '    open(os.environ["ONTHEGO_E2E_MARKER"], "w").write("started")\n'
            '    time.sleep(60)\n'
            f'os.execv({str(git)!r}, [{str(git)!r}] + sys.argv[1:])\n')
        wrapper.chmod(0o755)
        slow_env = dict(env, PATH=str(fixture_bin) + os.pathsep + env['PATH'],
                        ONTHEGO_E2E_PAUSE='1', ONTHEGO_E2E_MARKER=str(marker))
        output = base / 'interrupted.otg'
        log_path = base / 'interrupt.log'
        with log_path.open('w+') as log:
            process = subprocess.Popen([str(cli), 'snapshot', 'pack', '--output', str(output)], cwd=source,
                                       env=slow_env, stdout=log, stderr=log)
            try:
                deadline = time.monotonic() + 30
                while not marker.exists():
                    if process.poll() is not None:
                        log.seek(0)
                        raise RuntimeError('CLI exited before the cancellation point: ' + log.read())
                    if time.monotonic() > deadline:
                        raise RuntimeError('CLI never reached git bundle create')
                    time.sleep(0.02)
                process.send_signal(signal.SIGINT)
                code = process.wait(timeout=15)
                if code == 0:
                    raise AssertionError('Interrupted snapshot unexpectedly succeeded')
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
        if output.exists():
            raise AssertionError('Interrupted snapshot left a published output file')
        # Do not delete or modify either lock. A successful retry proves that
        # both the project and user-state lock were released on cancellation.
        retry = base / 'retry.otg'
        run([cli, 'snapshot', 'pack', '--output', retry], cwd=source, env=env, timeout=30)
        if not retry.is_file() or not retry.stat().st_size:
            raise AssertionError('Retry produced no encrypted snapshot')
        assert_no_temporary_data(base, env)
        print(f'PASS: SIGINT during real snapshot capture exited {code}; retry succeeded without lock deletion; no plaintext staging remains.')
        check_peer_input_cancellation(cli, base, source, env)


if __name__ == '__main__':
    main()
