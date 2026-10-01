# Quota: limits, rules, warnings and the over flag

The quota of an account consists of two counters (bytes and messages) in
the account's registry object and of a limit. The limit comes, in this
order, from `SETQUOTA` or from the userdb attribute
(`server.quota_attribute`, default `rnsMSQuota`, a value with an optional
K/M/G/T suffix), or from `server.quota_bytes`/`server.quota_msgs`
(0 = unlimited). The attribute sets the storage limit only; the message
limit still follows `SETQUOTA` or `server.quota_msgs`.

IMAP clients may only send `SETQUOTA` when `server.quota_self_service`
is on (default off); otherwise it is answered `NO [NOPERM]`. The server
advertises `QUOTASET` (RFC 9208) exactly when the setting is on.
`STATUS` reports `DELETED` (messages flagged `\Deleted`) and
`DELETED-STORAGE` (their size, in the 1024-octet unit of the `STORAGE`
resource, rounded up): what an `EXPUNGE` would free.

On top of that sits the `quota` section, modeled on Dovecot's
`quota_grace`, `quota_rule`, `quota_warning` and `quota_over_flag`:

```yaml
quota:
  grace: 10%                 # or a size such as 10M; empty = 0
  rules:                     # the first matching rule wins
    - special_use: '\Trash'
      extra_storage: 100M    # or 10%; extra_messages: 100 or 10%
    - special_use: '\Junk'
      ignore: true
    - mailbox: 'Archiv/Extern*'
      ignore: true
  warnings:                  # the first matching warning wins: highest threshold first
    - name: full
      resource: storage      # or messages
      percent: 100
      program: quota-warning
    - name: almost-full
      resource: storage
      percent: 90
      program: quota-warning
    - name: below-again
      resource: storage
      percent: 100
      direction: under       # default: over
      program: quota-warning
  over_flag:
    enabled: true
    attribute: quotaOverFlag
    over_value: 'TRUE'       # default; compared case-insensitively
    program: quota-over-flag
    recheck_seconds: 300
extprograms:
  bin_dir: /usr/local/lib/shardpost/programs
  programs:
    quota-warning:
      command: quota_warning.py      # bundled, see below
      kinds: [quota]
      env: {WARN_FROM: postmaster@example.org}
    quota-over-flag:
      command: quota_flag.py         # bundled, see below
      kinds: [quota]
      timeout_ms: 30000
      env: {FLAG_LDAP_URI: "ldaps://ldap.example.org", ...}
```

`quota.grace`, `quota.over_flag.enabled`, `.attribute`, `.over_value`,
`.program` and `.recheck_seconds` can also be set from the environment
(`QUOTA_GRACE`, `QUOTA_OVER_FLAG_ENABLED`, `QUOTA_OVER_FLAG_ATTRIBUTE`,
`QUOTA_OVER_FLAG_OVER_VALUE`, `QUOTA_OVER_FLAG_PROGRAM`,
`QUOTA_OVER_FLAG_RECHECK_SECONDS`); rules and warnings are YAML only.

## Grace

A store that starts below the storage limit may exceed the limit by the
grace; from the limit onwards it is rejected. This keeps the last large
mail from failing just short of the limit. The default is 0 (the previous
behavior); Dovecot uses 10%.

## Mailbox rules

A rule matches either by `mailbox` (name pattern: `*` matches any number
of characters including `/`, `?` exactly one; `INBOX` is matched
case-insensitively, leading and trailing `/` are stripped) or by
`special_use` (RFC 6154, independent of translated folder names).

* `ignore: true` takes the mailbox out of the accounting: delivery, APPEND
  and COPY into it reserve nothing, EXPUNGE and DELETE release nothing. A
  MOVE from an ignored into a counted mailbox is checked like a COPY and
  can fail with `OVERQUOTA`; a MOVE into an ignored mailbox releases the
  space; this also applies when the scrubber completes an interrupted
  MOVE. A RENAME that moves a mailbox into or out of the accounting (a
  name rule can also shadow a special-use rule) counts its messages
  afterwards, but is not rejected (just like a lowered limit).
* `extra_storage`/`extra_messages` raise the limit for stores into that
  mailbox, either as an absolute size or as a percentage of the limit. The
  typical case is the trash folder: a full account can still free up space
  by copying into it. A MOVE between two different counted mailboxes does
  not change the usage and is never rejected. (A MOVE into the currently
  selected mailbox itself runs as a COPY with EXPUNGE and is therefore
  subject to the limit.) An unlimited limit stays unlimited.

