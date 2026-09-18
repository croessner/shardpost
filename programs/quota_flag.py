#!/usr/local/lib/shardpost/venv/bin/python3
"""Over-quota flag: writes the real state into the userdb and flushes the
authentication cache for the account.

The server runs this program from its allowlist (extprograms kind
"quota") when the over-quota flag the userdb returned differs from the
real state. It sets the LDAP attribute to the target value (never a
toggle: running twice is harmless) and then asks Nauthilus to drop its
cached result for the account, so logins and recipient lookups see the
new flag at once.

Environment of the event (set by the server):
  QUOTA_ACCOUNT, QUOTA_OVER (true|false)

LDAP configuration (extprograms entry env):
  FLAG_LDAP_URI           e.g. ldaps://ldap.roessner-net.de:636 (required)
  FLAG_LDAP_BASE          search base (required)
  FLAG_LDAP_FILTER        filter, {account} is replaced (RFC 4515 escaped) (required)
  FLAG_LDAP_ATTRIBUTE     attribute (default rnsMSOverQuota)
  FLAG_LDAP_VALUE_OVER    value for "over" (default TRUE)
  FLAG_LDAP_VALUE_UNDER   value for "not over" (default FALSE)
  FLAG_LDAP_CA_FILE       CA bundle for the server certificate (required for ldaps/StartTLS)
  FLAG_LDAP_STARTTLS      "true" for StartTLS on an ldap:// URI
  FLAG_LDAP_AUTH          external (SASL EXTERNAL, client certificate) or simple
  FLAG_LDAP_CERT_FILE, FLAG_LDAP_KEY_FILE          for external
  FLAG_LDAP_BIND_DN, FLAG_LDAP_BIND_PASSWORD_FILE  for simple
  FLAG_LDAP_TIMEOUT       seconds per LDAP operation (default 5)

Cache flush (optional; FLAG_FLUSH_URL empty disables it):
  FLAG_FLUSH_URL               e.g. https://login.authserv.me/api/v1/cache/flush
  FLAG_OIDC_TOKEN_URL          token endpoint (client_credentials)
  FLAG_OIDC_CLIENT_ID
  FLAG_OIDC_CLIENT_SECRET_FILE
  FLAG_OIDC_SCOPE              default "nauthilus:authenticate nauthilus:security"
                               (authenticate is the base scope of every
                               backchannel call, security the flush)
  FLAG_OIDC_AUTH_METHOD        client_secret_basic (default) or client_secret_post
  FLAG_HTTP_TIMEOUT            seconds per HTTP request (default 5)
  FLAG_HTTP_CA_FILE            CA bundle for HTTPS (default: system trust)
  FLAG_HTTP_CONNECT_TO         host:port to connect to instead of the URL's
                               host, like curl --connect-to; TLS still
                               verifies the URL's host name (e.g. the
                               in-cluster service behind the public name)
The token is fetched for every run and not cached.

FLAG_DEADLINE (seconds, default 20) bounds the whole run: when it passes,
the program stops with 75 and its status line, before the server's
timeout_ms kills it silently. Keep it below timeout_ms.

Exit codes: 0 done, 75 temporary failure (LDAP or Nauthilus unreachable),
1 permanent failure (configuration, no or several entries). One status
line goes to stdout; secrets are never printed.
"""
import base64
import http.client
import json
import socket
import os
import re
import signal
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request

EX_TEMPFAIL = 75
ACCOUNT = re.compile(r'^[A-Za-z0-9@._%+-]{1,320}$')


class Failure(Exception):
    """A failure with its exit code."""

    def __init__(self, message, code=1):
        super().__init__(message)
        self.code = code


class NoRedirect(urllib.request.HTTPRedirectHandler):
    # Never forward a bearer token or client secret to a redirect target.
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def setting(env, name, default=None):
    """Returns a setting; a missing required one is a failure."""
    value = env.get(name, default)
    if value is None or value == '':
        raise Failure('%s is not set' % name)
    return value


def seconds(env, name, default):
    """Returns a positive number of seconds from env[name]."""
    try:
        value = int(env.get(name, default))
    except ValueError:
        raise Failure('%s must be a number of seconds' % name)
    if value <= 0:
        raise Failure('%s must be positive' % name)
    return value


def read_secret(env, name):
    """Reads the first line of the file named by env[name]."""
    path = setting(env, name)
    try:
        with open(path, encoding='utf-8') as f:
            value = f.readline().strip()
    except OSError as e:
        raise Failure('%s: %s' % (name, e.strerror))
    if not value:
        raise Failure('%s is empty' % name)
    return value


# ---------------------------------------------------------------------------
# LDAP


# ldap3 comes from the image's venv; it is imported where it is used so
# the cache flush and the tests of this module work without it.


