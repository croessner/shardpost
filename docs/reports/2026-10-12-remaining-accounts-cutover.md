# Move of the remaining mailstack accounts and the default shard

Date: 2026-10-12

## What moved

Every account that still had mail on the mailstack (Dovecot) moved to
ShardPost with `shardpostctl migrate` on 0.52.0, keeping UIDVALIDITY,
UIDs and MODSEQ. Afterwards the Nauthilus routing default
(`DEFAULT_SHARD` in `director.lua`) changed from `mailstack` to
`shardpost`, so every account without an entry now lands on ShardPost.

| Group | Accounts | Mailboxes | Messages | Cutover (UTC) |
| --- | --- | --- | --- | --- |
| 1 | 16 with data (`admin`, `de10002`, `de10019`, `de10024`, `de10027`, `de10029`, `de10034`, `de10035`, `de10044`, `de10045`, `de10046`, `de10049`, `de88888`, `grafana`, `help` @srvint.net, `dkim2-reflector@exampleserver.de`) | 110 | 1,872 | 09:25–09:34 |
| 1 | 5 routed only (`de20005`, `de20006`, `de20007`, `master-01`, `support` @srvint.net) | – | – | 09:31–09:33 |
| 2 | 9 (`de10008`, `de10017`, `de10021`, `de10022`, `de10023`, `de10025`, `de10026`, `de10036`, `de10037` @srvint.net) | 192 | 13,114 | 09:36–09:45 |
| Default | every other account | – | – | 09:56 |

Sieve scripts came along: 7 accounts with an active script, among them
vacation (`de10021`, `de10022`, `help`), `fileinto` (`de10017`,
`de10025`) and a `redirect :copy` (`de10049`). ShardPost relays redirects
and vacation replies through `relay.roessner-net.de:25025`.

## Decisions

- **Two groups.** The law-office accounts (`de10021`–`de10026`) and the
  other accounts with regular use moved in the second group, after the
  first had gone through cleanly. `de10008` sees mailboxes of `de10017`,
  so both moved together.
- **Accounts without IMAP access.** `de20005`, `de20006`, `de20007`,
  `master-01` and `support` are not in the Dovecot group in LDAP, so
  Nauthilus refuses even the master login and the import cannot read
  them. Only their routing moved. `de20005` leaves two messages from
  2025 on the mailstack; the other four are empty.
- **Not moved.** Directories on the mailstack without an account in the
  userdb (`de10031`, `de30000`, `nextcloud`, `oidc-e2e-20260816`,
  `pilot-mail-migration@exampleserver.de`) stay where they are. The
  2,259 directories of the load-test accounts `benchNNNNN@exampleserver.de`
  (28 MB of test mail from rns-server `bench/`) were not copied; those
  accounts route to ShardPost through the default and start empty
  there.
- **`de10021` and the public folders.** On the mailstack the account
  saw `Zabbix` and `Postfix` through `acl_groups`, unsubscribed and with
  every message unseen. It now holds `lrswipkxte` on `Public/Zabbix` and
  `Public/Postfix` in `~public`, like `zabbix` and `system`, again
  without subscription or seen marks. The public namespace was not
  imported again: a new `migrate '~public'` would remove what ShardPost
  received there since 2026-09-30.

## How

1. Pre-copy of all 25 readable accounts with the routing unchanged
   (about 22 minutes in total, `de10026` with 1,257 messages and 1 GB
   the longest), then a comparison against `doveadm mailbox status`:
   every mailbox present, messages, UIDVALIDITY and UIDNEXT equal,
   every mailbox `verified`; Sieve script counts and active scripts
   equal.
2. Per group, as on 2026-09-30: `users hold set` (15 min) and
   `users kick`, a delta, the entries in `director.lua` applied to the
   shadow and then the production Nauthilus, a second delta (it found
   nothing for either group), `users move --to-shard shardpost
   --strategy kick_existing`, route lookups, `users hold clear`. Group 2
   got a warm-up delta without a hold first, which kept its hold window
   at nine minutes.
3. After both groups: `DEFAULT_SHARD = "shardpost"`, the routing test
   adjusted to expect ShardPost for unpinned accounts as well, shadow,
   then production (nauthilus-tests `6bda4c9`, not pushed).

## Checks after the cutover

- Nauthilus logs `director_routing_mailShard = shardpost` for every
  account it routed after the switch; none for `mailstack`.
- An LMTP recipient probe through the director accepted `de10021`,
  `bench06478@exampleserver.de`, `de10022` and `de10000` in one
  transaction. Before the default changed, the same probe with a
  group-1 account and `de10021` (still on the mailstack) answered `451
  4.3.2 Recipient must be retried separately`, as it should across
  shards.
- The director keeps no user with runtime state on a mailstack backend.
- The first client logins of the moved accounts and the first
  deliveries arrived at ShardPost, among them a delivery to `de10017`
  that its imported Sieve script filed into `iTunes`. ShardPost logged
  no warning or error in the 20 minutes after the cutover.
- The Postfix queues (mx, egress, relay, submission, srsmail-native)
  were empty afterwards.
- The mailstack logged no new login or delivery for the moved accounts
  after their holds. Three IMAP sessions from before the kick
  (`de10022`, `de10025`, `de10036`, all IDLE or SELECT) closed by
  09:48 without deleting or expunging anything.

## Route lookups

`route lookup --user` answered several of the moved accounts with
`HTTP 503: … backend node shard mismatch` instead of a backend. A lookup
by user does not see the `mailShard` Nauthilus sets, as already noted on
2026-09-30, so it is no evidence about real routing; the probes above
are.

## Open

- The mailstack still holds every mailbox unchanged. A way back is a
  re-route; mail delivered to ShardPost in the meantime would have to be
  copied back.
- The `ACCOUNT_SHARD_OVERRIDES` entries for `shardpost` are redundant
  now that it is the default; they stay as a record and can go.
- The load-test tooling in rns-server `bench/` now meets ShardPost for
  the bench accounts, not the mailstack.
