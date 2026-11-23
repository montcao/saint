#!/usr/bin/env python3
"""Explicit developer-only ULFO wrapping, without changing the container runtime.

Create a new private output from a verified raw disk. hdiutil may temporarily
mount its own image during creation; no existing mount or image is detached.
"""
import argparse
import plistlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import importlib.util

spec = importlib.util.spec_from_file_location('profile', Path(__file__).with_name('make-tool-profile.py'))
profile = importlib.util.module_from_spec(spec)
spec.loader.exec_module(profile)


def wrap(raw, digest, output):
    raw = profile.private_path(raw)
    if not profile.DIGEST.fullmatch(digest) or profile.artifact(raw)['sha256'] != digest:
        raise ValueError('raw disk digest differs')
    dest = Path(output)
    profile.private_path(dest.parent, directory=True)
    if dest.exists() or dest.is_symlink() or not dest.is_absolute():
        raise ValueError('output must be new and absolute')
    # Reserve destination before invoking hdiutil. Never overwrite a prior disk.
    fd = os.open(dest, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    tmp = Path(tempfile.mkdtemp(prefix='.ulfo-', dir=dest.parent))
    # A failed or interrupted disk-image operation may have left a mount alive.
    # Keep its scratch and receipt until an operator can establish safe cleanup.
    (tmp / 'owner.json').write_text(__import__('json').dumps({
        'schema': 1, 'uid': os.getuid(), 'raw_sha256': digest,
        'output': str(dest), 'image': str(tmp / 'tools.dmg')}))
    (tmp / 'owner.json').chmod(0o600)
    try:
        src = tmp / 'source'
        src.mkdir(mode=0o700)
        shutil.copyfile(raw, src / 'tools.erofs')
        (src / 'tools.erofs').chmod(0o600)
        if profile.artifact(src / 'tools.erofs')['sha256'] != digest:
            raise ValueError('raw source changed during copy')
        image = tmp / 'tools.dmg'
        env = {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin'}
        subprocess.run(['/usr/bin/hdiutil', 'create', '-fs', 'APFS', '-format', 'ULFO',
                        '-srcfolder', str(src), '-volname', 'SaintTools', str(image)],
                       check=True, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, env=env, timeout=300)
        info = subprocess.check_output(['/usr/bin/hdiutil', 'info', '-plist'],
                                       env=env, stderr=subprocess.DEVNULL, timeout=15)
        for mounted in plistlib.loads(info).get('images', []):
            backing = Path(mounted.get('image-path', '/'))
            if backing == tmp or tmp in backing.parents:
                raise ValueError('owned scratch image is still attached')
        with image.open('rb') as f, os.fdopen(fd, 'wb', closefd=False) as out:
            shutil.copyfileobj(f, out, 1024 * 1024)
            out.flush()
            os.fsync(fd)
        shutil.rmtree(tmp)  # exact private scratch, only after absence check
    except BaseException as exc:
        raise RuntimeError('ULFO operation incomplete; retained owned scratch at ' + str(tmp)) from exc
    finally:
        os.close(fd)
    return profile.artifact(dest)['sha256']


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--raw', required=True)
    p.add_argument('--sha256', required=True)
    p.add_argument('--output', required=True)
    a = p.parse_args()
    print(wrap(a.raw, a.sha256, a.output))