def ldap_connection(env):
    """Opens and binds the LDAP connection."""
    import ldap3

    uri = setting(env, 'FLAG_LDAP_URI')
    timeout = seconds(env, 'FLAG_LDAP_TIMEOUT', 5)
    auth = env.get('FLAG_LDAP_AUTH', 'external')
    starttls = env.get('FLAG_LDAP_STARTTLS', 'false').lower() == 'true'

    tls_args = {'validate': ssl.CERT_REQUIRED, 'version': ssl.PROTOCOL_TLS_CLIENT,
                'ca_certs_file': setting(env, 'FLAG_LDAP_CA_FILE')}
    if auth == 'external':
        tls_args['local_certificate_file'] = setting(env, 'FLAG_LDAP_CERT_FILE')
        tls_args['local_private_key_file'] = setting(env, 'FLAG_LDAP_KEY_FILE')
    elif auth != 'simple':
        raise Failure('FLAG_LDAP_AUTH must be external or simple')

    if not uri.startswith('ldaps://') and not starttls:
        raise Failure('refusing an unencrypted LDAP connection (use ldaps:// or FLAG_LDAP_STARTTLS)')

    # ldap3 checks the host name itself after the handshake (CERT_REQUIRED).
    tls_args['sni'] = urllib.parse.urlsplit(uri).hostname
    try:
        server = ldap3.Server(uri, tls=ldap3.Tls(**tls_args), connect_timeout=timeout, get_info=ldap3.NONE)
        if auth == 'external':
            # An empty authorization identity: the certificate's identity
            # is used (without it OpenLDAP answers "mechanism not available").
            conn = ldap3.Connection(server, authentication=ldap3.SASL, sasl_mechanism=ldap3.EXTERNAL,
                                    sasl_credentials='', receive_timeout=timeout, raise_exceptions=True)
        else:
            conn = ldap3.Connection(server, user=setting(env, 'FLAG_LDAP_BIND_DN'),
                                    password=read_secret(env, 'FLAG_LDAP_BIND_PASSWORD_FILE'),
                                    authentication=ldap3.SIMPLE, receive_timeout=timeout, raise_exceptions=True)
    except ldap3.core.exceptions.LDAPException as e:
        raise Failure('LDAP configuration: %s: %s' % (type(e).__name__, e))
    try:
        conn.open()
        if starttls:
            conn.start_tls()
        bound = conn.bind()
    except ldap3.core.exceptions.LDAPException as e:
        raise Failure('LDAP bind: %s' % describe(e), temporary(e))
    # Some results (a SASL bind still "in progress") do not raise.
    if not bound:
        conn.unbind()
        raise Failure('LDAP bind: %s' % conn.result.get('description', 'failed'))
    return conn


def describe(exc):
    """Names an ldap3 exception; transport errors carry host and errno
    (no secrets) and are worth their text."""
    import ldap3

    if isinstance(exc, ldap3.core.exceptions.LDAPCommunicationError):
        return '%s: %s' % (type(exc).__name__, exc)
    return type(exc).__name__


def temporary(exc):
    """Maps an ldap3 exception to an exit code."""
    import ldap3

    temp = (ldap3.core.exceptions.LDAPCommunicationError, ldap3.core.exceptions.LDAPBusyResult,
            ldap3.core.exceptions.LDAPUnavailableResult, ldap3.core.exceptions.LDAPTimeLimitExceededResult)
    return EX_TEMPFAIL if isinstance(exc, temp) else 1


def set_flag(env, conn, account, over):
    """Sets the flag attribute of the account's entry; returns what it did."""
    import ldap3
    from ldap3.utils.conv import escape_filter_chars

    attr = env.get('FLAG_LDAP_ATTRIBUTE', 'rnsMSOverQuota')
    target = env.get('FLAG_LDAP_VALUE_OVER', 'TRUE') if over else env.get('FLAG_LDAP_VALUE_UNDER', 'FALSE')
    search = setting(env, 'FLAG_LDAP_FILTER').replace('{account}', escape_filter_chars(account))

    try:
        conn.search(setting(env, 'FLAG_LDAP_BASE'), search, search_scope=ldap3.SUBTREE,
                    attributes=[attr], size_limit=2)
    except ldap3.core.exceptions.LDAPSizeLimitExceededResult:
        raise Failure('several LDAP entries for the account')
    except ldap3.core.exceptions.LDAPException as e:
        raise Failure('LDAP: %s' % describe(e), temporary(e))

    entries = [e for e in conn.response if e.get('type') == 'searchResEntry']
    if len(entries) != 1:
        raise Failure('%d LDAP entries for the account' % len(entries))
    current = entries[0]['attributes'].get(attr) or []
    if isinstance(current, str):
        current = [current]
    if [str(v) for v in current] == [target]:
        return 'already %s' % target

    try:
        conn.modify(entries[0]['dn'], {attr: [(ldap3.MODIFY_REPLACE, [target])]})
    except ldap3.core.exceptions.LDAPException as e:
        raise Failure('LDAP: %s' % describe(e), temporary(e))
    return 'set %s' % target


# ---------------------------------------------------------------------------
# Nauthilus cache flush


