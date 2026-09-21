# Conformance runs

## IMAP: Dovecot imaptest

Image `docker.io/chrroessner/shardpost-imaptest` (`imaptest-poc` up to 0.12.x) = `dovecot/imaptest` plus the
test scripts from `github.com/dovecot/imaptest` (`src/tests`, 112 files)
and the example mailbox `dovecot-crlf`. Runs as a job in the namespace `shardpost`
against `shardpost:993` (TLS, own CA) with a test account from the secret
`imaptest-creds` (keys `user`, `pass`):

```bash
imaptest host=shardpost port=993 ssl=any-cert user=... pass=... mbox=/mbox test=/tests
```

The job template is in `hack/imaptest-job.yaml` (restricted
PodSecurity, secret `imaptest-creds`); results and assessment are below.

## Sieve: Pigeonhole test suite

The go-sieve fork ships the Dovecot test suite dialect (`vnd.dovecot.testsuite`).
With a checkout of the Pigeonhole tests under
`third_party/go-sieve/pigeonhole/tests` (symlink `tests/pigeonhole`):

```bash
cd third_party/go-sieve
SIEVE_CONFORMANCE=1 GOFLAGS=-mod=mod go test ./tests -run TestPigeonholeConformance -v | grep "SIEVE CONFORMANCE\|  FAIL"
```

## JMAP: not yet a conformance run

The JMAP front-end is being built in stages (see
[docs/jmap.md](../docs/jmap.md)). Until the mail data types of RFC 8621
exist there is nothing a conformance corpus could run against: a suite
would report "everything unimplemented", which is already in the
documentation. What can be checked today is Core, by hand against a
running server:

```bash
curl -sk -u "$USER:$PASS" https://<host>:8443/.well-known/jmap | jq .
curl -sk -u "$USER:$PASS" -H 'Content-Type: application/json' \
  -d '{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"hello":1},"c1"]]}' \
  https://<host>:8443/jmap/api/ | jq .
```

The planned run, once stage 4 is in: the `jmap-samples`/`jmap-client`
corpus, the Stalwart JMAP test set, and Thunderbird against the
Director, with its results recorded here like the IMAP ones.

## Results 0.11.2 (2026-09-16)

The server binary in 0.11.2 is identical to the fully verified 0.11.1.
The additional helper fix for HTTP 204 passes dedicated HTTP and spamc contract
tests, including against the installed helper in the final image.
The sequential Linux acceptance from a fixed source copy passes:
`go test -p 1 -race ./...` in the Sieve fork and
`go test -p 1 -race -tags tentacle ./...` in the server project.

Pigeonhole: **139 passed, 0 failed, 9 explicitly excluded,
0 unchecked out of 148 files**. All ten extprograms files and the CPU limit
test that used to hang now pass. The nine exclusions cover six files
for extensions that are not offered, the Dovecot-specific diagnostics check
and two invalid historical crash scenarios. Compiler rejection,
error paths and flag isolation are additionally checked separately.

For error counts, the adapter still compares error-free against
error state, not the exact number of Dovecot diagnostics. Details and
evidence are in the [fix report](../docs/reports/2026-09-16-sieve-conformance-fixes.md).
Exclusions never count as passed. The full Sieve run no longer needs
the CPU test to be excluded. Platform reference: Linux with `TZ=UTC`;
the unmodified extprograms test programs expect Linux shell behavior.

Public acceptance 0.11.2: both accounts passed, including the
CPU/MIME abort with a byte-identical original and a successful follow-up script.
Four new learn jobs confirmed (2 spam, 2 ham), no new retries or dead jobs,
learn queue empty. Both production pods ready, no restarts.

## Results 0.11.0 (2026-09-16)

The Linux fork build and the regular fork tests pass. The address fixture
that was missing until now is read directly from the existing `.svtest.in` source file;
the unchanged, documented exclusions in the address test remain.

