# Vendor-neutral IMAPSieve programs: review and completion

Status: **completed and rolled out to production on 2026-09-16: 0.11.0**.
Both server replicas are ready, the scrub CronJob references the same
published digest. Public acceptance with both test accounts passed;
four learning jobs confirmed through the Python helper (twice spam, twice ham).

## Result of the handover

Claude's interrupted work from 2026-09-16 was reconstructed from the conversation
history and the sources. A complete starting backup is stored under
`/private/tmp/imapcephpoc-before-extprograms-review-20260916.tgz` (mode 0600).
The workspace still has no git repository; hence no commit or git tag.

The Go server starts configurable programs through an allowlist. The
Rspamd integration lives in `programs/learn.py`; the durable learning path
remains Ceph-backed, with a lease, commit proofs, retry and failed jobs.

## Review and corrections

- **Message preservation on pipe errors:** LMTP falls back to the original in INBOX;
  IMAP does not set Deleted when a pipe fails. `:try` does not cancel the
  implicit keep on failure. Duplicate pipes to the same
  program name and the combination with reject are rejected.
- **Filters and action order:** filters see previous header changes;
  subsequent actions see the new result. Actions that are already queued
  keep their own version of the message. `execute :output` preserves line endings.
  Empty filter output does not replace a message. The line ending configuration
  for execute/filter and mutually exclusive tags have been verified.
- **Identity boundary:** on other users' mailboxes, no programs are started with the
  owner's identity, even when the trigger has ACL rights.
- **Configuration:** Viper-normalized environment names are converted back to
  uppercase; learning rules must name a program with a matching
  `learning` permission. Variable values are not modified.
- **Process limits:** the timeout also covers waiting for a free
  process slot. The program file and the program directory are checked;
  symlinks and group/world-writable entries are rejected.
  Script arguments are bounded and validated at the runner as well.
- **Queue compatibility:** the production programs are still named `spam`
  and `ham`. This keeps stored jobs and concurrently running
  0.10.1 workers compatible. Go contains no special handling of these names.
  The enqueue path now also stores the owner and the mailbox ID.
- **Python helper:** no HTTP redirects with the token, bounded responses,
  handling of 208, JSON type checking and precise already-learned detection.
  The optional spamc call uses `-x` and a checked exit code mapping.
- **Test environment:** message copies are isolated between test blocks;
  the program environment follows the respective call. The existing address fixture
  is read as `.svtest.in`. Skipped conformance files no longer count
  as passed by mistake.

