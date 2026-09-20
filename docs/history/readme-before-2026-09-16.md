> Historical state before the documentation rework of 2026-09-16.
> Not a current operations manual; statements and measurements may be outdated.
> Starting point: [current documentation](../README.md).

# imapcephpoc – IMAP/LMTP directly on RADOS

Proof of concept: a mail server (LMTP delivery, IMAP4rev1/rev2) that stores
messages **directly as RADOS objects in Ceph**. No CephFS, no RGW,
no file system, no database. The goal is to find out what a dedicated
server would have to look like that uses Ceph optimally and scales horizontally to
an audience of millions. The architecture is described in [ARCHITECTURE.md](../../ARCHITECTURE.md),
the stages up to a production system in
[ROADMAP.md](../../ROADMAP.md).

Test user: `test` / `test` (environment variable `USERS`, static backend).
In production: `AUTH_BACKEND=nauthilus` with `NAUTHILUS_GRPC_ADDR` and the
backchannel credentials, see [cmd/imapcephpoc/auth.go](../../cmd/shardpost/auth.go).

## Layout

```
cmd/imapcephpoc        main: configuration, listeners, health endpoints
internal/cephstore     RADOS store (librados via go-ceph): index in omap, blobs in 4 MiB chunks
                       with reference counting, full-text index in omap shards
internal/fts           tokenizer, Snowball stemming, trigrams (pure Go, no Ceph)
internal/imapproto     dedicated IMAP protocol layer (parser, dispatcher, encoder) with
                       CONDSTORE, QRESYNC, QUOTA, SORT, THREAD, OBJECTID, PREVIEW;
                       cgo-free, tested locally against an in-memory backend
internal/pop3proto     POP3 front end on the same interface
internal/sieve         Sieve execution (go-sieve) and ManageSieve server
internal/auth          authenticator: static, Nauthilus (gRPC/HTTP, OIDC client_credentials), master user, SASL parser
internal/postings      posting list encoding (delta varint, cgo-free)
internal/telemetry     OpenTelemetry setup (OTLP)
cmd/imapload           load generator (LMTP + IMAP sessions, percentiles)
internal/model         shared data types and the backend interface (cgo-free)
internal/memstore      in-memory backend for the protocol tests
internal/metrics       Prometheus metrics
internal/mailmeta      envelope/preview extraction for the index cache
internal/lmtpd         LMTP based on go-smtp
internal/rookcreds     fetches the cephx key + mon addresses from Rook (Kubernetes API, RBAC)
deploy/                Kustomize: namespace imap, Rook CRs (pools, CephClient), deployment, test pod
Dockerfile             build against librados 20.2.4 (Tentacle), runtime CentOS Stream 9 + librados2
Dockerfile.tools       Debian + swaks + curl for tests in the cluster
hack/test.sh           end-to-end test (swaks -> LMTP, curl -> IMAP)
hack/test-extended.sh  chunking (10 MiB), SEARCH over the index, COPY/MOVE/EXPUNGE
hack/test-search.sh    stemming, substrings, 40 parallel deliveries
hack/test-full.sh      hierarchy, RENAME, subscriptions, EXAMINE, keywords,
                       SEARCHRES, ENVELOPE cache, IDLE via watch/notify, TLS
hack/test-proto.sh     CONDSTORE, QRESYNC, QUOTA, SORT, THREAD, OBJECTID, MULTIAPPEND
hack/test-ops.sh       sharding, incremental poll, POP3, COMPRESS, charset, scrubber, metrics
hack/test-stage5.sh    ManageSieve, Sieve delivery, XOAUTH2/OAUTHBEARER, master user, drain, key rotation
hack/loadtest-job.yaml load test as a job in the cluster
```

## Build and deploy

```bash
make build build-tools   # build images (linux/amd64, cgo against librados)
make push                # push to docker.io/chrroessner/…
make deploy              # kubectl --context croessner apply -k deploy/overlays/croessner
make status
make test                # swaks/curl in the mailtest pod
go test ./internal/...   # protocol layer and tokenizer locally, without Ceph
```

The deploy creates two pools in `rook-ceph` (`imap-index` replicated,
`imap-data` erasure coded 2+1), a `CephClient` named `imap` and a Role
that makes exactly the secret `rook-ceph-client-imap` and the ConfigMap
`rook-ceph-mon-endpoints` readable for the ServiceAccount
`imap/imap-server`. The server fetches the key itself at startup; nothing is copied manually.

