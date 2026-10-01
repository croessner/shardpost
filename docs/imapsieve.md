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

ManageSieve PUTSCRIPT stores the script. SETMETADATA assigns it; SETACTIVE still concerns delivery only and rejects scripts with `require "imapsieve"`. JMAP `SieveScript/set` stores and activates the same way (see [JMAP](jmap.md#sieve-scripts)): a script with `require "imapsieve"` is stored, and `onSuccessActivateScript` leaves it inactive.

The script name `jmap-vacation` is reserved for the JMAP VacationResponse
(see [JMAP](jmap.md#vacation)): it is the server's copy of that setting,
for display. LISTSCRIPTS lists it and GETSCRIPT returns it, so a client
such as Roundcube can show the setting, but PUTSCRIPT, DELETESCRIPT,
RENAMESCRIPT (to or from it) and SETACTIVE answer `NO` for it. It changes
over JMAP only, is never the active script, and delivery does not run
it: the vacation is answered from the setting in the account's metadata,
whose entries under `/private/vendor/shardpost/jmap/` SETMETADATA refuses
(see [JMAP](jmap.md#server-managed-metadata)).

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

`notify :fcc` (RFC 8580) works in IMAPSieve scripts too: once the
notification went out, its copy is stored in the owner's mailbox. As
for `fileinto` in this context, that mailbox must exist (`:create` does
not create it) and lie in the owner's namespace; `:flags` and
`:specialuse` apply. A copy that cannot be stored is logged and does not
fail the run. `vacation`, and with it `vacation :fcc`, is not available
here.

Script errors leave the original IMAP change in place. With competing flags, the later client change wins. Target copies are durably committed before the original is possibly marked as deleted. A crash in that window can leave an additional copy behind; it removes no remaining copy of a message. On an ambiguous storage error, committed target copies are not rolled back.

## Sieve during delivery

LMTP and the local delivery API (the LDA, `shardpostctl deliver`) share
one delivery path, so everything below holds for both. The Sieve
settings are `key=value` pairs separated by `;` in `sieve.settings`
(`SIEVE_SETTINGS`), with Pigeonhole's key names where Pigeonhole has
the setting:

```yaml
sieve:
  settings: "sieve_vacation_min_period=1d;sieve_vacation_max_period=60d;sieve_vacation_default_period=7d;sieve_reject_mode=mdn"
```

A setting that cannot work — a period that does not parse, periods that
contradict each other, an unknown `sieve_reject_mode`, a
`postmaster_address` that is no address — is a startup error and fails
`config check`.

### Delivery headers

Every delivered copy gets `Return-Path: <envelope sender>` (`<>` for
the null sender) and `Delivered-To: <envelope recipient>` on top, the
way Dovecot's LMTP writes them. The order is:

```text
Return-Path: <sender@example.net>
Delivered-To: user@example.org
Received: from 192.0.2.25:41234
	by mail.example.org with LMTP
	for <user@example.org>; Thu, 01 Oct 2026 10:00:00 +0000
(the message as it arrived)
```

Return-Path fields the message brought along are removed first: only the
final delivery sets one (RFC 5321 4.4), so any other cannot be trusted.
`Delivered-To` fields already in the message stay; MTAs use them to
detect forwarding loops. Both headers are added before Sieve runs, so a
script sees them (`header :is "Return-Path" "<sender@example.net>"`);
the `envelope` test is unchanged. `editheader` cannot add or delete
either of them (`sieve_editheader_header`, see the defaults). The
recipient in `Delivered-To` is the address of `RCPT TO` (LMTP) or the
`recipient` parameter (LDA) as given, not the canonical account an
alias resolved to.

| Setting | Default | Meaning |
| --- | --- | --- |
| `server.lmtp_return_path` (`LMTP_RETURN_PATH`) | `true` | Add `Return-Path` and remove the ones the message brought along. `false` stores the message without a Return-Path of ours and leaves its own fields alone. |
| `server.lmtp_delivered_to` (`LMTP_DELIVERED_TO`) | `final` | `final` adds `Delivered-To` with the envelope recipient (Dovecot's `lmtp_hdr_delivery_address = final`), `none` adds none. Dovecot's `original` (the `ORCPT` address) has no counterpart: LMTP does not offer DSN, so no `ORCPT` arrives. |

### Vacation period

`vacation` answers a sender once per period (RFC 5230 4.1, `:seconds`
from RFC 6131). The period of a script is clamped into the configured
range, the way Pigeonhole does it:

| Setting | Default | Meaning |
| --- | --- | --- |
| `sieve_vacation_min_period` | `1d` | The shortest period a script can ask for; `:days 0` becomes this. `0` lets a script ask for `0` (`:days 0`, `:seconds 0`), which means a reply to every message: nothing is recorded. |
| `sieve_vacation_max_period` | `60d` | The longest period; `0` means no maximum. |
| `sieve_vacation_default_period` | `7d` | The period without `:days` and `:seconds`; it must lie between minimum and maximum. |

Periods are written the way Dovecot writes them: a number with an
optional unit (`s`, `m`, `h`, `d`, `w` or their long forms such as
`days`; a bare number is seconds), or a Go duration such as `36h`. Up to
0.35.2, `:days 0` and `:seconds 0` silently meant seven days.

### Reject and ereject

`ereject` (RFC 5429 2.2) refuses the message in the protocol: LMTP
answers `550 5.7.1` with the reason, the local delivery API `422`
(`rejected`). `reject` (RFC 5429 2.1) accepts the message instead,
stores nothing and sends a rejection message to the envelope sender: an
MDN (RFC 8098) with the null sender, `Auto-Submitted: auto-replied
(rejected)`, `multipart/report; report-type=disposition-notification`
with three parts — the reason as text, the disposition
`automatic-action/MDN-sent-automatically; deleted` with `Final-Recipient`
and `Original-Message-ID`, and the header of the original message (not
its body, as `text/rfc822-headers`). The message then counts as
delivered (LMTP `250`, the API `200` with nothing stored).

Mail from the null sender and automatic mail (`Auto-Submitted` other
than `no`) get no rejection message — that would answer a bounce or an
auto-reply; the message is discarded silently. Without an SMTP relay
(`smtp.addr`), or when the relay does not take the rejection message,
`reject` falls back to the protocol-level rejection, so the sender still
learns of it.

| Setting | Default | Meaning |
| --- | --- | --- |
| `sieve_reject_mode` | `mdn` | `mdn` as described; `protocol` makes `reject` refuse in the protocol like `ereject` (the behaviour up to 0.35.2). A ShardPost setting: Pigeonhole leaves this to the delivery agent (`dovecot-lda -e`). |
| `rejection_subject` | `Rejected: %s` | Subject of the rejection message (Dovecot's setting). `%s` is the original subject. |
| `rejection_reason` | `Your message to <%t> was automatically rejected:%n%r` | Text of the rejection message (Dovecot's setting). `%t` is the recipient, `%r` the reason of the script, `%n` a line break, `%%` a percent sign. |
| `postmaster_address` | `postmaster@%d` | Sender address of rejection messages and of notifications without `:from` (Dovecot's setting); `%d` is the recipient's domain. |

### Copies of generated messages (fcc)

`vacation` and `notify` accept `:fcc "mailbox"` (RFC 8580, `require
"fcc"`): the reply or notification is also stored in that mailbox of
the recipient once it was sent — a suppressed reply (within the period,
to a list, …) stores nothing either. The optional companions are
`:create` (needs `mailbox`) to create a missing mailbox, `:flags` (needs
`imap4flags`) for the flags of the copy, and `:specialuse` (needs
`special-use`), whose mailbox wins over the name when there is one.
They are only allowed together with `:fcc`.

```sieve
require ["vacation", "fcc", "mailbox", "imap4flags"];
vacation :days 7 :fcc "Sent" :create :flags "\\Seen" :subject "Away" "Back on Monday.";
```

The copy is stored like any message, under the account's quota.
Without `:create` a missing mailbox gets no copy; a public folder (under
`server.public_prefix`) needs the post right and must exist, a mailbox
of another account is refused. A copy that cannot be stored is logged
and never fails the delivery. The JMAP VacationResponse stores no copy.

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
