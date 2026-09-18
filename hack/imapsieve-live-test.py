#!/usr/bin/env python3
"""Native acceptance using one dedicated test account; no SMTP messages are sent.

IMAP_HOST, IMAP_USER and IMAP_PASSWORD are required. IMAP_TLS_NAME optionally
sets the certificate hostname when connecting through an internal service.
The run creates only uniquely named test mailboxes and one unactivated script.
"""
import base64
import imaplib
import json
import os
import socket
import ssl
import uuid
import time
from pathlib import Path

host = os.environ['IMAP_HOST']
user = os.environ['IMAP_USER']
password = os.environ['IMAP_PASSWORD']
tls_name = os.environ.get('IMAP_TLS_NAME', host)
context = ssl.create_default_context()
imaplib.Commands.setdefault('SETMETADATA', ('AUTH', 'SELECTED'))
imaplib.Commands.setdefault('MOVE', ('SELECTED',))
prefix = 'IMAPSieve-check-' + uuid.uuid4().hex[:12]
script = prefix
prefill = int(os.environ.get('IMAP_TEST_SHARD_BOUNDARY', '0'))
if not 0 <= prefill <= 100: raise ValueError('invalid prefill count')
boxes = [prefix + '/Source', prefix + '/Junk', prefix + '/Restored']

class Client(imaplib.IMAP4_SSL):
    def _create_socket(self, timeout):
        raw = socket.create_connection((self.host, self.port), timeout)
        return context.wrap_socket(raw, server_hostname=tls_name)

def ok(result):
    if result[0] != 'OK':
        raise RuntimeError('IMAP command rejected')
    return result[1]

sock = socket.create_connection((host, int(os.environ.get('SIEVE_PORT', '4190'))), 10)
stream = sock.makefile('rb')
def reply():
    while True:
        line = stream.readline()
        if not line:
            raise RuntimeError('ManageSieve connection closed')
        if line.startswith(b'OK'):
            return
        if line.startswith((b'NO', b'BYE')):
            raise RuntimeError('ManageSieve command rejected')
def command(data):
    sock.sendall(data + b'\r\n')
    reply()