def connect_to(target):
    """An HTTPS connection class that dials target (host:port) but keeps
    the request's host for SNI, the Host header and certificate checks."""
    host, _, port = target.rpartition(':')
    if not host or not port.isdigit():
        raise Failure('FLAG_HTTP_CONNECT_TO must be host:port')

    class Connection(http.client.HTTPSConnection):
        def connect(self):
            sock = socket.create_connection((host, int(port)), self.timeout)
            self.sock = self._context.wrap_socket(sock, server_hostname=self.host)

    return Connection


def opener(env):
    """An opener without proxies and redirects."""
    context = ssl.create_default_context(cafile=env.get('FLAG_HTTP_CA_FILE') or None)
    https = urllib.request.HTTPSHandler(context=context)
    if env.get('FLAG_HTTP_CONNECT_TO'):
        conn_class = connect_to(env['FLAG_HTTP_CONNECT_TO'])
        https.https_open = lambda req: https.do_open(conn_class, req, context=context)
    return urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect(), https)


def access_token(env, http_opener, timeout):
    """Fetches a client_credentials token (one per run, not cached)."""
    client_id = setting(env, 'FLAG_OIDC_CLIENT_ID')
    secret = read_secret(env, 'FLAG_OIDC_CLIENT_SECRET_FILE')
    form = {'grant_type': 'client_credentials',
            'scope': env.get('FLAG_OIDC_SCOPE', 'nauthilus:authenticate nauthilus:security')}
    headers = {'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'application/json'}

    method = env.get('FLAG_OIDC_AUTH_METHOD', 'client_secret_basic')
    if method == 'client_secret_basic':
        pair = '%s:%s' % (urllib.parse.quote(client_id, safe=''), urllib.parse.quote(secret, safe=''))
        headers['Authorization'] = 'Basic ' + base64.b64encode(pair.encode()).decode()
    elif method == 'client_secret_post':
        form.update(client_id=client_id, client_secret=secret)
    else:
        raise Failure('FLAG_OIDC_AUTH_METHOD must be client_secret_basic or client_secret_post')

    req = urllib.request.Request(setting(env, 'FLAG_OIDC_TOKEN_URL'), data=urllib.parse.urlencode(form).encode(),
                                 headers=headers, method='POST')
    body = call(http_opener, req, timeout, 'token')
    try:
        token = json.loads(body)['access_token']
    except (ValueError, KeyError, TypeError):
        raise Failure('token response without access_token', EX_TEMPFAIL)
    return token


def call(http_opener, req, timeout, what):
    """Performs req; non-2xx and transport errors are temporary failures."""
    try:
        with http_opener.open(req, timeout=timeout) as resp:
            return resp.read(1 << 20)
    except urllib.error.HTTPError as e:
        e.close()
        raise Failure('%s: HTTP %d' % (what, e.code), EX_TEMPFAIL)
    except (urllib.error.URLError, OSError) as e:
        raise Failure('%s: %s' % (what, getattr(e, 'reason', e)), EX_TEMPFAIL)


def flush(env, account):
    """Drops the account from the Nauthilus cache; returns what it did."""
    url = env.get('FLAG_FLUSH_URL', '')
    if not url:
        return 'no flush configured'
    timeout = seconds(env, 'FLAG_HTTP_TIMEOUT', 5)
    http_opener = opener(env)
    token = access_token(env, http_opener, timeout)
    req = urllib.request.Request(url, data=json.dumps({'user': account}).encode(), method='DELETE',
                                 headers={'Content-Type': 'application/json', 'Accept': 'application/json',
                                          'Authorization': 'Bearer ' + token})
    call(http_opener, req, timeout, 'cache flush')
    return 'cache flushed'


def deadline(signum, frame):
    """SIGALRM: the run took longer than FLAG_DEADLINE. Inside ldap3's
    connect or TLS setup the failure is wrapped (LDAPSocketOpenError ...
    deadline exceeded), still with exit 75."""
    raise Failure('deadline exceeded', EX_TEMPFAIL)


def main(env=None, connect=ldap_connection):
    env = os.environ if env is None else env
    try:
        signal.signal(signal.SIGALRM, deadline)
        signal.alarm(seconds(env, 'FLAG_DEADLINE', 20))
        account = env.get('QUOTA_ACCOUNT', '')
        if not ACCOUNT.match(account):
            raise Failure('invalid or missing QUOTA_ACCOUNT')
        over = env.get('QUOTA_OVER', '').lower()
        if over not in ('true', 'false'):
            raise Failure('invalid QUOTA_OVER')
        conn = connect(env)
        try:
            done = set_flag(env, conn, account, over == 'true')
        finally:
            conn.unbind()
        done += ', ' + flush(env, account)
    except Failure as e:
        print('quota-flag: %s' % e)
        return e.code
    finally:
        signal.alarm(0)
    print('quota-flag: %s: %s' % (account, done))
    return 0


if __name__ == '__main__':
    sys.exit(main())
