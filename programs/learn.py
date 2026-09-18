#!/usr/bin/env python3
"""Learning helper: hands the message on stdin to a classifier.

The IMAP server knows nothing about the classifier; it runs this program
from its allowlist with the message on stdin and reads the exit code:

  0   learned
  75  temporary problem, the server retries later (EX_TEMPFAIL)
  1   rejected for good

Backends (LEARN_BACKEND, default rspamd):
  rspamd  POST <LEARN_URL>/learnspam or /learnham, token from LEARN_TOKEN_FILE
          (or LEARN_TOKEN), Password header, JSON {"success": true} expected.
  spamc   pipes the message to `spamc -L spam|ham` (SpamAssassin).
  log     writes a line to stderr only (for tests).

Usage: learn.py --action spam|ham
Only the Python standard library is used.
"""
import argparse
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

EX_TEMPFAIL = 75


class NoRedirect(urllib.request.HTTPRedirectHandler):
    # Never forward the classifier token to a redirect target.
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def rspamd(action, message):
    url = os.environ.get("LEARN_URL", "").rstrip("/")
    if not url:
        print("learn.py: LEARN_URL not set", file=sys.stderr)
        return 1
    token = os.environ.get("LEARN_TOKEN", "")
    token_file = os.environ.get("LEARN_TOKEN_FILE")
    if token_file:
        try:
            with open(token_file, encoding="utf-8") as fh:
                token = fh.read().strip()
        except OSError as exc:
            print("learn.py: cannot read token file: %s" % exc.__class__.__name__, file=sys.stderr)
            return 1
    req = urllib.request.Request(url + "/learn" + action, data=message, method="POST")
    req.add_header("Content-Type", "message/rfc822")
    if token:
        req.add_header("Password", token)
    timeout = float(os.environ.get("LEARN_TIMEOUT", "20"))
    try:
        with HTTP.open(req, timeout=timeout) as resp:
            # Successful no-content replies require no JSON decoding.
            if resp.status in (204, 208):
                return 0
            body = resp.read(8193)
            if len(body) > 8192:
                return EX_TEMPFAIL
    except urllib.error.HTTPError as exc:
        # 4xx: the classifier refused (already learned counts as done)
        if 500 <= exc.code < 600 or exc.code in (408, 429):
            print("learn.py: classifier status %d" % exc.code, file=sys.stderr)
            return EX_TEMPFAIL
        print("learn.py: classifier rejected with status %d" % exc.code, file=sys.stderr)
        return 1
    except (urllib.error.URLError, OSError, TimeoutError) as exc:
        print("learn.py: classifier unreachable: %s" % exc.__class__.__name__, file=sys.stderr)
        return EX_TEMPFAIL
    try:
        reply = json.loads(body.decode("utf-8", "replace"))
    except ValueError:
        print("learn.py: invalid reply", file=sys.stderr)
        return EX_TEMPFAIL
    if not isinstance(reply, dict):
        return EX_TEMPFAIL
    if reply.get("success") is True and not reply.get("error"):
        return 0
    if "already learned" in str(reply.get("error", "")).lower():
        return 0
    print("learn.py: classifier did not learn", file=sys.stderr)
    return 1


def spamc(action, message):
    try:
        proc = subprocess.run(["spamc", "-x", "-L", action], input=message, capture_output=True, timeout=60, check=False)
    except FileNotFoundError:
        print("learn.py: spamc not installed", file=sys.stderr)
        return 1
    except subprocess.TimeoutExpired:
        return EX_TEMPFAIL
    # spamc -L: 5 = learned, 6 = already learned, 74 = I/O error
    if proc.returncode in (0, 5, 6):
        return 0
    if proc.returncode in (68, 69, 71, 74, 75):
        return EX_TEMPFAIL
    return 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--action", choices=["spam", "ham"], required=True)
    args = parser.parse_args()
    message = sys.stdin.buffer.read(64 * 1024 * 1024 + 1)
    if len(message) > 64 * 1024 * 1024:
        return 1
    if not message:
        print("learn.py: empty message", file=sys.stderr)
        return 1
    backend = os.environ.get("LEARN_BACKEND", "rspamd")
    if backend == "rspamd":
        return rspamd(args.action, message)
    if backend == "spamc":
        return spamc(args.action, message)
    if backend == "log":
        print("learn.py: %s %d bytes for %s" % (args.action, len(message), os.environ.get("SIEVE_ACCOUNT", "?")), file=sys.stderr)
        return 0
    print("learn.py: unknown LEARN_BACKEND", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
