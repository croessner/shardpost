"""Local HTTP contract tests: no real classifier or credentials."""
import contextlib
import http.server
import io
import os
import threading
import unittest
from unittest.mock import patch
import learn


class HelperTest(unittest.TestCase):
    def test_spamc_exit_contract(self):
        import subprocess
        for code, expected in [(0, 0), (5, 0), (6, 0), (69, 75), (74, 75), (75, 75), (77, 1)]:
            with patch.object(learn.subprocess, 'run', return_value=subprocess.CompletedProcess([], code)) as run:
                self.assertEqual(learn.spamc('ham', b'message'), expected)
                self.assertEqual(run.call_args.args[0], ['spamc', '-x', '-L', 'ham'])

    def test_http_contract(self):
        requests = []
        state = [200, b'{"success":true}']

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                requests.append((self.path, self.headers.get('Password'), self.rfile.read(int(self.headers['Content-Length']))))
                self.send_response(state[0])
                if state[0] == 302:
                    self.send_header('Location', '/redirect-must-not-run')
                self.end_headers()
                self.wfile.write(state[1])

            def log_message(self, *args):
                pass

        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with patch.dict(os.environ, {'LEARN_URL': 'http://127.0.0.1:%d' % server.server_port, 'LEARN_TOKEN': 'synthetic-token'}, clear=True), contextlib.redirect_stderr(io.StringIO()):
                for status, body, expected in [(200, b'{"success":true}', 0), (204, b'', 0), (208, b'', 0), (503, b'', 75), (429, b'', 75), (403, b'', 1), (302, b'', 1), (200, b'[]', 75), (200, b'null', 75), (200, b'', 75), (200, b'no json', 75), (200, b'{"error":"already learned"}', 0), (200, b'{"error":"already disconnected"}', 1), (200, b'x'*8193, 75)]:
                    state[:] = [status, body]
                    before = len(requests)
                    self.assertEqual(learn.rspamd('spam', b'synthetic message'), expected, (status, body[:40]))
                    self.assertEqual(len(requests), before + 1)
                    self.assertEqual(requests[-1], ('/learnspam', 'synthetic-token', b'synthetic message'))
                state[:] = [200, b'{"success":true}']
                self.assertEqual(learn.rspamd('ham', b'message'), 0)
                self.assertEqual(requests[-1][0], '/learnham')
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == '__main__':
    unittest.main()
