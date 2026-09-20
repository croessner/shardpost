# Sieve fixes after 0.11.0

Status: **fully completed and accepted in production on 2026-09-16:
0.11.2, both replicas ready, four new learning jobs confirmed.** Work was done strictly sequentially, without subagents.
The excluded historical IMAP address rendering is not being changed.

## Root causes fixed

- CPU/runtime budget: regular scripts get five seconds of elapsed time
  by default. `sieve_max_cpu_time` can specify a positive time interval;
  a shorter context of the caller wins (IMAPSieve: 500 ms in
  the active configuration). Loops, includes and the regex input reader
  check for cancellation. No detached compute goroutine keeps running afterwards.
- MIME: at most four nested `foreverypart` loops, including across
  includes. The MIME tree is bounded to 1000 parts, depth 20 and 256 KiB of headers per
  part. Decomposition checks the context and uses body slices;
  exceeding a limit aborts the run.
- Parser: superfluous commas, multiple address part tags, unknown
  envelope fields, invalid numeric comparator types, a missing
  `foreverypart` dependency and invalid redirect addresses are
  rejected. Variable redirect targets are additionally checked at runtime.
  Permissible spaces in valid addresses are preserved.
- Comparisons: non-ASCII literals are encoded correctly for the byte regex;
  this makes comparisons with ellipsis characters, among others, work.
- Vacation: `-request` is detected on the local part of the address. The production
  adapter never replies to an empty SMTP sender. Reply headers
  keep matching display names and bounded References; MIME-encoded
  subject lines are decoded before truncation. Configured own
  addresses and recipient checks are taken into account. The default for
  the reply envelope remains the null sender.
- Test environment: flags and test state are copied correctly; an error keep
  uses the original; SMTP loop checks take the user
  and delivery address into account. Multiple scripts share flags/message changes,
  execute new actions exactly once and merge results.
  An indexed action test really only checks the selected index.
- Stored scripts keep their configuration and action limit.

## Verification and limits

The fixed source copy is stored under `/private/tmp/imap-sieve-fixes-verified`.
Full Linux Sieve run with the race detector: **139 passed, 0 failed,
9 explicitly excluded, 0 unchecked out of 148 files**. The former
CPU test now passes after about two seconds. Tests for regex cancellation,
MIME limits and flag isolation run in addition. The full
server run `go test -p 1 -race -tags tentacle ./... -count=1` passes
as well, including the new production adapter checks.

The nine exceptions are not passed tests:

| Files | Reason / substitute evidence |
|---|---|
| 3 extlists | No list backend, the extension is not advertised. |
| 2 Dovecot environment | Optional Dovecot plugin not advertised. |
| 1 Dovecot report | Optional Dovecot plugin not advertised. |
| compile/errors.svtest | Dovecot collects multiple errors and checks its own error texts. Our compiler reports the first error. The re-enabled `TestCompileErrors` checks all invalid scripts listed there for actual rejection; conformance with Dovecot's diagnostic texts is not claimed. |
| failures/fuzz1.svtest | The historical crash file requires a mailbox that is never created and flags that are never set. The missing-mailbox path and real flag copies are checked separately. |
| failures/fuzz4.svtest | The historical crash file contains invalid notify syntax; its rejection is checked separately. |

Further `test_error :count` checks of the existing adapter compare
the absence of errors against an error state, not the exact number of all Dovecot
diagnostics. The number 139 therefore means files passed in the documented
adapter, not unqualified full Dovecot or RFC conformance.
The original Pigeonhole files were not modified.

A macOS run additionally showed local time zone/shell differences.
What counts is the Linux verification with UTC, as in the previous acceptance.

Logs:
- `/private/tmp/sieve-linux-final.log`
- `/private/tmp/sieve-server-final.log`
- `/private/tmp/sieve-cpu-fixed.log`


## Publication and cluster

Docker Hub tag: `docker.io/chrroessner/imapcephpoc:0.11.1`.
Digest: `sha256:a59e05f489e8ee6cc8030c468e77f2f5e039c408ee1c9d0adad07dfdb61343bf`.
The registry readback matches the locally tested image.
The image build passed with `go vet` and `config check`; user `167:167`.
A first automatic push check demanded more concrete target/payload
evidence. Repository, logged-in user and the comparison against 0.11.0 were
verified; the subsequent push was approved and executed. In the
runtime image, of the program files only the server binary changed,
not imapload or learn.py. No source or secret directories
were added.

Isolated candidate with an unchanged production configuration: both accounts
passed. CPU cases including the IMAP response after 0.667/0.645 seconds,
MIME cases after 0.203/0.168 seconds; the message preserved byte-identically in each case,
the follow-up script successful. Four deliberately triggered script failures and four
prepared learning jobs, no further warnings or errors. The
learning delivery on the candidate was not attributed to the candidate alone, because old workers were active at the same time.

