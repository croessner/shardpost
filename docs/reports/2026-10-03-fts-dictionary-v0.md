# Full-text dictionary of the largest account: V0

Date: 2026-10-03

## Finding

After the switch to full-text layout 3, Ceph reported large omap objects
in `shardpost-index`: the dictionary objects of the largest account
(36 297 messages) held about 320 000 keys each (`d0` 322 137, `d24`
319 224) at the cap of 32 dictionary objects (`fts.shards_max`, which
caps postings and dictionary alike). The account's dictionary has about
10.1 million keys, the postings about 2.1 million.

## V0 (configuration only, image 0.48.0)

- `fts.shards_max: 128` in the croessner overlay, rolled out (pod
  restart).
- `shardpostctl plan -due-only fts_layout de10000@srvint.net` (target
  32/128/1), applied at 06:54:19 UTC; the job succeeded at 06:57:43 UTC
  (the account was locked for about 3.5 minutes; deliveries were
  deferred, JMAP answered 503 with `Retry-After`).
- Result: generation `6ef560a3d56f1968`, S/D/H 32/128/1; dictionary
  objects hold 71 000 to 92 000 keys (`d0` 76 618, `d64` 91 513, `d127`
  71 161), postings unchanged (`p0` 64 982).
- `ceph osd pool deep-scrub shardpost-index`: the `LARGE_OMAP_OBJECTS`
  warning cleared at 07:00 UTC.

A delivery into this account now touches 128 dictionary objects at most
instead of 32 (about 198 instead of 116 operations). Separate caps and
targets for postings and dictionary, and a reduction of what enters the
dictionary, follow as stages B14g and B14h (plan
`temp/13-fts-dictionary-large-accounts.md`).
