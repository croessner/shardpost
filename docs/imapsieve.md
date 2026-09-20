# IMAPSieve, external programs and learning

IMAPSieve reacts, after the store, to APPEND/MULTIAPPEND, COPY, MOVE (Sieve cause COPY), actual STORE flag changes and FETCH that sets the Seen flag. A STORE that changes nothing and BODY.PEEK do not trigger a flag run. Scripts run outside the connection write lock; the changes they produce reach clients over the existing IMAP notification path.

```yaml
imapsieve:
  enabled: true
  user_scripts: true
  managesieve_url: sieve://mail.example.org:4190
  timeout_ms: 500
  max_parallel: 4
  learning:
    enabled: true
    max_attempts: 5
    max_message_bytes: 4194304
    rules:
      - name: spam
        operations: [COPY, MOVE]
        destination_special_use: '\Junk'
        source_not_special_use: '\Junk'
        program: spam
      - name: ham
        operations: [MOVE]
        source_special_use: '\Junk'
        destination_not_special_use: ['\Junk', '\Trash']
        program: ham
extprograms:
  bin_dir: /usr/local/lib/shardpost/programs
  programs:
    spam:
      command: learn.py
      args: ["--action", "spam"]
      kinds: [learning]
      timeout_ms: 30000
      env: {LEARN_BACKEND: rspamd, LEARN_URL: "http://classifier:11334", LEARN_TOKEN_FILE: /run/secrets/learning/token}
    ham:
      command: learn.py
      args: ["--action", "ham"]
      kinds: [learning]
      timeout_ms: 30000
      env: {LEARN_BACKEND: rspamd, LEARN_URL: "http://classifier:11334", LEARN_TOKEN_FILE: /run/secrets/learning/token}
```

## External programs (extprograms)

The server knows neither a classifier nor a shell. Everything that starts
an external program goes through the `extprograms.programs` table,
following the pattern of a sudoers rule: the name is the key, the entry is
exactly one command line. A Sieve script or a learning rule can only
choose the name.

* `command` lives under `bin_dir`, without a directory component, must be
  a regular file (not a symlink) and must not be writable by group or
  others. The directory belongs to root, the server runs as an
  unprivileged user.
* `args` are fixed; `{account}` and `{mailbox}` are substituted, and for
  quota programs the quota placeholders as well (see `docs/quota.md`).
  Scripts may append arguments of their own only with
  `allow_script_args: true` (character filter: no control characters, at most
  64 arguments of 1024 bytes each).
* `kinds` restricts the use to `execute`, `pipe`, `filter`,
  `learning` or `quota` (quota warnings and the over flag); empty means all.
* Environment: only `PATH`, `LANG`, `HOME`, the entries from `env` and the
  invocation values (for `quota` the `QUOTA_*` values instead of these) `SIEVE_ACCOUNT`, `SIEVE_MAILBOX`, `SIEVE_CAUSE`,
  `SIEVE_CHANGED_FLAGS`. In the learning worker, `SIEVE_MAILBOX` holds the
  stable mailbox ID; for Sieve events it holds the mailbox name. The message arrives on stdin, stdout is read up to
  `max_output_bytes` (default 1 MiB), `timeout_ms` (default
  10 s) terminates the process group, including the wait for a free execution slot. `max_parallel` (default 4) limits
  concurrent runs per entry.
* Exit codes: 0 success, 75 (`EX_TEMPFAIL`) a temporary error,
  everything else permanent. Metric `imap_extprograms_runs_total{program,result}`.

This makes Pigeonhole's extensions available in Sieve, during
delivery as well as in IMAPSieve scripts, under the capability names
`vnd.dovecot.execute`, `vnd.dovecot.pipe` and `vnd.dovecot.filter`
(ManageSieve advertises them only when programs are configured):

```sieve
require ["vnd.dovecot.execute", "vnd.dovecot.pipe", "vnd.dovecot.filter", "variables", "copy"];
if execute :pipe :output "score" "classify" { ... }   # the program reads the message, stdout into ${score}
pipe :copy "archive";                                  # copy to a program, delivery continues
filter "rewrite";                                      # the program's stdout replaces the message
```

`execute` and `filter` run while the script is evaluated. `pipe` is
registered as a delivery action and is executed before the message is
discarded or `\Deleted` is set. Under IMAP, an error without `:try` keeps
the original; under LMTP it leads to the safe INBOX delivery. A
failed `pipe :try` behaves like an action that was not performed and
does not cancel the implicit keep. A program may receive only one
pipe action per script; pipe and reject are incompatible.

`execute` and `filter` can also be used as tests. Program names are
constants. Filters and header changes apply to subsequent actions;
actions that are already registered keep their own version of the message.
The output of `execute :output` is preserved including its line endings.
The input of these two invocations uses LF by default;
`sieve_execute_input_eol=crlf` and `sieve_filter_input_eol=crlf`
in `sieve.settings` can select CRLF. The durable learning queue hands the
original message to the program launcher unchanged.

In the IMAP path, external programs may only be invoked by the mailbox
owner. Access to a shared mailbox must not cause external side effects
under the owner's identity. Program invocations and
Ceph writes are not one common transaction: after an
abort, external effects can be repeated. Programs have to be suitable for
such cases; exactly-once is not guaranteed.

## Learning program

`programs/learn.py` is an example built purely on the standard library. It
switches via `LEARN_BACKEND` between `rspamd` (HTTP controller, `Password`
header, `/learnspam` and `/learnham`), `spamc` (SpamAssassin) and `log`
and maps the results onto the exit codes. A different classifier needs a
different program in the directory, nothing else.

