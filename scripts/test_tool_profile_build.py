#!/usr/bin/env python3
"""Synthetic artifact checks. No runtime, network, disks or hdiutil calls."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


profile = load('profile', 'make-tool-profile.py')
wrapper = load('wrapper', 'wrap-tool-disk.py')


class BuildBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.inputs = {}
        for name in ('adapter', 'tools', 'root', 'kernel', 'review', 'packages'):
            p = self.root / name
            p.write_text('synthetic fixture: ' + name)
            p.chmod(0o600)
            self.inputs[name] = p
        receipt = dict(schema=1, architecture='arm64',
                       tools_sha256=profile.artifact(self.inputs['tools'])['sha256'],
                       root_sha256=profile.artifact(self.inputs['root'])['sha256'],
                       packages_sha256=profile.artifact(self.inputs['packages'])['sha256'])
        p = self.root / 'receipt'
        p.write_text(json.dumps(receipt))
        p.chmod(0o600)
        argv = []
        values = dict(self.inputs, receipt=p, raw_tools=self.inputs['tools'], format='erofs',
                      image='example.invalid/scaffold@sha256:' + '1' * 64,
                      init='example.invalid/init@sha256:' + '2' * 64, output=self.root / 'manifest')
        for key, value in values.items():
            argv += ['--' + key.replace('_', '-'), str(value)]
        self.args = profile.parser().parse_args(argv)

    def test_manifest_pins_exact_artifacts_and_refuses_overwrite(self):
        ref = profile.write_manifest(self.args)
        out = Path(self.args.output)
        self.assertEqual(ref.split('#')[1], profile.artifact(out)['sha256'])
        self.assertEqual(out.stat().st_mode & 0o777, 0o600)
        before = out.read_bytes()
        with self.assertRaises(FileExistsError):
            profile.write_manifest(self.args)
        self.assertEqual(out.read_bytes(), before)

    def test_tampered_artifact_prevents_output(self):
        self.inputs['tools'].write_text('tampered')
        with self.assertRaises(ValueError):
            profile.write_manifest(self.args)
        self.assertFalse(Path(self.args.output).exists())

    def test_aliases_are_not_trusted_artifacts(self):
        alias = self.root / 'alias'
        alias.symlink_to(self.inputs['tools'])
        with self.assertRaises(ValueError):
            profile.artifact(alias)
        alias.unlink()
        os.link(self.inputs['tools'], alias)
        with self.assertRaises(ValueError):
            profile.artifact(self.inputs['tools'])

    def test_wrapper_failure_retains_owned_scratch(self):
        digest = profile.artifact(self.inputs['tools'])['sha256']
        with patch.object(wrapper.subprocess, 'run', side_effect=subprocess.TimeoutExpired('synthetic', 1)):
            with self.assertRaisesRegex(RuntimeError, 'retained owned scratch'):
                wrapper.wrap(self.inputs['tools'], digest, self.root / 'candidate.dmg')
        scratch = list(self.root.glob('.ulfo-*'))
        self.assertEqual(len(scratch), 1)
        self.assertTrue((scratch[0] / 'owner.json').is_file())
        self.assertTrue((scratch[0] / 'source/tools.erofs').is_file())

    def test_wrapper_preserves_existing_destination(self):
        dest = self.root / 'candidate.dmg'
        dest.write_bytes(b'neighbor')
        with patch.object(wrapper.subprocess, 'run') as run:
            with self.assertRaises(ValueError):
                wrapper.wrap(self.inputs['tools'], profile.artifact(self.inputs['tools'])['sha256'], dest)
        run.assert_not_called()
        self.assertEqual(dest.read_bytes(), b'neighbor')


if __name__ == '__main__':
    unittest.main()
