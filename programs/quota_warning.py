#!/usr/bin/env python3
"""Quota warning: delivers a warning mail into the account's mailbox.

The server runs this program from its allowlist (extprograms kind
"quota") when a change of the usage crosses a configured threshold. The
event comes in the QUOTA_* environment; the mail is rendered from a
template and delivered locally with shardpostctl (admin API, by default
without quota enforcement, like dovecot-lda noenforcing), so it reaches a
full mailbox as well.

Environment of the event (set by the server):
  QUOTA_ACCOUNT, QUOTA_WARNING, QUOTA_RESOURCE, QUOTA_PERCENT,
  QUOTA_DIRECTION (over|under), QUOTA_STORAGE_USAGE, QUOTA_STORAGE_LIMIT,
  QUOTA_MESSAGES_USAGE, QUOTA_MESSAGES_LIMIT

Configuration (extprograms entry env):
  WARN_FROM            sender address (required)
  WARN_TEMPLATE_OVER   template for "over" (default: templates/quota-warning-over.txt)
  WARN_TEMPLATE_UNDER  template for "under" (default: templates/quota-warning-under.txt)
  WARN_MSGID_DOMAIN    Message-ID domain (default: domain of WARN_FROM)
  WARN_ENFORCE_QUOTA   "true" delivers with quota enforcement (default false)
  WARN_SIEVE           "false" skips the user's Sieve script (default true)
  WARN_SHARDPOSTCTL    path of shardpostctl (default /usr/local/bin/shardpostctl)
  WARN_ADMIN_SOCKET    admin socket (default: shardpostctl's default)
  WARN_DEADLINE        seconds for the whole run (default 50; keep it
                       below the entry's timeout_ms, so the status line
                       is written before the server kills the program)

A template is "Subject: <line>", an empty line and the body; ${name}
placeholders are the event values in lower case without the QUOTA_
prefix plus storage_usage_human and storage_limit_human.

Exit codes: 0 delivered, 75 temporary failure, 1 permanent failure. One
status line goes to stdout (the server logs it on failure).
Only the Python standard library is used.
"""
import email.policy
import email.utils
import os
import re
import signal
import string
import subprocess
import sys
from email.message import EmailMessage

EX_TEMPFAIL = 75
HERE = os.path.dirname(os.path.abspath(__file__))
ACCOUNT = re.compile(r'^[A-Za-z0-9@._%+-]{1,320}$')
FIELDS = ('account', 'warning', 'resource', 'percent', 'direction', 'storage_usage',
          'storage_limit', 'messages_usage', 'messages_limit')


class Failure(Exception):
    """A failure with its exit code."""

    def __init__(self, message, code=1):
        super().__init__(message)
        self.code = code


def human(value):
    """Formats a byte count with a binary unit."""
    try:
        n = float(value)
    except (TypeError, ValueError):
        return str(value)
    for unit in ('B', 'KiB', 'MiB', 'GiB'):
        if n < 1024:
            return ('%d %s' if unit == 'B' else '%.1f %s') % (n, unit)
        n /= 1024
    return '%.1f TiB' % n


def event(env):
    """Returns the event values from the environment."""
    values = {k: env.get('QUOTA_' + k.upper(), '') for k in FIELDS}
    if not ACCOUNT.match(values['account']):
        raise Failure('invalid or missing QUOTA_ACCOUNT')
    if values['direction'] not in ('over', 'under'):
        raise Failure('invalid QUOTA_DIRECTION')
    values['storage_usage_human'] = human(values['storage_usage'])
    values['storage_limit_human'] = human(values['storage_limit'])
    return values


def render(path, values):
    """Returns subject and body of the template at path."""
    try:
        with open(path, encoding='utf-8') as f:
            text = string.Template(f.read()).safe_substitute(values)
    except OSError as e:
        raise Failure('template %s: %s' % (path, e.strerror))
    head, sep, body = text.partition('\n\n')
    if not sep or not head.startswith('Subject:') or '\n' in head:
        raise Failure('template %s: expected "Subject: ...", an empty line and the body' % path)
    return head[len('Subject:'):].strip(), body


def compose(env, values):
    """Builds the warning message (bytes, CRLF line endings)."""
    sender = env.get('WARN_FROM', '')
    if not ACCOUNT.match(sender) or '@' not in sender:
        raise Failure('WARN_FROM must be a sender address')
    default = os.path.join(HERE, 'templates', 'quota-warning-%s.txt' % values['direction'])
    subject, body = render(env.get('WARN_TEMPLATE_' + values['direction'].upper(), default), values)
    domain = env.get('WARN_MSGID_DOMAIN') or sender.rpartition('@')[2]

    msg = EmailMessage(policy=email.policy.SMTP)
    msg['From'] = sender
    msg['To'] = values['account']
    msg['Subject'] = subject
    msg['Date'] = email.utils.formatdate(localtime=True)
    msg['Message-ID'] = email.utils.make_msgid('quota', domain=domain)
    # Automatic mail: no vacation replies, no redirects (RFC 3834).
    msg['Auto-Submitted'] = 'auto-generated'
    msg.set_content(body, charset='utf-8', cte='quoted-printable')
    return msg.as_bytes()


def deliver(env, account, message, timeout):
    """Delivers message with shardpostctl; returns the status line."""
    cmd = [env.get('WARN_SHARDPOSTCTL', '/usr/local/bin/shardpostctl')]
    if env.get('WARN_ADMIN_SOCKET'):
        cmd += ['-socket', env['WARN_ADMIN_SOCKET']]
    cmd += ['deliver', '-d', account, '-f', env['WARN_FROM']]
    if env.get('WARN_ENFORCE_QUOTA', 'false').lower() != 'true':
        cmd.append('--no-enforce-quota')
    if env.get('WARN_SIEVE', 'true').lower() == 'false':
        cmd.append('--no-sieve')
    try:
        res = subprocess.run(cmd, input=message, capture_output=True, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired) as e:
        raise Failure('shardpostctl: %s' % e, EX_TEMPFAIL)
    out = (res.stdout or res.stderr).decode('utf-8', 'replace').strip().splitlines()
    line = out[0] if out else 'exit %d' % res.returncode
    if res.returncode == 0:
        return line
    raise Failure(line, EX_TEMPFAIL if res.returncode == EX_TEMPFAIL else 1)


def seconds(env, name, default):
    """Returns a positive number of seconds from env[name]."""
    try:
        value = int(env.get(name, default))
    except ValueError:
        raise Failure('%s must be a number of seconds' % name)
    if value <= 0:
        raise Failure('%s must be positive' % name)
    return value


def deadline(signum, frame):
    """SIGALRM: the run took longer than WARN_DEADLINE."""
    raise Failure('deadline exceeded', EX_TEMPFAIL)


def main(env=None):
    env = os.environ if env is None else env
    try:
        budget = seconds(env, 'WARN_DEADLINE', 50)
        signal.signal(signal.SIGALRM, deadline)
        signal.alarm(budget)
        values = event(env)
        # shardpostctl gets a little less, so its own error wins.
        status = deliver(env, values['account'], compose(env, values), max(budget - 5, 1))
    except Failure as e:
        print('quota-warning: %s' % e)
        return e.code
    finally:
        signal.alarm(0)
    print('quota-warning: %s' % status)
    return 0


if __name__ == '__main__':
    sys.exit(main())
