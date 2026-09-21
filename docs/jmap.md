# JMAP

[Documentation](README.md) · [Architecture](../ARCHITECTURE.md) · [RADOS storage](architecture/storage.md)

JMAP is roadmap stage 6: a third front-end beside IMAP and POP3, on the
same store. It is a protocol layer on top of `model.User`, not a second
data store — everything it answers comes from the same index, the same
blobs and the same account change log that IMAP reads. The staged plan
behind it is `temp/08-jmap-staged-plan.md`.

## What exists

| Stage | Contents | State |
|---|---|---|
| 0 | The account change log: one state per account, and the changes behind it | built |
| 1 | Core (RFC 8620): session resource, API endpoint, authentication, blobs | built |
| 2 | `Mailbox/*` | planned |
| 3 | `Email/get`, `Email/changes`, `Thread/*` | planned |
| 4 | `Email/query`, `queryChanges`, `Email/set` | planned |
| 5 | Push (EventSource and Web Push), `EmailSubmission/*`, `Identity/*` | planned |
| 6 | `SieveScript/*`, `Quota/*`, `Blob/*`, sharing | planned |

A client that asks for a capability this server does not have is told
`unknownCapability`, and a method that is not implemented answers
`unknownMethod`. Nothing is served half-correct.

## The account state

IMAP counts MODSEQ per mailbox; JMAP wants one number per account and
data type. The store keeps an account change log for it, described in
[RADOS storage](architecture/storage.md#the-account-change-log). Two
consequences matter outside the store:

- Every write path now writes to the account log as well. That is the
  price of serving two state models from one store, and it is paid on
  every delivery, flag change, copy, move, expunge and mailbox change,
  whether or not the account has a JMAP client.
- The log is bounded (`server.account_log_retention`, 10000 entries).
  A client whose state token is older resynchronises.

## Endpoints

| Path | Method | Contents |
|---|---|---|
| `/.well-known/jmap` | GET | The session resource (RFC 8620 2) |
| `/jmap/api/` | POST | Method calls, batched, with back-references |
| `/jmap/upload/{accountId}/` | POST | Blob upload |
| `/jmap/download/{accountId}/{blobId}/{name}` | GET | Blob download, with HTTP ranges |
| `/jmap/healthz` | GET | Backend check: no login, no side effects |

The account id is the canonical account name. It is already stable,
unique within the server and the thing every log line names; an opaque
number would only add a mapping to keep correct. Only the account of the
authenticated user is listed in the session — other accounts (shared
mailboxes, the public namespace) belong to JMAP Sharing, which is stage
6 of the plan.

A blob id is the GUID of the message data, the same identity IMAP
reports as `EMAILID`. An uploaded blob that nothing references is
debris: the scrubber removes it once two consecutive runs have seen it
unreferenced, which is the expiry RFC 8620 6.1 allows. Two consequences
an operator should know: the expiry is the scrub cadence, not a time, so
a deployment that does not schedule `shardpost scrub` never collects
them; and an upload is only refused when the account is already over its
quota, because an unreferenced blob is counted nowhere until something
references it. A tighter bound belongs with `Blob/*` (RFC 9404) in stage
6, where blob lifetime is a protocol matter rather than a guess.

A download is served with `X-Content-Type-Options: nosniff` and a
sandboxing `Content-Security-Policy`, because the media type is whatever
the client asked for and must never be interpreted as a document of this
origin. A request for an account other than the authenticated one is a
404 rather than a 403: the server does not confirm that the other
account exists.

## Authentication

Basic and Bearer, both verified against the same Nauthilus backend the
other front-ends use (`internal/auth`). The server verifies every
request itself even when a Director in front has already done so, so
that it is correct without a Director as well.

A Bearer token needs `nauthilus.bearer_mode: introspect`: the token has
to yield the account, and the `password` mode has no user name to go
with it. Authentication on a plaintext connection is refused unless
`jmap.insecure_auth` is set.

## Configuration

JMAP listens on an address of its own rather than sharing
`server.http_addr`: that one serves metrics and the health probes and is
cluster-internal, while JMAP is customer traffic with its own
certificate and its own network policy. Without `jmap.addr` the
front-end is not started at all.

```yaml
jmap:
  addr: ':8443'
  tls_cert: /tls/jmap/tls.crt
  tls_key: /tls/jmap/tls.key
  public_base_url: https://jmap.example.org
  max_size_upload: 52428800
  max_size_request: 10485760
  max_concurrent_upload: 4
  max_concurrent_requests: 4
  max_calls_in_request: 16
  max_objects_in_get: 500
  max_objects_in_set: 500
```

`tls_cert`/`tls_key` fall back to `tls.cert`/`tls.key` when they are
empty. A limit left at zero takes the built-in default; the session
resource advertises whatever is in force, so a client adapts rather than
guessing.

`public_base_url` is the address clients reach. Behind the Director that
is the Director's address and not this pod's: its default mode
(`backend_advertised`) rewrites nothing, so the session resource has to
advertise URLs that work from outside. Without the setting the URLs are
built from the request, which is right for a direct connection only.

## Limits of this stage

- `Email/*`, `Mailbox/*` and everything else of RFC 8621 are not here.
- Push is not here: the session advertises an `eventSourceUrl`, and
  stage 5 makes it answer.
- The private `\Seen` marks of a public folder are recorded in the
  account log of the public account, not of the account the mark belongs
  to. JMAP Sharing (RFC 9670) is where that is resolved.
- `Email/queryChanges` is decided, not built: it will answer only for
  the default sort and return `cannotCalculateChanges` otherwise, rather
  than carrying a materialised index per sort key.