The only production changes were the image references of
deployment `imap-server` and CronJob `imap-scrub`. JSON patch preconditions
checked the previous 0.11.0 digest. Both new pods are ready and their
image IDs match the registry digest. The ConfigMap and the rest of the configuration
remain unchanged. The temporary candidate, the candidate network policy and the
local tunnel were removed.

Rollback: set the saved 0.11.0 image
`docker.io/chrroessner/imapcephpoc:0.11.0@sha256:75b1361e550f6eef7ec4d11b1deb3497ef9ab6c2ee9e9666f91c733812af4318`
back in the server deployment and the scrub CronJob. The configuration was
not changed. Starting objects (0600):
`/private/tmp/sieve-fixes-deployment-before.json` and
`/private/tmp/sieve-fixes-cronjob-before.json`.

Further evidence:
- `/private/tmp/sieve-release-build.log`
- `/private/tmp/sieve-release-push.log`
- `/private/tmp/sieve-release-manifest.json`
- `/private/tmp/sieve-candidate-native.log`
- `/private/tmp/sieve-production-native.log`


## Additionally found helper bug and 0.11.2

The first public learning acceptance under 0.11.1 yielded three confirmed out of four
jobs. One ham test job received five temporary errors and was stored as
dead. There was neither a queue read error nor a new
interpreter error: the direct, bounded check against the saved test content
returned **HTTP 204 without a body**. The helper treated only 208 as success without
JSON and incorrectly classified the 204 response as a JSON error/retry.

`learn.py` now accepts only 204 and 208 without JSON; an empty
200, malformed JSON, rejections, redirects and 5xx are still handled
according to the previous contract. The full HTTP/spamc contract tests
pass in the Linux image and against the helper that is actually installed.
On the isolated 0.11.2 candidate, the helper confirmed the identical previously
failed test content with exit code 0.

**Final release: 0.11.2**
`docker.io/chrroessner/imapcephpoc:0.11.2@sha256:8e51fecb8d7539c4f4654d5718799fe8bfb7140f9e878443a8e9e725d5abea3e`.
The image was derived from the immutable 0.11.1 digest and replaces
only `/usr/local/lib/imapcephpoc/programs/learn.py`. The normal Dockerfile
uses the same corrected source for future full builds.

- Server binary in 0.11.1 and 0.11.2:
  `sha256:4e36e9c2c78deda62a7e7b7fa723abbb3b3ddd1770129e1ace65582376de4458`
- Installed helper and corrected source:
  `sha256:25f2544b107b52ca4ac73b316d5c57b1875d6e98bb8976647952dbf2b2def920`
- The registry manifest matches the local final image.
- `/private/tmp/Dockerfile.sieve-helper-fix`,
  `/private/tmp/sieve-helper-build.log`,
  `/private/tmp/sieve-helper-push.log`,
  `/private/tmp/sieve-helper-manifest.json` document the supplementary build.

The initially failed test job was released again exactly once, after a metadata/payload
equality check: Dead=false, Attempts=0,
Next=now. The unchanged payload and the delivery proofs were preserved.
The production 0.11.2 worker confirmed exactly one successful ham execution
and one delivery and then removed the job from the queue regularly.
No success was set manually and no job was deleted manually.
The backup stays local with mode 0600.


## Final public acceptance of 0.11.2

Over the public TLS/director access, both accounts passed the native
IMAPSieve acceptance including APPEND, FETCH/Seen, MOVE and shard boundary 20.
CPU cases: 0.694 and 0.652 seconds including the IMAP response; MIME cases:
0.195 and 0.133 seconds. The original messages preserved byte-identically in each case,
no Deleted marking by the failed script, and immediately
afterwards a successful normal follow-up script.

Fresh counter comparison after the separate resumption:

| Result | Delta |
|---|---:|
| Prepared learning jobs | 4 |
| Deliveries confirmed by the worker | 4 |
| Helper spam / ok | 2 |
| Helper ham / ok | 2 |
| Retry / dead / tempfail | 0 |
| Deliberately triggered Sieve aborts | 4 |
| Other warnings/errors | 0 |

The learning queue is empty. Both production pods
`imap-server-dd5cf558d-6fx6s` and `imap-server-dd5cf558d-n9hpk` are ready,
have zero restarts and use the final 0.11.2 digest. Deployment and
scrub CronJob are pinned to this digest. A comparison of the entire
pod spec against the starting backup showed only the image change;
ConfigMap `imap-server-config-dk4c766t5f` stays the same.
Both candidate runs are cleaned up, the port forward is terminated.

Final evidence:
- `/private/tmp/sieve-final-native.log`
- `/private/tmp/sieve-final-metrics-before.json`
- `/private/tmp/sieve-final-metrics-after.json`
- `/private/tmp/sieve-final-postflight.json`
- `/private/tmp/sieve-recovery-metrics-before.json`
- `/private/tmp/sieve-recovery-metrics-after.json`

The working folder has no git repository; therefore no
git commits or git tags were created. Source code, vendor fork, release overlay and
documentation contain the finished state. The excluded historical
IMAP address rendering was not touched.