The HTTP helper follows no redirects, so that the token is not passed on to
other targets. 208 and a confirmed "already learned" count as
success; transport errors, 408, 429 and 5xx are retried. The alternative
`spamc` path disables the safe fallback with `-x`; it is not installed in
the standard image and has been verified through its invocation and exit-code contract.

When upgrading from 0.10.1, the program names in the production profile
remain **spam** and **ham**. They correspond to the stored `Action` values
of old queue entries and are also understood by old workers during the
rolling upgrade. Program names remain freely choosable in general, but
while jobs still exist they must not be removed or renamed without a
transition. Go contains no special handling for classifiers here.
Viper normalizes YAML keys; `env` keys are therefore uppercased before
validation. Their values stay unchanged.

## User scripts

ManageSieve PUTSCRIPT stores the script. SETMETADATA assigns it; SETACTIVE still concerns delivery only and rejects scripts with `require "imapsieve"`.

```text
SETMETADATA INBOX ("/shared/imapsieve/script" "inbox-events")
```

An existing mailbox entry wins over the shared server entry in the reserved namespace `~server`. A mailbox entry that is empty or points to a missing script disables the run without a fallback. Shared server metadata is read-only for users. On shared mailboxes, assigning a script requires the administration right `a`; the script comes from the owner's storage. Its actions remain bounded by the rights of the user who triggered it. Cross-owner fileinto and outgoing messages from other people's mailboxes are rejected.

Example:

```sieve
require ["imapsieve", "environment", "imap4flags"];
if allof (environment :is "imap.cause" "FLAG",
          environment :contains "imap.changedflags" "\\seen") {
    addflag "processed";
}
```

`fileinto` currently requires the target mailbox to exist already; `:create` does not create a mailbox in the IMAP path. `keep` retains the original; `fileinto` creates an additional message and, without keep, marks the original as Deleted. `discard` marks the original as Deleted. There is no automatic EXPUNGE. Header changes affect only subsequent copies and forwards. Redirect and mailto notify use the configured SMTP relay with loop protection; they require a valid owner address. Reject, ereject and vacation are not permitted in the IMAP context; envelope tests raise a runtime error. Eight actions and the configured time limit bound a single run. FLAG scripts do not trigger further FLAG scripts; fileinto does not produce a COPY event.

Script errors leave the original IMAP change in place. With competing flags, the later client change wins. Target copies are durably committed before the original is possibly marked as deleted. A crash in that window can leave an additional copy behind; it removes no remaining copy of a message. On an ambiguous storage error, committed target copies are not rolled back.

## Durable learning queue

Learning is an operator rule and independent of user scripts; every
rule names the `extprograms` entry that receives the message. COPY/MOVE within one's own account are supported; APPEND/imports and cross-owner operations do not train. The first matching rule wins; special-use assignments are independent of translated folder names.

Before the Ceph link, an independent copy of the content is persisted in the data pool namespace `~learning`. A commit receipt is written atomically together with the target entry, even across higher index shards. Only then can the worker send. EXPUNGE leaves the payload untouched. Link and mailbox DELETE coordinate through a Ceph lease; before the deletion, commit receipts that are still needed are transferred into the queue.

Exactly one worker holds the cluster-wide learning lease. Failed attempts are retried with backoff. The selection prioritizes the oldest due entries. After a successful response the final state is stored, and receipt and payload are removed afterwards. Exit 75 from the program leads to a retry with backoff, any other failure ends the job permanently. A crash between the program accepting the message and the local confirmation can cause a repeated invocation: the transport offers at-least-once delivery, not a distributed exactly-once guarantee. Jobs that have permanently failed or are still unconfirmed after 24 hours are kept for diagnosis; they are not silently removed as successful.

Messages above the size limit are not learned and are counted as `oversize`. An error while durably preparing the learning job prevents the corresponding link, so that no successfully confirmed learning operation exists without a stored job.

Metrics: `imap_imapsieve_events_total{phase=...}` and `imap_imapsieve_learning_total{result=...}`. No message texts, passwords or script errors containing message data are logged. Dead-letter jobs require a deliberate operator decision; there is no graphical queue management yet.

The learning path uses the same program table as the Sieve extensions; in the IMAP pod only the configured program runs, no shell.

Reference: RFC 6785, https://www.rfc-editor.org/rfc/rfc6785.html


## Resource limits of the Sieve interpreter (from 0.11.1)

A regular script run is limited to five seconds of elapsed time by
default. `sieve_max_cpu_time` in the Sieve settings accepts
positive Go durations such as `2s`; the limit measures elapsed time and
not CPU time alone. A shorter timeout from the caller wins.
The existing IMAPSieve setting `timeout_ms` therefore also applies during
expensive regex comparisons. On abort, execution does not continue in a
detached goroutine.

`foreverypart` may be nested four levels deep, includes counted.
The MIME tree is limited to 1000 parts, depth 20 and 256 KiB of headers
per part. If a limit is exceeded, script execution is aborted. LMTP
uses its existing error path with the original message in INBOX;
IMAPSieve discards the failed result and does not mark the message
as deleted. The message itself is not truncated in order to hide an
exceeded limit.

Details: [Sieve conformance fix report](reports/2026-09-16-sieve-conformance-fixes.md).


From 0.11.2 on, the rspamd helper accepts HTTP 204 and 208 as successful
responses without a JSON body. An empty HTTP 200 remains a temporary error.
HTTP 204 was actually observed during the production acceptance; the
previous version wrongly retried such already successfully answered
jobs until they ended up in the failed set.