`make undeploy` removes everything again, including the pools (Rook deletes the
pools when the CRs disappear).

## Nauthilus and nauthilus-director

The overlay `deploy/overlays/croessner-nauthilus` switches the login
to Nauthilus (component `deploy/components/nauthilus`): gRPC against
`nauthilus-service.auth.svc.cluster.local:8444` (doppelgaenger sidecar,
TLS name `login.authserv.me`, system CAs), backchannel auth via OIDC
`client_credentials` (`imappoc-service`, token URL
`https://login.authserv.me/oidc/token`, scopes `nauthilus:authenticate`
and `nauthilus:lookup_identity`), master user `nauthilus-director` for the
director's backend logins, `INSECURE_AUTH=false`. The director's bearer replays
(XOAUTH2/OAUTHBEARER) go to Nauthilus with the SASL method name in the
password field.

The two secrets that the overlay expects are created in the other
repositories and are never created by hand:

| Secret (namespace imap) | Keys | Source |
|---|---|---|
| `imap-nauthilus-oidc` | client-id, client-secret, token-url | nauthilus-tests/kubernetes: `make handover-imappoc` |
| `imap-director-backend` | master-password | kubernetes/director: `make sync-imappoc-secrets` |

The server certificate of the POC listeners is issued by a namespace-local CA
(`imap-poc-ca`, cert-manager); its certificate lives in the
director repo as `certificates/imap-poc-ca.crt` and is used there as the
`tls.ca_file` of the backends `imappoc-imap/-lmtp/-sieve` (shard `imappoc`).
Nauthilus pins `rbdpoc1@exampleserver.de` and
`rbdpoc2@exampleserver.de` permanently to this shard via `director.lua`.

As of 2026-09-15: the migration has happened, the overlay
`croessner-nauthilus` is the live state. `hack/acceptance-nauthilus.sh`
checks login, LMTP delivery and the negative cases with the LDAP accounts
(passwords only from the SOPS file of the rns-server repo, in memory).

Order of the migration (each step a production rollout approved by the
respective owner):

1. nauthilus-tests/kubernetes: `make apply-prod`, then `make handover-imappoc`
   (details: `docs/imappoc-handover.md` there).
2. LDAP: the two accounts and `mail_health_imappoc` under `ou=health`.
3. kubernetes/director: `make -C director sync-imappoc-secrets`, then
   `make -C director deploy` (details: `director/imappoc.md` there).
4. Here: `kubectl --context croessner apply -k deploy/overlays/croessner-nauthilus`.
   The test scripts under `hack/` assume the static accounts and
   no longer apply after that; the acceptance test runs through the director
   (`mail.roessner-net.de`, script in `director/imappoc.md`).

Memory: librados alone holds about 150 MiB RSS, plus the
message budget (`MEMORY_BUDGET_BYTES`, 256 MiB). The deployment therefore sets
a 1 GiB limit and `GOMEMLIMIT=700MiB`; under the load test (20
users, 25 deliveries/s) the pod sits at about 230 MiB.

## Testing manually

```bash
kubectl --context croessner -n imap exec -it deploy/mailtest -- bash
swaks --protocol LMTP --server imap:24 --from a@example.org --to test@example.org
curl --url 'imap://imap/INBOX?ALL' --user test:test          # SEARCH ALL -> UIDs
curl --url 'imap://imap/INBOX;UID=1' --user test:test        # fetch the message
curl --url 'imap://imap/' --user test:test -X 'STATUS INBOX (MESSAGES UNSEEN)'
curl --url 'imap://imap/INBOX' --user test:test -X 'SEARCH SUBJECT invoice FROM alice'   # from the RADOS index
curl --url 'imap://imap/INBOX' --user test:test -X 'UID MOVE 2 Archive'
curl --url 'imap://imap/INBOX' --user test:test -X 'UID STORE 3 +FLAGS (\Deleted)'
curl --url 'imap://imap/INBOX' --user test:test -X 'EXPUNGE'
```

