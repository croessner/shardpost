"""Tests of quota_warning.py with a fake shardpostctl; no server needed."""
import contextlib
import email
import io
import email.policy
import os
import stat
import tempfile
import unittest

import quota_warning

EVENT = {
    'QUOTA_ACCOUNT': 'user@example.org', 'QUOTA_WARNING': 'warn95', 'QUOTA_RESOURCE': 'storage',
    'QUOTA_PERCENT': '95', 'QUOTA_DIRECTION': 'over', 'QUOTA_STORAGE_USAGE': str(973 << 20),
    'QUOTA_STORAGE_LIMIT': str(1 << 30), 'QUOTA_MESSAGES_USAGE': '10', 'QUOTA_MESSAGES_LIMIT': '100000',
    'WARN_FROM': 'postmaster@example.org',
}


class WarningTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.args = os.path.join(self.dir.name, 'args')
        self.mail = os.path.join(self.dir.name, 'mail')

    def fake_ctl(self, code, line):
        """A shardpostctl that records its arguments and stdin."""
        path = os.path.join(self.dir.name, 'shardpostctl')
        with open(path, 'w') as f:
            f.write('#!/bin/sh\nprintf "%%s\\n" "$@" > %s\ncat > %s\necho "%s"\nexit %d\n' % (self.args, self.mail, line, code))
        os.chmod(path, stat.S_IRWXU)
        return path

    def run_main(self, code=0, line='user@example.org: stored in INBOX uid 7', **extra):
        env = dict(EVENT, WARN_SHARDPOSTCTL=self.fake_ctl(code, line), **extra)
        with contextlib.redirect_stdout(io.StringIO()):
            return quota_warning.main(env)

    def test_delivers_rendered_warning(self):
        self.assertEqual(self.run_main(), 0)
        with open(self.args) as f:
            args = f.read().split('\n')
        self.assertEqual(args[:5], ['deliver', '-d', 'user@example.org', '-f', 'postmaster@example.org'])
        self.assertIn('--no-enforce-quota', args)
        with open(self.mail, 'rb') as f:
            msg = email.message_from_bytes(f.read(), policy=email.policy.default)
        self.assertIn('95 %', msg['Subject'])
        self.assertEqual(msg['Auto-Submitted'], 'auto-generated')
        self.assertTrue(msg['Message-ID'].endswith('@example.org>'))
        body = msg.get_content()
        self.assertIn('973.0 MiB von 1.0 GiB', body)
        self.assertIn('[English]', body)

    def test_under_uses_its_own_template(self):
        self.assertEqual(self.run_main(QUOTA_DIRECTION='under', QUOTA_PERCENT='100', WARN_ENFORCE_QUOTA='true', WARN_SIEVE='false'), 0)
        with open(self.args) as f:
            args = f.read().split('\n')
        self.assertNotIn('--no-enforce-quota', args)
        self.assertIn('--no-sieve', args)
        with open(self.mail, 'rb') as f:
            msg = email.message_from_bytes(f.read(), policy=email.policy.default)
        self.assertIn('wieder unter 100 %', msg['Subject'])

    def test_exit_codes(self):
        self.assertEqual(self.run_main(75, 'shardpostctl: temporary: try again'), 75)
        self.assertEqual(self.run_main(67, 'shardpostctl: no_such_user: no such user'), 1)
        self.assertEqual(self.run_main(QUOTA_ACCOUNT='a b@example.org'), 1)
        self.assertEqual(self.run_main(QUOTA_DIRECTION='sideways'), 1)
        self.assertEqual(self.run_main(WARN_FROM=''), 1)
        self.assertEqual(self.run_main(WARN_TEMPLATE_OVER='/nonexistent'), 1)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(quota_warning.main(dict(EVENT, WARN_SHARDPOSTCTL='/nonexistent/shardpostctl')), 75)

    def test_injection_and_socket(self):
        self.assertEqual(self.run_main(WARN_FROM='postmaster@example.org\nBcc: x@example.org'), 1)
        self.assertEqual(self.run_main(QUOTA_ACCOUNT='user@example.org\r\nBcc: x@example.org'), 1)
        self.assertEqual(self.run_main(WARN_ADMIN_SOCKET='/run/other.sock'), 0)
        with open(self.args) as f:
            self.assertEqual(f.read().split('\n')[:2], ['-socket', '/run/other.sock'])

    def test_encoding(self):
        own = os.path.join(self.dir.name, 'own.txt')
        with open(own, 'w', encoding='utf-8') as f:
            f.write('Subject: Größe ${percent} %\n\nÜber ${percent} %.\n')
        self.assertEqual(self.run_main(WARN_TEMPLATE_OVER=own), 0)
        with open(self.mail, 'rb') as f:
            raw = f.read()
        self.assertIn(b'\r\n', raw)
        self.assertNotIn(b'\n', raw.replace(b'\r\n', b''))
        self.assertIn(b'Subject: =?utf-8?', raw)
        msg = email.message_from_bytes(raw, policy=email.policy.default)
        self.assertEqual(msg['Subject'], 'Größe 95 %')
        self.assertIn('Über 95 %.', msg.get_content())

    def test_deadline(self):
        slow = os.path.join(self.dir.name, 'slow')
        with open(slow, 'w') as f:
            f.write('#!/bin/sh\ncat >/dev/null\nsleep 5\n')
        os.chmod(slow, stat.S_IRWXU)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(quota_warning.main(dict(EVENT, WARN_SHARDPOSTCTL=slow, WARN_DEADLINE='2')), 75)

    def test_template_format(self):
        bad = os.path.join(self.dir.name, 'bad.txt')
        with open(bad, 'w') as f:
            f.write('no subject line\n\nbody\n')
        self.assertEqual(self.run_main(WARN_TEMPLATE_OVER=bad), 1)

    def test_human(self):
        self.assertEqual(quota_warning.human('512'), '512 B')
        self.assertEqual(quota_warning.human(str(3 << 30)), '3.0 GiB')
        self.assertEqual(quota_warning.human('x'), 'x')


if __name__ == '__main__':
    unittest.main()