Pigeonhole: **127 passed, 20 failed, 1 separately checked
CPU limit file out of 148**. All ten extprograms files pass.
The 20 remaining failures still concern extensions that are not implemented
and existing general deviations; this is not a full
Sieve conformance sign-off. The raw logs and the detailed assessment are
in the [acceptance report](../docs/reports/2026-09-16-extprograms.md).

The CPU limit test was run separately, aborted after a 20 second outer time limit,
and did not pass.
For the remaining run:

```bash
SIEVE_CONFORMANCE=1 GOFLAGS=-mod=mod go test ./tests \
  -run TestPigeonholeConformance \
  -skip 'TestPigeonholeConformance/execute/errors-cpu-limit.svtest' -v
```

The summary explicitly counts filtered-out files as not
run. They do not count as passed.

## Results (2026-09-15, image 0.8.3)

Sieve (image 0.9.0, fork state 2026-09-15): **117 of 148 files** pass
(0.8.3: 50). To get there, the following were implemented: ihave, include (with global/return,
:once/:optional, cycle and depth limits), index, regex, spamtest/
spamtestplus/virustest (header and thresholds configurable), mboxmetadata/
servermetadata, special-use, enotify with mailto, foreverypart/break, mime
(:type/:subtype/:contenttype/:param, :anychild) and extracttext; plus the
test suite environment (mail store, outbound SMTP, mailbox creation, metadata,
configuration switches, files from the Pigeonhole examples). The 31 open
files: extprograms/vnd.dovecot.* (Dovecot's own plugins: execute,
filter, pipe, report, environment, 12 files), extlists (no
list backend, 3), multiscript/test_multiscript (4), fuzz fixtures with
deliberately broken headers (2), parser recovery and error counting
(compile/errors, recover, mime/errors, editheader/errors: our loader
reports the first error, Pigeonhole counts all of them), CPU limit, local
time zone (date/zones), two vacation details (subject truncation with an
ellipsis, reply rules in special cases) and one
redirect loop case in the SMTP test.

## IMAP result 0.11.0 (2026-09-16)

The full suite against the release candidate produced 72 groups: **1 failed,
11 skipped because of missing capabilities**. Base protocol: 1/460
commands failed; extensions: 0/261. The only failure remains
`fetch-envelope` with obsolete source routes. No Nauthilus UNAVAILABLE
or initialization errors in this run. The public native acceptance
with both accounts, along with four Rspamd learn confirmations, passed after the rollout.

## IMAP result (2026-09-15, image 0.8.7; 0.8.8 bugfix only, see docs/review-round6.md)

History: 0.8.3 26 of 72 groups red, 0.8.4 9 red, 0.8.6/0.8.7 **1 red**
(72 groups, 11 skipped because of missing capabilities; base protocol
1 of 441 commands, extensions 0 of 261; plus 0 to 2 groups
per run whose LOGIN failed on a Nauthilus tempfail, see the note).

Fixed between 0.8.4 and 0.8.6: STATUS RECENT (the first session that
sees a message takes its \Recent), ESEARCH MIN/MAX with the MODSEQ of the
returned subset, RFC 5322 address parser for ENVELOPE (groups,
comments, Sender/Reply-To defaulting to From), SEARCH FROM/TO/CC/BCC and
HEADER <address> also against the parsed addresses, SORT by group
names, THREAD=ORDEREDSUBJECT with the oldest message as the root,
MOVE into the selected mailbox (EXPUNGE before EXISTS), LIST CHILDINFO only
for children that no pattern lists itself, subscription survives DELETE
(\NonExistent), FLAGS/PERMANENTFLAGS before every FETCH with a new keyword,
APPEND with Content-Transfer-Encoding: binary is re-encoded to base64.

Open: for `<@route:user@domain>`, `fetch-envelope` expects the
source route in the ENVELOPE (`(NIL "@route" "user" "domain")`); the parser
discards the obsolete route. Deliberately not implemented (go-imap does not know
the field, no client needs it).

Note: a run can additionally fail on a `NO [UNAVAILABLE]` response
(imaptest then reports "initialization failed"); this is not a
server error but a sporadic tempfail from Nauthilus after
successful authentication (see the final report of 2026-09-15).
