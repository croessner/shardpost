# Move of six accounts and the public folders to ShardPost

Date: 2026-09-30

## What moved

Six accounts of `srvint.net` that share the public folders of the
mailstack and each other's mailboxes, and those public folders:

| What | Mailboxes | Messages |
| --- | --- | --- |
| `lists@srvint.net` | 9 | 74 |
| `de10000@srvint.net` | 39 | 36078 |
| `system@srvint.net` | 10 | 1353 |
| `zabbix@srvint.net` | 4 | 1301 |
| `de10043@srvint.net` | 13 | 714 |
| `spamtrap@srvint.net` | 5 | 347 |
| `Mailinglists/` → `Public/Mailinglists/…` | 18 | 17482 |
| `Zabbix/` → `Public/Zabbix` | 1 | 10712 |
| `Postfix/` → `Public/Postfix` | 1 | 1604 |

`de10021@srvint.net` stays on the mailstack and no longer sees `Zabbix`
and `Postfix`; the owner accepted that.

What the mailstack did with groups and a global script is done here as
follows:

- **Rights.** Dovecot granted them through `acl_groups`. Here they are
  entries per account: `lists` and `de10000` hold `lrswipkxtea` on every
  public folder, `zabbix` holds `lrswipkxte` on `Public/Zabbix`, `system`
  on `Public/Postfix`.
- **Filing.** The `sieve_after` script `public-mls.sieve` became
  `sieve-after/50-public-folders.sieve` in the overlay
  (`sieve.after_dir`), with the targets below `Public/`.
- **Reading state.** The messages carry the `\Seen` of `lists`; the
  other readers got their own state as private marks (`de10000`: 17334
  messages in the list folders, 10547 in `Zabbix`, 1599 in `Postfix`).
- **Shares.** The ACL entries of `lists`, `system`, `de10043` and
  `spamtrap` for `de10000` were copied; `de10000` sees them under
  `Shared/<account>/…` instead of `Shared/srvint.net/<name>/…`.

## How

1. 0.34.0 added the two missing pieces (global Sieve scripts, import of
   a public namespace); the live copy then found five defects in the
   import and the full-text index, fixed in 0.34.1 to 0.34.6 and 0.35.0
   (see the reports of that day).
2. Copy with the routing unchanged, then a comparison with the source:
   every folder of the six accounts over IMAP against `doveadm mailbox
   status` (messages, UIDVALIDITY, UIDNEXT, HIGHESTMODSEQ, unseen — the
   unseen counts of the public folders per reader), the Sieve scripts
   byte for byte, dry runs of the Sieve sequence with test messages, a
   scrub of all seven namespaces.
3. Cutover at 16:30 UTC: `users hold set` and `users kick` for the six
   accounts, the delta for accounts and public folders (64 seconds, all
   mailboxes verified), the pins in `director.lua` applied to the shadow
   and then the production Nauthilus, a second delta that found nothing,
   `users move --to-shard shardpost --strategy kick_existing`, route
   lookups, `users hold clear` at 16:39 UTC.

The hold came before the pins, not after them as in the procedure of
0.17.0: an account without a retained binding would otherwise have its
next delivery routed to ShardPost before the last delta, and that delta
removes what the source does not have.

## State after the cutover

- IMAP route lookups select `shardpost-imap` for all six; an LMTP
  recipient probe through the director selects `shardpost-lmtp`.
- The first client logins of `de10000` arrived at ShardPost; the
  mailstack logged nothing for the six accounts after the move.
- The data on the mailstack is unchanged. A way back is a re-route; mail
  that arrived here in the meantime would have to be copied back.

## Open

- `ListMailboxesAfter` and `ListSharedN` read one omap page per call.
- An address search (`FROM`) with many hits took ten seconds on the test
  copy.
- The unit tests of the dictionary's key-range scan do not run against a
  store.
