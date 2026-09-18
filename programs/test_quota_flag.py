"""Tests of quota_flag.py: fake LDAP connection, local HTTP token and
flush endpoints; needs ldap3 (run in the image's venv, see Makefile)."""
import base64
import contextlib
import http.server
import io
import json
import os
import tempfile
import threading
import time
import unittest

import ldap3

import quota_flag


class FakeConn:
    """Records search and modify calls; entries are (dn, value) pairs."""

    def __init__(self, entries, fail=None, delay=0):
        self.entries, self.fail, self.delay = entries, fail, delay
        self.response, self.modified, self.searched, self.unbound = [], [], None, False

    def search(self, base, search, **kw):
        time.sleep(self.delay)
        if self.fail:
            raise self.fail
        self.searched = (base, search, kw)
        self.response = [{'type': 'searchResEntry', 'dn': dn, 'attributes': {'rnsMSOverQuota': [v] if v else []}}
                         for dn, v in self.entries] + [{'type': 'searchResDone'}]

    def modify(self, dn, changes):
        self.modified.append((dn, changes))

    def unbind(self):
        self.unbound = True


class Nauthilus(http.server.BaseHTTPRequestHandler):
    """Token endpoint and cache flush; records the requests."""
    requests = []
    token_status = 200
    flush_status = 200

    def body(self):
        return self.rfile.read(int(self.headers.get('Content-Length', 0)))

    def do_POST(self):
        Nauthilus.requests.append(('POST', self.path, self.headers.get('Authorization'), self.body()))
        self.send_response(self.token_status)
        self.end_headers()
        self.wfile.write(json.dumps({'access_token': 'synthetic-token', 'token_type': 'Bearer'}).encode())

    def do_DELETE(self):
        Nauthilus.requests.append(('DELETE', self.path, self.headers.get('Authorization'), self.body()))
        self.send_response(self.flush_status)
        if self.flush_status == 302:
            self.send_header('Location', '/redirect-must-not-run')
        self.end_headers()
        self.wfile.write(b'{}')

    def log_message(self, *args):
        pass


class FlagTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        secret = os.path.join(self.dir.name, 'secret')
        with open(secret, 'w') as f:
            f.write('synthetic-secret\n')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Nauthilus)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        Nauthilus.requests, Nauthilus.token_status, Nauthilus.flush_status = [], 200, 200
        base = 'http://127.0.0.1:%d' % server.server_port
        self.env = {
            'QUOTA_ACCOUNT': 'user@example.org', 'QUOTA_OVER': 'true',
            'FLAG_LDAP_BASE': 'ou=people,dc=example,dc=org',
            'FLAG_LDAP_FILTER': '(&(objectClass=rnsMSDovecotAccount)(rnsMSDovecotUser={account}))',
            'FLAG_FLUSH_URL': base + '/api/v1/cache/flush', 'FLAG_OIDC_TOKEN_URL': base + '/token',
            'FLAG_OIDC_CLIENT_ID': 'shardpost-quota', 'FLAG_OIDC_CLIENT_SECRET_FILE': secret,
        }

    def run_main(self, conn, **extra):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = quota_flag.main(dict(self.env, **extra), connect=lambda env: conn)
        self.assertNotIn('synthetic-secret', out.getvalue())
        self.assertNotIn('synthetic-token', out.getvalue())
        return code

    def test_sets_target_and_flushes(self):
        conn = FakeConn([('uid=user,ou=people,dc=example,dc=org', 'FALSE')])
        self.assertEqual(self.run_main(conn), 0)
        self.assertEqual(conn.modified, [('uid=user,ou=people,dc=example,dc=org', {'rnsMSOverQuota': [(ldap3.MODIFY_REPLACE, ['TRUE'])]})])
        self.assertIn('(rnsMSDovecotUser=user@example.org)', conn.searched[1])
        self.assertTrue(conn.unbound)
        (_, path, auth, form), (_, fpath, bearer, body) = Nauthilus.requests
        self.assertEqual(path, '/token')
        self.assertEqual(auth, 'Basic ' + base64.b64encode(b'shardpost-quota:synthetic-secret').decode())
        self.assertIn(b'grant_type=client_credentials', form)
        self.assertIn(b'scope=nauthilus%3Aauthenticate+nauthilus%3Asecurity', form)
        self.assertEqual((fpath, bearer, json.loads(body)), ('/api/v1/cache/flush', 'Bearer synthetic-token', {'user': 'user@example.org'}))

    def test_idempotent_and_under(self):
        conn = FakeConn([('uid=user,dc=x', 'FALSE')])
        self.assertEqual(self.run_main(conn, QUOTA_OVER='false'), 0)
        self.assertEqual(conn.modified, [])
        self.assertEqual(len(Nauthilus.requests), 2)  # the cache is flushed anyway

    def test_filter_is_escaped(self):
        # main refuses such accounts; the escaping is the second line of defense.
        conn = FakeConn([('uid=user,dc=x', '')])
        self.assertEqual(quota_flag.set_flag(self.env, conn, 'a*b)(uid=*', True), 'set TRUE')
        self.assertIn(r'a\2ab\29\28uid=\2a', conn.searched[1])

    def test_failures(self):
        self.assertEqual(self.run_main(FakeConn([])), 1)
        self.assertEqual(self.run_main(FakeConn([('a', 'TRUE'), ('b', 'TRUE')])), 1)
        self.assertEqual(self.run_main(FakeConn([], fail=ldap3.core.exceptions.LDAPSocketOpenError('down'))), 75)
        self.assertEqual(self.run_main(FakeConn([('a', 'TRUE')]), QUOTA_ACCOUNT='x y@example.org'), 1)
        self.assertEqual(self.run_main(FakeConn([('a', 'TRUE')]), QUOTA_OVER='maybe'), 1)

    def test_flush_failures_are_temporary(self):
        Nauthilus.flush_status = 403
        self.assertEqual(self.run_main(FakeConn([('a', 'FALSE')])), 75)
        Nauthilus.flush_status = 302
        self.assertEqual(self.run_main(FakeConn([('a', 'FALSE')])), 75)
        self.assertFalse(any(r[1] == '/redirect-must-not-run' for r in Nauthilus.requests))
        Nauthilus.token_status = 401
        self.assertEqual(self.run_main(FakeConn([('a', 'FALSE')])), 75)

    def test_client_secret_post_and_no_flush(self):
        self.assertEqual(self.run_main(FakeConn([('a', 'FALSE')]), FLAG_OIDC_AUTH_METHOD='client_secret_post'), 0)
        self.assertIsNone(Nauthilus.requests[0][2])
        self.assertIn(b'client_secret=synthetic-secret', Nauthilus.requests[0][3])
        Nauthilus.requests = []
        self.assertEqual(self.run_main(FakeConn([('a', 'FALSE')]), FLAG_FLUSH_URL=''), 0)
        self.assertEqual(Nauthilus.requests, [])

    def test_ldap_error_mapping(self):
        errors = ldap3.core.exceptions
        for exc, code in [(errors.LDAPBusyResult(), 75), (errors.LDAPUnavailableResult(), 75),
                          (errors.LDAPInvalidCredentialsResult(), 1), (errors.LDAPSizeLimitExceededResult(), 1)]:
            self.assertEqual(self.run_main(FakeConn([], fail=exc)), code, type(exc).__name__)

    def test_configuration_errors(self):
        base = dict(self.env, FLAG_LDAP_URI='ldaps://ldap.example.org', FLAG_LDAP_CA_FILE='/ca',
                    FLAG_LDAP_CERT_FILE='/c', FLAG_LDAP_KEY_FILE='/k')
        # Refused before any connection is attempted.
        for extra in ({'FLAG_LDAP_AUTH': 'kerberos'}, {'FLAG_LDAP_CERT_FILE': ''}, {'FLAG_LDAP_AUTH': 'simple'},
                      {'FLAG_LDAP_TIMEOUT': 'soon'}, {'FLAG_DEADLINE': '0'}):
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(quota_flag.main(dict(base, **extra)), 1, extra)
        self.assertEqual(self.run_main(FakeConn([('a', 'FALSE')]), FLAG_HTTP_TIMEOUT='soon'), 1)

    def test_deadline(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = quota_flag.main(dict(self.env, FLAG_DEADLINE='1'), connect=lambda env: FakeConn([('a', 'FALSE')], delay=3))
        self.assertEqual(code, 75)
        self.assertIn('deadline exceeded', out.getvalue())

    def test_connect_to(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(quota_flag.main(dict(self.env, FLAG_HTTP_CONNECT_TO='no-port'),
                                             connect=lambda env: FakeConn([('a', 'FALSE')])), 1)
        dialed = []

        class Refused(OSError):
            pass

        def fake_dial(address, timeout=None):
            dialed.append(address)
            raise Refused('refused')

        orig = quota_flag.socket.create_connection
        quota_flag.socket.create_connection = fake_dial
        try:
            env = dict(self.env, FLAG_OIDC_TOKEN_URL='https://login.example.org/oidc/token',
                       FLAG_HTTP_CONNECT_TO='nauthilus-service.auth.svc.cluster.local:8443')
            self.assertEqual(self.run_main(FakeConn([('a', 'FALSE')]), **{k: v for k, v in env.items() if k.startswith('FLAG_')}), 75)
        finally:
            quota_flag.socket.create_connection = orig
        self.assertEqual(dialed, [('nauthilus-service.auth.svc.cluster.local', 8443)])

    def test_refuses_plain_ldap(self):
        with contextlib.redirect_stdout(io.StringIO()):
            code = quota_flag.main(dict(self.env, FLAG_LDAP_URI='ldap://ldap.example.org', FLAG_LDAP_CA_FILE='/ca',
                                        FLAG_LDAP_AUTH='simple', FLAG_LDAP_BIND_DN='cn=x', FLAG_LDAP_BIND_PASSWORD_FILE='/pw'))
        self.assertEqual(code, 1)


if __name__ == '__main__':
    unittest.main()