Semantic basis: [Pigeonhole extprograms specification](https://github.com/dovecot/pigeonhole/blob/main/doc/rfc/spec-bosch-sieve-extprograms.txt).
The optional spamc contract follows the [Apache documentation](https://spamassassin.apache.org/full/4.0.x/doc/spamc.html).

## Verification

| Check | Result |
|---|---|
| Entire server project: Linux `go test -race -tags tentacle ./...` | passed |
| Fork: `go build ./...` and `go test ./...` | passed |
| Pigeonhole extprograms | all 10 files passed |
| Pigeonhole overall | 127 passed, 20 failed; CPU limit file handled separately |
| Isolated CPU limit test | aborted after a 20 second external limit, not passed |
| Python helper HTTP and spamc contracts | passed; synthetic data and tokens only |
| Active versus proposed server configuration | only learning/extprograms changed |
| Candidate resources: Kubernetes server dry run | passed |
| Final image: build with `go vet`, Python, `config check` | passed; UID 167, helper and directory root/0755 |
| Candidate: both accounts, native IMAPSieve acceptance | passed; APPEND, FETCH/Seen, MOVE and shard boundary 20 |
| Candidate: real extprograms calls, both accounts | pipe success, pipe failure, pipe try failure and execute passed |
| Candidate: learning queue | 4 prepared; 3 confirmed by the candidate helper; the fourth job not correlated separately with old workers active in parallel |
| Dovecot imaptest against the candidate | 72 groups: 1 known failure, 11 capability skips; base 1/460, extensions 0/261 failures |
| Nauthilus during the imaptest run | no UNAVAILABLE/initialization errors |
| Docker Hub | push and manifest readback successful |
| Production: public TLS acceptance through the director, both accounts | passed, including higher Ceph shards |
| Production: new learning confirmations | exactly 4; helper successful twice for spam and twice for ham |
| Production: image/configuration parity | both pod imageIDs, deployment, ConfigMap and scrub CronJob verified |
| Postflight | 2/2 ready, no warnings or errors during the observation period |
| Temporary resources | candidate, candidate service/ConfigMap/NetworkPolicy, imaptest job and the local tunnel removed |

The like-for-like Linux comparison against Claude's backed-up starting state
shows **no newly failing files**. Three previously red files
are now green; the CPU test is handled separately in both comparisons.

The other 20 Pigeonhole failures concern parser/error counting,
multiscript, extlists, further Dovecot plugins, fuzz fixtures and existing
SMTP/vacation/notify deviations. They are not a passed full conformance.
The CPU limit test remains a separate known gap outside this
extprograms migration. The Pigeonhole source files were not modified.

The single imaptest failure is the already documented difference with
stale source routes in `fetch-envelope`; no new IMAP deviation.
The deliberately triggered candidate failures produced four failed
test program calls and two apply errors, each as expected with the
messages preserved. These test programs are not in the production configuration.
A first tunnel attempt ran into local ports that were already in use; the acceptance
was carried out exclusively over dedicated ports that were confirmed afterwards.

Verification logs (local):
- `/private/tmp/extprograms-server-verified.log`
- `/private/tmp/extprograms-conformance-complete.log`
- `/private/tmp/extprograms-conformance-summary.txt`
- `/private/tmp/extprograms-cpu-limit.log`
- `/private/tmp/extprograms-release-build.log`
- `/private/tmp/extprograms-baseline-conformance.log`
- `/private/tmp/extprograms-imaptest.log` (0600)
- `/private/tmp/extprograms-candidate-native.log`
- `/private/tmp/extprograms-production-native.log`
- `/private/tmp/extprograms-production-metrics-before.json`
- `/private/tmp/extprograms-production-metrics-after.json`
- `/private/tmp/extprograms-push.log`

An earlier parallel test run was invalid because the vendor tree was renewed
at the same time. The successful full run uses a fixed source copy.

## Rollout and rollback

The user explicitly approved the Docker Hub push and the cluster rollout.
Published: **0.11.0**, the tag did not exist before the push.
Digest: `sha256:75b1361e550f6eef7ec4d11b1deb3497ef9ab6c2ee9e9666f91c733812af4318`.
The image build including `go vet`, the local `config check` and the registry readback
are recorded as the release chain.
Production changes were limited to the server deployment, its ConfigMap and the
image of the scrub CronJob. Director, Rspamd and the Ceph configuration
remain outside this change.

Previous image:
`docker.io/chrroessner/imapcephpoc:0.10.1@sha256:f9bdea21100a3bcd76a1e8de40fec52f4e70109b2e6770247307c79e8a25574b`.

Rollback backups (0600):
- `/private/tmp/imap-extprograms-deployment-before.json`
- `/private/tmp/imap-extprograms-scrub-before.json`
- `/private/tmp/extprograms-config-before.json`

The previous ConfigMap `imap-server-config-gcc6246d4t` remains available for a rollback;
`imap-server-config-dk4c766t5f` is active. A rollback
restores the old image and the old ConfigMap together;
no deletion of mail or queue data. Renaming
program names later requires its own queue transition strategy.

External program calls and Ceph changes do not form a single
transaction. Repeated external effects are possible on aborts;
exactly-once is not guaranteed. The optional spamc backend contract was
verified synthetically, not against a production SpamAssassin service.
