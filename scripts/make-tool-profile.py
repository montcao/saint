#!/usr/bin/env python3
"""Create an explicitly reviewed, checksum-pinned local profile; never download.

Inputs must already be private, owned artifacts outside all guest mounts.
No credential or runtime inventory is read. The output must be a new file.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat

RUNTIME_COMMIT = 'a15b46097aa4c2325da13c1fb5192b9274aab083'
DIGEST = re.compile(r'[0-9a-f]{64}')


def private_path(path, directory=False):
    path = Path(path)
    if not path.is_absolute() or path != path.resolve():
        raise ValueError('private input must be absolute without symlinks')
    for index, entry in enumerate((path, *path.parents)):
        s = entry.lstat()
        if index == 0:
            valid_type = stat.S_ISDIR(s.st_mode) if directory else stat.S_ISREG(s.st_mode)
            if not valid_type or s.st_uid != os.getuid() or s.st_mode & 0o077:
                raise ValueError('input must be owned and private')
            if not directory and s.st_nlink != 1:
                raise ValueError('hardlink aliases rejected')
        elif not stat.S_ISDIR(s.st_mode) or s.st_uid not in (0, os.getuid()) or (
                s.st_mode & 0o022 and not s.st_mode & stat.S_ISVTX):
            raise ValueError('unsafe ancestor')
    return path


def artifact(path):
    path = private_path(path)
    h = hashlib.sha256()
    with path.open('rb') as f:
        for data in iter(lambda: f.read(1024 * 1024), b''):
            h.update(data)
    return {'path': str(path), 'sha256': h.hexdigest()}


def image_digest(ref):
    parts = ref.rsplit('@sha256:', 1)
    if len(parts) != 2 or not parts[0] or not DIGEST.fullmatch(parts[1]):
        raise ValueError('image must have an explicit SHA256 digest')
    return 'sha256:' + parts[1]


def write_manifest(args):
    receipt_path = private_path(args.receipt)
    if receipt_path.stat().st_size > 65536:
        raise ValueError('receipt too large')
    receipt = json.loads(receipt_path.read_text())
    if receipt.get('schema') != 1 or receipt.get('architecture') != 'arm64':
        raise ValueError('unsupported build receipt')
    m = {name: artifact(getattr(args, name)) for name in ('adapter', 'tools', 'root', 'kernel', 'review')}
    raw = artifact(args.raw_tools)
    if raw['sha256'] != receipt['tools_sha256'] or m['root']['sha256'] != receipt['root_sha256']:
        raise ValueError('disk artifacts differ from the build receipt')
    if artifact(args.packages)['sha256'] != receipt['packages_sha256']:
        raise ValueError('package inventory differs from receipt')
    if args.format == 'erofs' and raw != m['tools']:
        raise ValueError('raw profile must select the verified raw disk')
    m.update(schema=1, runtime_commit=RUNTIME_COMMIT, format=args.format,
             tool_sha256=raw['sha256'], image_reference=args.image,
             image_digest=image_digest(args.image), init_reference=args.init,
             init_digest=image_digest(args.init))
    dest = Path(args.output)
    private_path(dest.parent, directory=True)
    if not dest.is_absolute() or dest.name in ('.', '..'):
        raise ValueError('output must be an absolute new file')
    data = (json.dumps(m, indent=2) + '\n').encode()
    fd = os.open(dest, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'wb') as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
    # Print only the selection reference, never contents of a review or artifact.
    return str(dest) + '#' + hashlib.sha256(data).hexdigest()


def parser():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ('adapter', 'tools', 'raw-tools', 'root', 'kernel', 'review', 'receipt', 'packages', 'image', 'init', 'output'):
        p.add_argument('--' + name, required=True)
    p.add_argument('--format', choices=('erofs', 'ulfo'), required=True)
    return p


if __name__ == '__main__':
    print(write_manifest(parser().parse_args()))
