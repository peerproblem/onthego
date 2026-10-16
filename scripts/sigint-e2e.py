#!/usr/bin/env python3
"""Interrupt a real CLI while Git is busy, then retry without deleting locks.

A temporary git wrapper pauses only `git bundle create` before executing the
real Git command. This makes signal timing reproducible without a large fixture
or application test hooks. The CLI, locking and cancellation paths are real.
"""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time

import sys
sys.dont_write_bytecode = True

from e2e_support import binary, environment, init_project, run


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
        staging = list(base.rglob('.onthego-stage-*')) + list(base.rglob('native-sessions'))
        temp_home = Path(env['ONTHEGO_HOME']) / 'tmp'
        temporary_payloads = [path for path in temp_home.iterdir() if path.name not in {'.cleanup.lock', '.leases'}] if temp_home.exists() else []
        leases = temp_home / '.leases'
        if leases.exists():
            temporary_payloads.extend(leases.iterdir())
        if staging or temporary_payloads:
            raise AssertionError(f'Cancellation or retry left plaintext temporary data: {staging + temporary_payloads}')
        print(f'PASS: SIGINT during real snapshot capture exited {code}; retry succeeded without lock deletion; no plaintext staging remains.')


if __name__ == '__main__':
    main()
