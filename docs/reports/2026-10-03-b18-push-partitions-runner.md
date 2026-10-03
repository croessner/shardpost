# B18: push partitions (B18a) and job runner pods (B18b)

Date: 2026-10-03. Base: 0.51.0 plus `ed72c98`. Branch
`b18a-push-partitions`. No version bump, no image, no rollout: this
report describes the implementation on the branch, not a release.

## B18a: push partitions

**What changed.** The JMAP push worker shares 64 partitions (the shards
of the hint index `ix.jmap/push/acct`) among its pods: a lease per
partition (`LockQueueKeep`, standing through key rotations, moved with
`Retake` after one), a membership object `ix.jmap/push/members` with a
heartbeat every third of the lease and event-driven writes, the share
⌊64 / n⌋ (+1 for the first 64 mod n pods by name), orphaned and
contested partitions taken at once over the share on the fast cadence,
sheds of one partition per rebalance interval after the settle time,
waves of a third at shutdown with one control notify each, restart
detection, a membership older than two cadences that freezes taking and
shedding. The state per account carries an acknowledgement per
subscription and an epoch; the record is written with a compare-and-swap
that merges by max (the higher epoch wins whole). The holder of a
partition reads its shard alone, a record only when its checksum in the
index value changed or has none, and every record once per full scan
interval; the front-ends notify the partition's lock object with the
account as payload. The pods of 0.52.0 hold the lease record of the
single worker as a marker and wait while an old pod holds it.
Registrations show `partitions` and `watched`; the push deployment has
two replicas, a rolling update, a spread and a disruption budget.

**Deviations from the plan (Fassung 6).**
- `Retake` takes no `*User`: the lease reopens its view on the current
  connection itself (the worker has no access to a `User`).
- The membership heartbeat is a third of the lease (10 s at 30 s), not
  a fixed 10 s, so that a shorter lease keeps its records fresh.
- The membership record carries the pod's bounds (`max_held`,
  `max_watched`), which the capacity rule of §4.4 needs.
- A partition once seen orphaned is tried over the share until a fresh
  member holds it (not only while the stale record is there), and a pod
  restarted under the name of a crashed one waits a renewal interval
  and a cadence beyond its crashed record's expiry, so that the healthy
  pods hold the orphaned partitions before it tries anything.
- The round budget of a watcher is bounded by four fifths of
  `handoff_seconds` (3 s, as the plan says, with the default hand-off).
- The "backend suspected" rule of the account check applies per
  partition scan, not per scan round of the pod.
- `imapceph_jmap_push_lock_op_seconds` is observed in the store for the
  keep leases (take, renew, release).
- Verification queue items carry a `queued` time for the age metric.
- The tests of §8.1 run with short real intervals (a lease of 600 ms, a
  cadence of 100 ms), not with a fake clock.
- The push deployment is committed with `replicas: 2`; the runbook
  rolls out with 1 first and sets 2 after the smoke test.

**Not done (B18a).** Of §8.1: the tests "members hang 7 s → unknown",
"at most one read in flight", "self count", "event-driven write",
"no flapping over 30 min", and the CAS case of the epoch with two
watchers (the merge rule is unit-tested). Of §8.2: the self-stop and the
blocked lock object against real RADOS (test hooks in the store) and the
two-worker run over real RADOS with 200 accounts; the system pool
variant runs as part of `make test-docker`. The smoke test on croessner
(§8.4) and the measurements M1–M4 (§10) belong to the rollout.

## B18b: job runner pods

**What changed.** `shardpost runner` runs the job runner with the expiry
and `fts_due` workers, the upload expiry, the delayed submission worker
and the learning worker without any listener, registers with the role
`runner`, checks the placement like a server and loads the servers'
configmap without the authentication secrets (`config.LoadRunner`).
`workers.run` (`WORKERS_RUN`, default true) makes a server a front-end
that keeps the jobs API, the registration, the auth flush and the
writers of the queues; it takes precedence over
`jmap.submission.delayed.worker`, and the front-end warns once when no
runner is registered. The job manager moves from the admin listeners
into the server's background. `jobs.max_parallel` gets
`JOBS_MAX_PARALLEL`. The learning holder runs
`imapsieve.learning.concurrency` jobs at once with an in-flight set, a
semaphore per account tried without waiting and a detached context with
`shutdown_seconds` of grace; only the holder watches the queue. The
delayed submission worker names the partitions it hands over in a
notify with payload, and another worker takes them at once. `--pods` of
the cluster marks counts servers and runners (`IsStoreWriter`).
Registrations show `workers`. Metrics: `imap_imapsieve_learning_job_seconds`,
`_due`, `_lag_seconds`, `imapceph_job_claim_wait_seconds`,
`imapceph_job_running`. Component `deploy/components/runner` (not in the
croessner overlays).

**Deviations from the plan.**
- `config.LoadRunner` leaves the secret files of the authentication
  unread and skips the authentication checks: `config.Load` resolves
  `*_file` secrets itself, so the runner needed a load of its own to
  start without them.
- With a concurrency of 1 a pass runs its jobs itself (synchronous), as
  before; with more, a pass waits for a free runner slot (the account
  semaphore never blocks it).
- The claim wait is measured from the job's creation (queued) or the
  end of its lease (left behind) to the claim.

**Not done (B18b).** The end-to-end test "serve with WORKERS_RUN=false
answers the jobs API and a runner executes the submit" (the test checks
the registration and that no worker lease is taken); the smoke test of
the separated topology on croessner (§8.4).

## Checks

`make lint`: 0 issues. `make man`, `make check-generate`: clean.
`go test ./...` of the packages that build on macOS: green.
`make test-docker`: green (both runs, index pool and system pool), after a
race in the lease test of `cephstore` was fixed (the lock observer is an
atomic pointer now).

Note on the environment: `internal/sieve`'s conformance regression test
reads `third_party/go-sieve/pigeonhole/...`, an untracked directory of
the main checkout; a fresh clone or a worktree lacks it (copied by hand
for the run).