Changing rules or name patterns changes what is counted: the scrubber
(`shardpost scrub`) counts according to the current rules and adjusts the
counters after two matching runs.

## Warnings

A warning starts its program when a change in usage crosses the threshold
(`percent` of the base limit, without the extras): `over` when usage rises
from below the threshold to at least the threshold, `under` when it falls
from at least the threshold to below it. Per change, only the first
matching warning in configuration order runs. The triggers are delivery,
APPEND, COPY, MOVE (when the accounting changes), EXPUNGE, DELETE and
RENAME. A store reports its change only once it has succeeded; a
reservation that a failed store returns reports nothing. If, on the other
hand, an already stored part of a delivery into several folders or of a
MULTIAPPEND is rolled back, that is an EXPUNGE and can trigger an
`under` warning. Limit changes and the scrubber correction trigger nothing.

The counters change by compare-and-swap on the registry object; only the
pod whose write crosses the threshold sees the transition. A warning
therefore runs exactly once cluster-wide per transition, without any
coordination between the pods.

## Over flag

The over flag is a userdb attribute that lets an MTA reject mail for full
accounts early. The server compares the attribute value with the real
state (usage ≥ limit in bytes or messages) at every login (IMAP, POP3,
ManageSieve), at every LMTP delivery before storing (using the attributes
from recipient resolution) and after a successful store (a delivery
rejected because of OVERQUOTA does not change the state). If they differ,
the server starts the program that is meant to correct the attribute
(for example via LDAP). The program also runs when a change in usage
crosses the limit itself, upwards or downwards, for example because an
IMAP client frees up space without a login or a delivery following it
(in that case without a previous attribute value). A missing attribute
counts as "not full".
Control characters are removed from the attribute value, and it is
truncated to 256 bytes. Because the userdb (the Nauthilus cache) can keep
returning the old value for a while, the program runs at most once per
`recheck_seconds` and pod for the same account and the same state; a
state change runs immediately.

## Programs

Both events start entries from `extprograms.programs` of the kind
`quota` (the sudoers principle, as with Sieve, see
[IMAPSieve and external programs](imapsieve.md)): a fixed command line under
`bin_dir`, no shell, a private environment, a timeout, no input on
stdin. `kinds: [quota]` prevents a Sieve script from invoking the program.

Placeholders in `args`:

| Placeholder | Warning | Over flag |
| --- | --- | --- |
| `{account}` | account | account |
| `{warning}`, `{resource}`, `{percent}`, `{direction}` | name, `storage`/`messages`, threshold, `over`/`under` | – |
| `{state}`, `{flag}` | – | `over`/`under`, previous attribute value |
| `{storage_usage}`, `{storage_limit}`, `{messages_usage}`, `{messages_limit}` | after the change | current state |

Environment: `QUOTA_EVENT` (`warning` or `over_flag`), `QUOTA_ACCOUNT`,
`QUOTA_STORAGE_USAGE`, `QUOTA_STORAGE_LIMIT`, `QUOTA_MESSAGES_USAGE`,
`QUOTA_MESSAGES_LIMIT`; for warnings additionally `QUOTA_WARNING`,
`QUOTA_RESOURCE`, `QUOTA_PERCENT`, `QUOTA_DIRECTION`; for the over flag
`QUOTA_OVER` (`true`/`false`) and `QUOTA_FLAG`.

The programs run asynchronously; delivery and login do not wait. At most
64 runs are open at the same time, further ones are dropped and
counted. On shutdown the server waits up to 10 s for running programs.
Metrics: `imap_quota_events_total{event,result}` (`dispatched`,
`dropped`) and `imap_extprograms_runs_total{program,result}`.

## Bundled programs

The image contains two programs under
`/usr/local/lib/shardpost/programs` (entries with `kinds: [quota]`, without
`args`; they read the `QUOTA_*` environment). They are configured through
the `env` of the `extprograms` entry, and secrets through file paths.

**`quota_warning.py`** delivers a warning message into the mailbox, using
the local delivery of the [Admin API](admin-api.md) (`shardpostctl deliver`,
by default without quota enforcement, like `dovecot-lda … noenforcing`,
and with Sieve). The message carries `Auto-Submitted: auto-generated` so
that vacation and redirect do not react to it.