Environment variables: `CEPH_CHUNK_SIZE` (bytes, default 4 MiB), `FTS_SHARDS`
(shard objects per user, default 16), `USERS`, `QUOTA_BYTES`/`QUOTA_MSGS`
(0 = unlimited), `TLS_CERT`/`TLS_KEY` (enables STARTTLS and IMAPS on
`IMAPS_ADDR`), `INSECURE_AUTH` (allow LOGIN without TLS, POC default true).

Further ones: `MBOX_SHARD_SIZE` (UIDs per index shard, applies to new mailboxes,
default 100000; the POC overlay sets 20), `MAX_CONNS_PER_IP`,
`MAX_CONNS_PER_USER`, `POP3_MARK_SEEN`, `SCRUB_GRACE_SECONDS`,
`SCRUB_DRY_RUN`, `SCRUB_USERS`.

In the cluster, cert-manager issues a self-signed certificate; the service ports
are 143 (IMAP + STARTTLS), 993 (IMAPS), 110 (POP3 + STLS), 995 (POP3S),
4190 (ManageSieve + STARTTLS) and 24 (LMTP + STARTTLS). Metrics are on port 8080 under `/metrics`.
The [observability documentation](../../docs/observability.md) describes how to enable OTLP,
what the measurements mean, and the acceptance of Prometheus, Tempo and the operator dashboard. The
scrubber runs nightly as the CronJob `imap-scrub` and manually with

```bash
kubectl --context croessner -n imap create job scrub-now --from=cronjob/imap-scrub
```

Looking directly into Ceph (namespace = user):

```bash
kubectl --context croessner -n rook-ceph exec deploy/rook-ceph-tools -- rados -p imap-index -N test ls
kubectl --context croessner -n rook-ceph exec deploy/rook-ceph-tools -- rados -p imap-index -N test listomapvals mbox/INBOX
kubectl --context croessner -n rook-ceph exec deploy/rook-ceph-tools -- rados -p imap-data  -N test ls
```

## Quota, deletion, LMTP details

* **Quota per account:** the default comes from `QUOTA_BYTES`/`QUOTA_MSGS`; a per-account
  value comes from the attribute that Nauthilus supplies with the login
  (`QUOTA_ATTRIBUTE`, default `rnsMSQuota`, bytes with an optional
  K/M/G/T suffix, 0 = unlimited) and is adopted on every login and every
  delivery. `GETQUOTAROOT INBOX` shows the effective value;
  SETQUOTA by users is blocked (`QUOTA_SELF_SERVICE`).
* **Deletion:** EXPUNGE removes the index entry and releases the
  blob reference; with the last reference, the chunks are deleted
  immediately (no trash can in the store, no retention). Orphans from
  crashes are cleaned up by the scrubber after two runs.
* **LMTP:** SMTPUTF8 and 8BITMIME are advertised, every delivery
  gets its own `Received:` header (sender address, this host
  from `LMTP_HOSTNAME`, recipient, time).

## Access rights, annotations, NOTIFY, CATENATE, Sieve sending

* **ACL and shared folders (RFC 4314):** SETACL/DELETEACL/GETACL/LISTRIGHTS/
  MYRIGHTS. Another user's mailbox appears as
  `Other Users/<account>/<mailbox>` (NAMESPACE reports the prefix). Rights
  live in the owner's registry entry, entitled users get a
  reference in their own namespace; COPY/MOVE between users copies the
  data (blobs are namespace-bound). Flags are shared, not per
  user.
* **METADATA (RFC 5464):** GETMETADATA/SETMETADATA with DEPTH and MAXSIZE.
  Mailbox entries live at the owner, `/private/*` per accessing
  account; `/shared/*` at server level is global and read-only for accounts.
* **NOTIFY (RFC 5465):** SELECTED, SELECTED-DELAYED, INBOXES, PERSONAL,
  SUBSCRIBED, SUBTREE, MAILBOXES with MessageNew (including fetch attributes),
  MessageExpunge, FlagChange; at most 64 watched mailboxes,
  events also during IDLE. MailboxName, SubscriptionChange and
  AnnotationChange are rejected with BADEVENT.
* **CATENATE (RFC 4469):** APPEND from TEXT parts and IMAP URLs
  (`imap:///Postfach;UIDVALIDITY=n/;UID=n[/;SECTION=x]`), rights are
  checked, errors as BADURL.
