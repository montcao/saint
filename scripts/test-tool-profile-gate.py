#!/usr/bin/env python3
"""Real herdr negative lifecycle test. Never boots a VM or changes runtime settings.

Requires an already running, incompatible Apple daemon. A supported daemon must
instead run the full positive lifecycle suite described in the release gates.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import uuid


def digest(data):
    return hashlib.sha256(data).hexdigest()


def inventory(container):
    # Hash configuration in memory; never record environments/provider credentials.
    rows = json.loads(subprocess.check_output([container, 'ls', '-a', '--format', 'json']))
    return sorted([{'id': row['id'], 'status': row['status'],
                    'configuration_sha256': digest(json.dumps(row['configuration'], sort_keys=True).encode())}
                   for row in rows], key=lambda x: x['id'])


def file_hash(path):
    return digest(path.read_bytes()) if path.exists() else None


def public_summary(report):
    """Allowlist assertions; never export raw diagnostics or host fingerprints."""
    keys = ('no_session_or_vm_created', 'unrelated_state_unchanged',
            'owned_server_exited', 'owned_scratch_removed')
    return dict(kind='synthetic-profile-gate',
                neighbor_responsive=all(sample.get('neighbor_responsive') is True
                                        for sample in report.get('samples', []))
                                    and bool(report.get('samples')),
                **{key: report.get(key) is True for key in keys})


def write_summary(path, report):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    # Exclusive creation prevents overwriting personal files or following a link.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as output:
        json.dump(public_summary(report), output, indent=2)
        output.write('\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('saint', 'adapter', 'herdr', 'output'):
        parser.add_argument('--' + name, required=True, type=Path)
    parser.add_argument('--container', default=shutil.which('container'))
    args = parser.parse_args()
    assert args.container, 'container CLI required'
    for binary in (args.saint, args.adapter, args.herdr):
        assert binary.is_absolute() and binary.is_file(), binary
    # The helper's actual daemon check must reject before test resources exist.
    probe = subprocess.run([str(args.adapter), 'probe'], capture_output=True, text=True, timeout=15,
                           env={'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'HOME': str(Path.home())})
    assert probe.returncode != 0 and 'Requires pinned API server' in probe.stderr, probe.stderr
    root = Path(tempfile.mkdtemp(prefix='spg-', dir='/private/tmp'))
    root.chmod(0o700)
    session = 'sp-' + uuid.uuid4().hex[:6]
    home = Path.home()
    protected = {'saint_sessions': home/'.saint/sessions.json',
                 'herdr_config': home/'.config/herdr/config.toml',
                 'container_config': home/'Library/Application Support/com.apple.container/config.toml'}
    before = inventory(args.container)
    hashes = {key: file_hash(path) for key, path in protected.items()}
    hashes['runtime_properties'] = digest(subprocess.check_output([args.container, 'system', 'property', 'list']))
    report = {'samples': [], 'no_session_or_vm_created': False}
    server = None
    env = {'HOME': str(root/'home'), 'PATH': '/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin',
           'SHELL': '/bin/sh', 'TERM': 'xterm-256color', 'SAINT_HOME': str(root/'state'),
           'HERDR_CONFIG_PATH': str(root/'config.toml')}
    base = [str(args.herdr), '--session', session]

    def call(argv, check=True):
        result = subprocess.run(base+argv, env=env, capture_output=True, text=True, timeout=25)
        if check:
            assert result.returncode == 0, (argv, result.stderr)
        return result

    def panes(workspace):
        return json.loads(call(['pane', 'list', '--workspace', workspace]).stdout)['result']['panes']

    try:
        for name in ('home', 'state', 'repo', 'plugin', 'artifacts'):
            (root/name).mkdir(mode=0o700)
        (root/'config.toml').write_text('[terminal]\ndefault_shell="/bin/sh"\nshell_mode="non_login"\n'
                                      '[update]\nversion_check=false\nmanifest_check=false\n')
        subprocess.run(['/usr/bin/git', 'init', '-q', str(root/'repo')], env=env, check=True)
        shutil.copy2(args.saint, root/'plugin/saint')
        repo = Path(__file__).resolve().parent.parent
        shutil.copy2(repo/'herdr-plugin.toml', root/'plugin/herdr-plugin.toml')

        def artifact(name, data, mode=0o600):
            path = root/'artifacts'/name
            path.write_bytes(data)
            path.chmod(mode)
            return {'path': str(path), 'sha256': digest(data)}

        tool = artifact('tools.erofs', b'Negative gate fixture; deliberately not bootable.\n')
        pinned = 'sha256:'+'1'*64
        manifest = {'schema': 1, 'runtime_commit': 'a15b46097aa4c2325da13c1fb5192b9274aab083',
                    'format': 'erofs', 'adapter': artifact('adapter', args.adapter.read_bytes(), 0o700),
                    'tools': tool, 'tool_sha256': tool['sha256'], 'root': artifact('root.ext4', b'gate-only'),
                    'kernel': artifact('kernel', b'gate-only'),
                    'review': artifact('review.txt', b'Negative test only. No qualified boot artifacts.'),
                    'image_reference': 'example.invalid/gate@'+pinned, 'image_digest': pinned,
                    'init_reference': 'example.invalid/init@'+pinned, 'init_digest': pinned}
        manifest_artifact = artifact('manifest.json', json.dumps(manifest, sort_keys=True).encode())
        ref = manifest_artifact['path']+'#'+manifest_artifact['sha256']
        with (root/'server.log').open('wb') as log:
            server = subprocess.Popen(base+['server'], env=env, cwd=root/'repo',
                                      stdin=subprocess.DEVNULL, stdout=log, stderr=log)
        (root/'owner.json').write_text(json.dumps({'session': session, 'pid': server.pid,
                                                 'directory': str(root), 'purpose': 'negative gate'}))
        for _ in range(50):
            assert server.poll() is None, 'owned herdr server exited'
            if call(['workspace', 'list'], check=False).returncode == 0:
                break
            time.sleep(.1)
        else:
            raise RuntimeError('owned server did not become ready')
        created = json.loads(call(['workspace', 'create', '--cwd', str(root/'repo'),
                                   '--label', 'Saint gate', '--no-focus']).stdout)['result']
        workspace, source = created['workspace']['workspace_id'], created['root_pane']['pane_id']
        call(['plugin', 'link', str(root/'plugin')])
        action_env = dict(env, HERDR_BIN_PATH=str(args.herdr),
                          HERDR_SOCKET_PATH=str(root/'home/.config/herdr/sessions'/session/'herdr.sock'),
                          HERDR_PLUGIN_CONTEXT_JSON=json.dumps({'workspace_id': workspace,
                          'focused_pane_id': source, 'focused_pane_cwd': str(root/'repo')}))
        for attempt in range(3):
            previous = {p['pane_id'] for p in panes(workspace)}
            subprocess.run([str(root/'plugin/saint'), 'herdr', 'open', 'claude',
                            '--experimental-profile', ref], env=action_env, check=True, timeout=25)
            new = [p for p in panes(workspace) if p['pane_id'] not in previous]
            assert len(new) == 1, new
            pane = new[0]['pane_id']
            output = json.loads(call(['pane', 'wait-output', pane, '--match',
                                      'Press Enter to close this pane.', '--timeout', '15000',
                                      '--source', 'recent-unwrapped']).stdout)['result']['read']['text']
            assert ('requires the pinned Apple container CLI' in output or
                    'experimental adapter probe failed' in output), 'expected unsupported-runtime gate'
            assert {source, pane}.issubset({p['pane_id'] for p in panes(workspace)})
            # A neighboring owned shell remains usable while the failed pane waits.
            marker = 'SAINT_NEIGHBOR_'+str(attempt)
            call(['pane', 'run', source, "printf 'SAINT_NEIGHBOR_%s\\n' "+str(attempt)])
            call(['pane', 'wait-output', source, '--match', marker, '--timeout', '5000'])
            if attempt == 0:
                call(['pane', 'send-keys', pane, 'enter'])
                # The pane's process exits; herdr may retain an exited pane.
                time.sleep(.2)
            if pane in {p['pane_id'] for p in panes(workspace)}:
                call(['plugin', 'pane', 'close', pane])
            assert {p['pane_id'] for p in panes(workspace)} == previous
            report['samples'].append({'neighbor_responsive': True})
            print('herdr negative lifecycle sample', attempt+1, 'passed', flush=True)
        assert not list((root/'state/sessions').glob('*')), 'gate created session state'
        assert not (root/'state/tool-profiles').exists(), 'gate reserved tool resources'
        report['no_session_or_vm_created'] = True
    finally:
        if server is not None and server.poll() is None:
            call(['server', 'stop'], check=False)
            try:
                server.wait(timeout=10)
            except subprocess.TimeoutExpired:
                # Exact Popen child only; never signal a discovered/global server.
                server.terminate()
                server.wait(timeout=10)
        report['after'] = inventory(args.container)
        report['protected_after'] = {key: file_hash(path) for key, path in protected.items()}
        report['protected_after']['runtime_properties'] = digest(subprocess.check_output([args.container, 'system', 'property', 'list']))
        report['unrelated_state_unchanged'] = report['after'] == before and report['protected_after'] == hashes
        report['owned_server_exited'] = server is None or server.poll() is not None
        if report['owned_server_exited']:
            shutil.rmtree(root)
        report['owned_scratch_removed'] = not root.exists()
        write_summary(args.output, report)
    assert report['unrelated_state_unchanged'], 'unrelated state changed during test'
    print('owned cleanup and unrelated-state comparison passed', flush=True)


if __name__ == '__main__':
    main()
