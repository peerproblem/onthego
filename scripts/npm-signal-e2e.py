#!/usr/bin/env python3
"""Check the npm launcher's signal forwarding using an isolated native fixture."""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True

from e2e_support import REPO


def wait_for(path, process):
    deadline = time.monotonic() + 5
    while not path.exists():
        if process.poll() is not None:
            raise AssertionError(f'Process exited before fixture readiness: {process.returncode}')
        if time.monotonic() > deadline:
            raise AssertionError('Native fixture did not become ready')
        time.sleep(0.01)


def run_case(command, base, sig, group, graceful):
    marker = base / 'ready'
    marker.unlink(missing_ok=True)
    env = dict(os.environ, ONTHEGO_SIGNAL_READY=str(marker),
               ONTHEGO_SIGNAL_GRACEFUL='1' if graceful else '0')
    process = subprocess.Popen(command, env=env, stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True)
    child_pid = None
    child_exited = False
    try:
        wait_for(marker, process)
        child_pid = int(marker.read_text())
        started = time.monotonic()
        if group:
            os.killpg(process.pid, sig)
        else:
            process.send_signal(sig)
        try:
            stdout, stderr = process.communicate(timeout=2)
        except subprocess.TimeoutExpired:
            raise AssertionError('Signal left the native child or launcher alive')
        if time.monotonic() - started >= 1:
            raise AssertionError('Signal termination exceeded one second')
        try:
            os.kill(child_pid, 0)
        except ProcessLookupError:
            child_exited = True
        else:
            raise AssertionError(f'Native child {child_pid} survived launcher exit')
        expected = 130 if graceful else -sig
        if process.returncode != expected:
            raise AssertionError(f'Exit {process.returncode}, wanted {expected}: {stdout!r} {stderr!r}')
        return process.returncode
    finally:
        if process.poll() is None:
            process.kill()
        if child_pid is not None and not child_exited:
            try:
                os.kill(child_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        process.wait()


def main():
    node = shutil.which('node')
    if node is None:
        raise RuntimeError('Node 18 or newer is required')
    platform = subprocess.check_output([node, '-p', 'process.platform+"-"+process.arch'], text=True).strip()
    binaries = {'darwin-arm64': 'onthego-darwin-arm64', 'darwin-x64': 'onthego-darwin-amd64',
                'linux-arm64': 'onthego-linux-arm64', 'linux-x64': 'onthego-linux-amd64'}
    with tempfile.TemporaryDirectory(prefix='onthego-npm-signals-') as temporary:
        base = Path(temporary)
        (base / 'bin').mkdir()
        (base / 'vendor').mkdir()
        launcher = base / 'bin' / 'onthego.js'
        shutil.copy2(REPO / 'npm' / 'bin' / 'onthego.js', launcher)
        native = base / 'vendor' / binaries[platform]
        native.write_text(f'#!{sys.executable}\n' + '''import os,signal,sys,time
from pathlib import Path

if len(sys.argv) > 1:
    sys.exit(int(sys.argv[1]))

def canceled(signum, frame):
    # Match the Go CLI: cancellation starts cleanup and restores defaults.
    # A duplicate forwarded group signal would interrupt cleanup and change
    # this expected ordinary 130 exit into termination by a signal.
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, signal.SIG_DFL)
    time.sleep(0.1)
    sys.exit(130)

for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    signal.signal(sig, canceled if os.environ["ONTHEGO_SIGNAL_GRACEFUL"] == "1" else signal.SIG_DFL)
marker = Path(os.environ["ONTHEGO_SIGNAL_READY"])
staged = marker.with_suffix(".tmp")
staged.write_text(str(os.getpid()))
staged.replace(marker)
while True:
    signal.pause()
''')
        native.chmod(0o755)
        count = 0
        for graceful in (False, True):
            for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
                expected = run_case([str(native)], base, sig, False, graceful)
                for group in (False, True):
                    actual = run_case([node, str(launcher)], base, sig, group, graceful)
                    if actual != expected:
                        raise AssertionError('Launcher status differed from direct native execution')
                    count += 1
        for code in (0, 7):
            ordinary = subprocess.run([node, str(launcher), str(code)], capture_output=True, timeout=5)
            if ordinary.returncode != code:
                raise AssertionError(f'Launcher did not preserve normal exit {code}: {ordinary.returncode}')
        native.chmod(0o600)
        failed = subprocess.run([node, str(launcher)], capture_output=True, text=True, timeout=5)
        if failed.returncode != 1 or 'EACCES' not in failed.stderr:
            raise AssertionError(f'Spawn failure did not exit 1: {failed.returncode} {failed.stderr}')
        print(f'PASS: {count} launcher-only and process-group signal cases match native status, '
              'finish within one second, leave no child; normal exit status is preserved; spawn errors exit 1.')


if __name__ == '__main__':
    main()