* **Sieve redirect and vacation:** require `SMTP_RELAY_ADDR` (optionally
  `SMTP_RELAY_USER/PASSWORD`, `SMTP_RELAY_TLS`, `SMTP_RELAY_HELO`). Redirect
  keeps the envelope sender, avoids loops (no redirect to
  itself or from automatic mail, at most four targets) and is a temporary
  LMTP error on a relay failure. Vacation follows RFC 5230 (no
  reply to lists, automatic mail, the null sender, or when the
  account is not addressed), once per sender and period (marker
  on the Sieve object), the reply with a null sender and Auto-Submitted.
  For this, go-sieve is vendored as a fork with a vacation extension under
  `third_party/go-sieve` (replace directive).

## Further extensions and conformance

* IMAP: `WITHIN` (SEARCH YOUNGER/OLDER), `REPLACE` (RFC 8508, also with
  CATENATE), `LIST-MYRIGHTS` (RFC 8440), `UNAUTHENTICATE` (RFC 8437).
* Sieve (in the fork `third_party/go-sieve`): `date`/`currentdate`
  (RFC 5260), `duplicate` (RFC 7352, marker on the Sieve object),
  `editheader` (RFC 5293, Received/Return-Path/Auto-Submitted/Delivered-To
  protected; the stored message is the edited one), `mailbox`
  (`fileinto :create`, `mailboxexists`), `ihave`, `include` (`:personal`
  from the user's own script storage, `:global` from `SIEVE_GLOBAL_DIR`,
  `global`/`return`), `index`, `regex`, `spamtest`/`spamtestplus`/
  `virustest` (header and scale via `SIEVE_SETTINGS`, default
  `X-Spam-Score` with a maximum of 15), `mboxmetadata`/`servermetadata` (reads
  the IMAP METADATA entries), `special-use` (`fileinto :specialuse`,
  `specialuse_exists`), `enotify` with `mailto:` (sent through the relay,
  no notification about automatic mail), `foreverypart`/`break`,
  `mime` (`:type`, `:subtype`, `:contenttype`, `:param`, `:anychild`) and
  `extracttext`, configurable `imapsieve` and `vnd.dovecot.execute`/`pipe`/`filter` through a program allowlist (see below). Not implemented: `extlists`, `replace`/
  `enclose`, Dovecot's own `vnd.dovecot.*`. Settings with the
  Pigeonhole keys (`sieve_editheader_header/...`, `sieve_notify_*`,
  `sieve_redirect_envelope_from`, `sieve_vacation_default_subject*`) in
  `SIEVE_SETTINGS` as `key=value;key=value`.
* Conformance: `hack/conformance.md` describes the runs with Dovecot's
  imaptest (job `hack/imaptest-job.yaml`, image `chrroessner/imaptest-poc`)
  and with the Pigeonhole test suite (`SIEVE_CONFORMANCE=1 go test ./tests` in
  the fork, checkout under `third_party/go-sieve/pigeonhole`). As of 0.8.7:
  imaptest 71 of 72 groups (only the source route in the ENVELOPE is open),
  Pigeonhole as of 0.11.2: 139 files passed, 0 failed,
  9 excluded with a justification. CPU/MIME limits are verified; optional
  extensions and diagnostic/crash fixtures are explicitly delimited in
  `hack/conformance.md`.
* Header addresses (`internal/mailmeta/address.go`): a dedicated RFC 5322 parser
  for ENVELOPE, SORT and SEARCH, with groups, comments and the
  obsolete syntax; Sender and Reply-To fall back to From.
* `\Recent`: the first session that sees a message (SELECT or
  NOOP/IDLE) takes it; STATUS RECENT counts 0 afterwards.
* APPEND with `Content-Transfer-Encoding: binary` (literal8) is re-encoded
  to base64 when stored (RFC 3516 allows that), so that
  clients without BINARY can read the message as well.

## What the POC deliberately cannot do

TLS, SASL mechanisms other than LOGIN, RENAME, quota, Sieve, phrase search and
substrings across token boundaries, a garbage collector for orphaned blobs. None of that is an architectural question,
see ARCHITECTURE.md.

## Configuration file and IMAPSieve

The server supports YAML via `--config` or `IMAP_CONFIG`, validated with Viper/validator. Existing env variables remain overrides. Details: [server configuration](../../docs/configuration.md) and [IMAPSieve/Rspamd](../../docs/imapsieve.md).
