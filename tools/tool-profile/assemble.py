#!/usr/bin/env python3
"""Assemble matched raw disks offline inside the dedicated Linux build stage.

No mounts, loop devices, host credentials, or privileged container are needed.
The output directory must not exist. Builder paths are not included in receipts.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

EPOCH = 1793491200  # fixed input timestamp, not a measurement
REQUIRED = ('bash', 'git', 'python3', 'ssh', 'rg', 'curl', 'less')


def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()


def assemble(tree, out):
    if os.uname().machine not in ('aarch64', 'arm64'):
        raise ValueError('ARM64 Linux builder required')
    if out.exists() or out.is_symlink():
        raise ValueError('output must be new')
    for name in REQUIRED:
        if not (tree / 'usr/bin' / name).is_file():
            raise ValueError('incomplete tool profile: ' + name)
    agent = tree / 'usr/local/bin/claude'
    with agent.open('rb') as f:
        header = f.read(20)
    if header[:4] != b'\x7fELF' or int.from_bytes(header[18:20], 'little') != 183:
        raise ValueError('Claude must be native ARM64 ELF')
    out.mkdir(mode=0o700)
    root = out / 'root-tree'
    root.mkdir()
    shutil.copytree(tree / 'etc', root / 'etc', symlinks=True)
    # Do not carry builder/host trust additions or credentials into a guest.
    for p in (root / 'etc/ssh').glob('ssh_host_*'):
        p.unlink()
    for name, content in {'hosts': '127.0.0.1 localhost\n::1 localhost\n',
                          'hostname': 'saint\n', 'resolv.conf': '', 'machine-id': ''}.items():
        path = root / 'etc' / name
        if path.exists() or path.is_symlink():
            path.unlink()
        path.write_text(content)
    for name in ('usr', 'dev', 'proc', 'sys', 'tmp', 'run', 'var/tmp', 'home/agent', 'saint'):
        (root / name).mkdir(parents=True, exist_ok=True)
    os.chmod(root / 'tmp', 0o1777)
    os.chmod(root / 'var/tmp', 0o1777)
    os.chown(root / 'home/agent', 1000, 1000)
    for name, target in [('bin', 'usr/bin'), ('sbin', 'usr/sbin'), ('lib', 'usr/lib')]:
        (root / name).symlink_to(target)
    # Fixed formatting parameters; reproducibility means pinned inputs/recipe,
    # not a claim that ext4 output is byte-identical across toolchain revisions.
    subprocess.run(['mkfs.erofs', '-T', str(EPOCH), str(out / 'tools.erofs'), str(tree / 'usr')], check=True)
    disk = out / 'root.ext4'
    with disk.open('xb') as f:
        f.truncate(256 * 1024 * 1024)
    subprocess.run(['mke2fs', '-q', '-t', 'ext4', '-b', '4096', '-m', '0',
                    '-E', 'lazy_itable_init=0,lazy_journal_init=0',
                    '-d', str(root), str(disk)], check=True)
    subprocess.run(['e2fsck', '-fn', str(disk)], check=True)
    inventory = subprocess.check_output(['dpkg-query', '--admindir=' + str(tree / 'var/lib/dpkg'), '-W',
                                        '-f=${Package}\t${Version}\t${Architecture}\n'], text=True)
    (out / 'packages.tsv').write_text(inventory)
    # Inventory includes Debian runtime closure; Claude's hash is recorded separately.
    receipt = {'schema': 1, 'architecture': 'arm64', 'root_bytes': disk.stat().st_size,
               'tools_sha256': sha(out / 'tools.erofs'), 'root_sha256': sha(disk),
               'claude_sha256': sha(agent), 'packages_sha256': sha(out / 'packages.tsv')}
    (out / 'build-receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
    shutil.rmtree(root)  # solely the directory created above
    for p in out.iterdir():
        p.chmod(0o600)


if __name__ == '__main__':
    if len(sys.argv) != 3:
        raise SystemExit('usage: assemble.py ROOT_TREE NEW_OUTPUT')
    assemble(Path(sys.argv[1]), Path(sys.argv[2]))
