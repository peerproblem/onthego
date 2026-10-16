#!/usr/bin/env python3
"""Real OpenSSH device add -> push -> remote edit -> pull integration test.

Linux with root or passwordless sudo creates and removes a dedicated account.
Other hosts try an unprivileged sshd as the current user, with isolated keys,
known-hosts, configuration and ONTHEGO/agent homes. No running SSH service is used.
"""
import os
from pathlib import Path
import pwd
import shlex
import shutil
import socket
import subprocess
import tempfile
import time

import sys
sys.dont_write_bytecode = True

from e2e_support import binary, environment, init_project, peer_wrapper, run


def main():
    cli = binary()
    sshd = shutil.which('sshd') or '/usr/sbin/sshd'
    ssh = shutil.which('ssh')
    if not Path(sshd).is_file() or not ssh:
        raise RuntimeError('OpenSSH client and server are required (Ubuntu: apt-get install openssh-server)')
    with tempfile.TemporaryDirectory(prefix='onthego-ssh-e2e-') as temporary:
        base = Path(temporary).resolve()
        base.chmod(0o755)
        local_env = environment(base / 'local')
        remote_env = environment(base / 'remote')
        source, target = base / 'source', base / 'remote' / 'project'
        remote_cli = base / 'remote' / 'onthego'
        shutil.copy2(cli, remote_cli)
        wrapper = peer_wrapper(base / 'remote' / 'peer', remote_cli, remote_env)
        username = pwd.getpwuid(os.getuid()).pw_name
        privilege = []
        temporary_user = False
        server = None
        logfile = None
        try:
            if os.uname().sysname == 'Linux':
                if os.getuid() == 0:
                    privilege = []
                    can_admin = True
                else:
                    can_admin = shutil.which('sudo') and subprocess.run(['sudo', '-n', 'true'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
                    privilege = ['sudo', '-n'] if can_admin else []
                if can_admin:
                    username = f'otgtest{os.getpid()}'
                    run(privilege + ['useradd', '--no-create-home', '--home-dir', remote_env['HOME'], '--shell', '/bin/sh', username])
                    temporary_user = True
                    run(privilege + ['usermod', '-p', '*', username])
                    run(privilege + ['chown', '-R', username, str(base / 'remote')])
                    run(privilege + ['mkdir', '-p', '/run/sshd'])
            with socket.socket() as listener:
                listener.bind(('127.0.0.1', 0))
                port = listener.getsockname()[1]
            for name in ['host_key', 'client_key']:
                run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', base / name])
            host_public = (base / 'host_key.pub').read_text().split()
            (base / 'known_hosts').write_text(f'[127.0.0.1]:{port} {host_public[0]} {host_public[1]}\n')
            config = base / 'sshd_config'
            config.write_text(f'''Port {port}
ListenAddress 127.0.0.1
HostKey {base / 'host_key'}
PidFile {base / 'sshd.pid'}
AuthorizedKeysFile {base / 'client_key.pub'}
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
UsePAM no
AllowUsers {username}
AllowTcpForwarding no
X11Forwarding no
PermitTunnel no
PermitTTY no
PermitRootLogin prohibit-password
LogLevel ERROR
''')
            client_config = base / 'ssh_config'
            client_config.write_text(f'''Host *
  IdentitiesOnly yes
  IdentityAgent none
  IdentityFile {base / 'client_key'}
  UserKnownHostsFile {base / 'known_hosts'}
  GlobalKnownHostsFile /dev/null
  StrictHostKeyChecking yes
  BatchMode yes
''')
            client_bin = base / 'client-bin'
            client_bin.mkdir()
            # Go executes ssh from PATH. This wrapper only selects the isolated
            # OpenSSH config; it still executes the real ssh client and server.
            (client_bin / 'ssh').write_text(f'#!/bin/sh\nexec {shlex.quote(ssh)} -F {shlex.quote(str(client_config))} "$@"\n')
            (client_bin / 'ssh').chmod(0o755)
            local_env['PATH'] = str(client_bin) + os.pathsep + os.environ['PATH']
            logfile = (base / 'sshd.log').open('w+')
            server = subprocess.Popen(privilege + [sshd, '-D', '-e', '-f', str(config)], stdout=logfile, stderr=logfile)
            for _ in range(100):
                if server.poll() is not None:
                    logfile.seek(0)
                    raise RuntimeError('Isolated sshd exited: ' + logfile.read())
                try:
                    with socket.create_connection(('127.0.0.1', port), timeout=0.1):
                        break
                except OSError:
                    time.sleep(0.05)
            else:
                raise RuntimeError('Isolated sshd did not start')
            init_project(source, cli, local_env)
            run([cli, 'device', 'add', 'loopback', '--host', f'{username}@127.0.0.1', '--port', port,
                 '--identity-file', base / 'client_key', '--path', target, '--binary', wrapper], cwd=source, env=local_env)
            (source / 'work.txt').write_text('base\nstaged\n')
            run(['git', 'add', 'work.txt'], cwd=source, env=local_env)
            with (source / 'work.txt').open('a') as file:
                file.write('unstaged\n')
            (source / 'draft.txt').write_text('untracked\n')
            run([cli, 'push', 'loopback'], cwd=source, env=local_env)
            ssh_args = [ssh, '-F', client_config, '-p', port, f'{username}@127.0.0.1']
            target_worktree = run(ssh_args + [f'cat {shlex.quote(str(target / "work.txt"))}'])
            if (source / 'work.txt').read_text().strip() != target_worktree:
                raise AssertionError('SSH push did not preserve the worktree')
            source_index = run(['git', 'show', ':work.txt'], cwd=source, env=local_env)
            target_index = run(ssh_args + [f'git -C {shlex.quote(str(target))} show :work.txt'])
            if source_index != target_index:
                raise AssertionError('SSH push did not preserve staged changes')
            edit = f"printf 'remote edit\\n' >> {shlex.quote(str(target / 'draft.txt'))}"
            run(ssh_args + [edit])
            run([cli, 'pull', 'loopback'], cwd=source, env=local_env)
            if (source / 'draft.txt').read_text() != 'untracked\nremote edit\n':
                raise AssertionError('SSH pull did not return the remote edit')
            print('PASS: real OpenSSH pairing, push, remote edit and pull with isolated identities and agent homes.')
        finally:
            if server is not None and server.poll() is None:
                if privilege:
                    run(privilege + ['kill', str(server.pid)])
                else:
                    server.terminate()
                try:
                    server.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait()
            if logfile:
                logfile.close()
            if temporary_user:
                run(privilege + ['userdel', username])
                # Remove a dedicated user's files before TemporaryDirectory's
                # unprivileged cleanup, without touching any real home.
                run(privilege + ['rm', '-rf', str(base / 'remote')])


if __name__ == '__main__':
    main()