reply()
command(b'STARTTLS')
stream.close()
sock = context.wrap_socket(sock, server_hostname=tls_name)
stream = sock.makefile('rb')
reply()
command(b'AUTHENTICATE "PLAIN" "' + base64.b64encode(b'\0' + user.encode() + b'\0' + password.encode()) + b'"')
program = b'require ["imapsieve", "environment", "imap4flags"]; if environment :is "imap.cause" "APPEND" { addflag "event-append"; } if environment :is "imap.cause" "FLAG" { addflag "event-flag"; }'
command(b'PUTSCRIPT "' + script.encode() + b'" {' + str(len(program)).encode() + b'+}\r\n' + program)
c = Client(host, int(os.environ.get('IMAP_PORT', '993')), timeout=15)
created = []
try:
    ok(c.login(user, password))
    caps = b' '.join(ok(c.capability())).upper().split()
    if not any(v.startswith(b'IMAPSIEVE=') for v in caps):
        raise RuntimeError('IMAPSIEVE capability missing')
    for box in boxes:
        args = '(USE (\\Junk))' if box == boxes[1] else None
        ok(c._simple_command('CREATE', box, *([args] if args else [])))
        created.append(box)
    ok(c._simple_command('SETMETADATA', boxes[0], '("/shared/imapsieve/script" "' + script + '")'))
    body = ('From: test@example.org\r\nSubject: Native IMAPSieve acceptance ' + prefix + '\r\n\r\nThis is an isolated protocol acceptance message. No SMTP delivery takes place.\r\n').encode()
    for _ in range(prefill):
        ok(c.append(boxes[1], None, None, b'Subject: shard boundary filler\r\n\r\nNo training on APPEND.'))
    ok(c.append(boxes[0], None, None, body))
    ok(c.select(boxes[0]))
    result = ok(c.fetch('1', '(FLAGS)'))
    if b'event-append' not in b' '.join(x for x in result if isinstance(x, bytes)).lower():
        raise RuntimeError('APPEND script did not update flags')
    ok(c.fetch('1', '(BODY[])'))
    result = ok(c.fetch('1', '(FLAGS)'))
    if b'event-flag' not in b' '.join(x for x in result if isinstance(x, bytes)).lower():
        raise RuntimeError('FETCH Seen did not trigger FLAG')
    ok(c._simple_command('MOVE', '1', boxes[1]))
    ok(c.select(boxes[1]))
    ok(c._simple_command('MOVE', str(prefill + 1), boxes[2]))
    ok(c.select(boxes[2]))
    if ok(c.search(None, 'ALL')) != [b'1']:
        raise RuntimeError('MOVE did not preserve message')
    if os.environ.get('IMAP_SIEVE_LIMITS_TEST') == '1':
        cpu = (Path(__file__).resolve().parents[1] / 'third_party/go-sieve/pigeonhole/tests/execute/errors/cpu-limit.sieve').read_bytes()
        mime_code = b'require ["foreverypart"]; foreverypart { discard; stop; }'
        multipart = b'Content-Type: multipart/mixed; boundary=x\r\n\r\n' + b'--x\r\nContent-Type: text/plain\r\n\r\nx\r\n' * 1001 + b'--x--\r\n'
        for label, code, message in [('cpu', cpu, body), ('mime', mime_code, multipart)]:
            command(b'PUTSCRIPT "' + script.encode() + b'" {' + str(len(code)).encode() + b'+}\r\n' + code)
            ok(c.select(boxes[0]))
            started = time.monotonic()
            ok(c.append(boxes[0], None, None, message))
            elapsed = time.monotonic() - started
            uid = c.response('APPENDUID')[1][0].split()[-1]
            fetched = ok(c.uid('FETCH', uid, '(FLAGS BODY.PEEK[])'))
            flags = b' '.join(x[0] if isinstance(x, tuple) else x for x in fetched if isinstance(x, (bytes, tuple))).lower()
            contents = [x[1] for x in fetched if isinstance(x, tuple)]
            if elapsed > 3 or b'\\deleted' in flags or contents != [message]:
                raise RuntimeError('Sieve resource-limit acceptance failed: ' + label)
            print(json.dumps({'resource_limit': label, 'seconds': round(elapsed, 3), 'original_preserved': True}))
        command(b'PUTSCRIPT "' + script.encode() + b'" {' + str(len(program)).encode() + b'+}\r\n' + program)
        ok(c.append(boxes[0], None, None, body))
        uid = c.response('APPENDUID')[1][0].split()[-1]
        flags = b' '.join(x for x in ok(c.uid('FETCH', uid, '(FLAGS)')) if isinstance(x, bytes)).lower()
        if b'event-append' not in flags:
            raise RuntimeError('Sieve worker did not recover after bounded failure')
        print(json.dumps({'resource_limit_recovery': 'pass'}))
    if os.environ.get('IMAP_EXTPROGRAMS_TEST') == '1':
        cases = [
            ('pipe-failure', b'require "vnd.dovecot.pipe"; pipe "acceptance-fail";', False, None),
            ('pipe-try-failure', b'require "vnd.dovecot.pipe"; pipe :try "acceptance-fail";', False, None),
            ('execute', b'require ["vnd.dovecot.execute", "imap4flags", "imapsieve", "environment"]; if environment :is "imap.cause" "APPEND" { if execute :pipe "acceptance-ok" { addflag "external-ok"; } }', False, b'external-ok'),
            ('pipe-success', b'require "vnd.dovecot.pipe"; pipe "acceptance-ok";', True, None),
        ]
        for label, code, deleted, flag in cases:
            command(b'PUTSCRIPT "' + script.encode() + b'" {' + str(len(code)).encode() + b'+}\r\n' + code)
            ok(c.select(boxes[0]))
            ok(c.append(boxes[0], None, None, body))
            uid = c.response('APPENDUID')[1][0].split()[-1]
            flags = b' '.join(x for x in ok(c.uid('FETCH', uid, '(FLAGS)')) if isinstance(x, bytes)).lower()
            if (b'\\deleted' in flags) != deleted or (flag and flag not in flags):
                raise RuntimeError('extprograms disposition failed: ' + label)
        print(json.dumps({'extprograms': 'pass', 'pipe_failure_preserves_mail': True, 'pipe_try_preserves_mail': True, 'execute': True, 'pipe_success': True}))
    print(json.dumps({'native_imapsieve': 'pass', 'append': True, 'fetch_flag': True, 'move_to_junk_and_back': True, 'rspamd_delivery': 'verify worker metrics separately'}))
finally:
    try:
        c.unselect()
        for box in reversed(created):
            ok(c.delete(box))
        c.logout()
    finally:
        command(b'DELETESCRIPT "' + script.encode() + b'"')
        stream.close()
        sock.close()
