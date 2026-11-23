#!/usr/bin/env python3
"""Synthetic regression checks for the gate harness's public-output boundary."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location(
    'gate', Path(__file__).with_name('test-tool-profile-gate.py'))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class PublicOutputTests(unittest.TestCase):
    def test_private_fields_never_leave_the_allowlist(self):
        private = 'synthetic-private-sentinel'
        report = dict(before=private, after=private, protected_before=private,
                      protected_after=private, runtime_probe=private,
                      machine=private, local_path=private, credential=private,
                      samples=[dict(neighbor_responsive=True, timing=private)],
                      no_session_or_vm_created=True, unrelated_state_unchanged=True,
                      owned_server_exited=True, owned_scratch_removed=True)
        result = gate.public_summary(report)
        self.assertNotIn(private, json.dumps(result))
        self.assertEqual(set(result), {'kind', 'neighbor_responsive',
                         'no_session_or_vm_created', 'unrelated_state_unchanged',
                         'owned_server_exited', 'owned_scratch_removed'})
        # An arbitrary string in a permitted slot must not escape either.
        report['owned_server_exited'] = private
        self.assertIs(gate.public_summary(report)['owned_server_exited'], False)

    def test_output_is_private_and_cannot_replace_a_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/'report.private.json'
            gate.write_summary(path, {})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            before = path.read_bytes()
            with self.assertRaises(FileExistsError):
                gate.write_summary(path, {'owned_server_exited': True})
            self.assertEqual(path.read_bytes(), before)
            alias = Path(directory)/'alias'
            alias.symlink_to(path)
            with self.assertRaises(FileExistsError):
                gate.write_summary(alias, {})


if __name__ == '__main__':
    unittest.main()