| Variable | Meaning |
| --- | --- |
| `WARN_FROM` | sender (required) |
| `WARN_TEMPLATE_OVER`, `WARN_TEMPLATE_UNDER` | templates; default `templates/quota-warning-{over,under}.txt` (DE/EN) |
| `WARN_MSGID_DOMAIN` | domain of the Message-ID (default: the one from `WARN_FROM`) |
| `WARN_ENFORCE_QUOTA` | `true` delivers with enforcement (default `false`) |
| `WARN_SIEVE` | `false` bypasses the user script |
| `WARN_SHARDPOSTCTL`, `WARN_ADMIN_SOCKET` | paths, if they differ |
| `WARN_DEADLINE` | total time in seconds (default 50, keep it below `timeout_ms`) |

A template consists of `Subject: …`, a blank line and the body text;
placeholders `${account}`, `${percent}`, `${warning}`, `${direction}`,
`${resource}`, `${storage_usage}`, `${storage_limit}`,
`${storage_usage_human}`, `${storage_limit_human}`, `${messages_usage}`,
`${messages_limit}`.

**`quota_flag.py`** sets the account's attribute via LDAP to the
**target value** (no toggling; if it is already correct, nothing is written)
and then flushes the account's Nauthilus cache
(`DELETE /api/v1/cache/flush`, bearer token via OIDC
client_credentials, fetched anew on every run). It uses `ldap3` from the
image's venv (`/usr/local/lib/shardpost/venv`, `programs/requirements.txt`
with pinned hashes) and refuses unencrypted LDAP.

| Variable | Meaning |
| --- | --- |
| `FLAG_LDAP_URI`, `FLAG_LDAP_BASE`, `FLAG_LDAP_FILTER` | server, search base, filter with `{account}` (RFC 4515 escaped) |
| `FLAG_LDAP_ATTRIBUTE`, `FLAG_LDAP_VALUE_OVER`, `FLAG_LDAP_VALUE_UNDER` | default `rnsMSOverQuota`, `TRUE`, `FALSE` |
| `FLAG_LDAP_CA_FILE`, `FLAG_LDAP_STARTTLS` | server CA; StartTLS for `ldap://` |
| `FLAG_LDAP_AUTH` | `external` (SASL EXTERNAL with `FLAG_LDAP_CERT_FILE`/`FLAG_LDAP_KEY_FILE`) or `simple` (`FLAG_LDAP_BIND_DN`, `FLAG_LDAP_BIND_PASSWORD_FILE`) |
| `FLAG_FLUSH_URL` | flush endpoint; empty disables the flush |
| `FLAG_OIDC_TOKEN_URL`, `FLAG_OIDC_CLIENT_ID`, `FLAG_OIDC_CLIENT_SECRET_FILE` | client_credentials |
| `FLAG_OIDC_SCOPE`, `FLAG_OIDC_AUTH_METHOD` | default `nauthilus:authenticate nauthilus:security` (base and flush scope), `client_secret_basic` |
| `FLAG_HTTP_CONNECT_TO` | `host:port` to connect to instead of the URL host (like `curl --connect-to`); TLS still verifies the URL host. Required because the public edge does not route `/api/v1/cache` |
| `FLAG_HTTP_CA_FILE` | CA for HTTPS (default: the system trust store) |
| `FLAG_LDAP_TIMEOUT`, `FLAG_HTTP_TIMEOUT` | seconds per operation (default 5) |
| `FLAG_DEADLINE` | total time in seconds (default 20, keep it comfortably below `timeout_ms`) |

Once the total time is up, both programs abort on their own with 75 and a
status line, before the server terminates them after `timeout_ms`. Accounts
with characters outside `A-Z a-z 0-9 @ . _ % + -` are rejected by both with
1 (as in the mailstack scripts). Both programs write a status line
to stdout that the server logs, and exit with 0 (done), 75 (temporary, for
example LDAP or Nauthilus unreachable) or 1 (configuration, no entry or
several entries). `make test-programs` checks both in the runtime base
image, `make test-programs-ldap` checks `quota_flag.py` against a
throwaway OpenLDAP (`chrroessner/openldap`, RNS schema, SASL EXTERNAL). Its
`hack/testdata/quota-ldap/slapd.conf` shows the suggested provider rule:
`authz-regexp` for the certificate identity and an ACL that grants it
write access to `rnsMSOverQuota` only.
