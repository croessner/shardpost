go-sieve
====================

Sieve email filtering language ([RFC 5228]) interpreter implementation in Go.

## Features

* Binary representation for faster load/execute cycles (`Script.Save`, `sieve.RestoreSaved`).
* Integration tests harness for MTA integration testing (see `tests/execute.go`).

## Supported extensions

The list `require` accepts (`supportedRequires` in `interp/load.go`).
Actions with side effects (vacation replies, notifications, rejection
messages, `:fcc` copies, pipes) are only recorded; carrying them out is
the caller's job.

- envelope, fileinto, encoded-character ([RFC 5228])
- comparator-i;octet, comparator-i;ascii-casemap ([RFC 4790]),
  comparator-i;ascii-numeric ([RFC 4790]), comparator-i;unicode-casemap ([RFC 5051])
- imap4flags ([RFC 5232])
- variables ([RFC 5229])
- relational ([RFC 5231])
- copy ([RFC 3894])
- reject, ereject ([RFC 5429])
- subaddress ([RFC 5233])
- environment ([RFC 5183])
- body ([RFC 5173])
- vacation ([RFC 5230]), vacation-seconds ([RFC 6131]); the period is
  clamped by `sieve_vacation_min_period`, `sieve_vacation_max_period` and
  `sieve_vacation_default_period` (`VacationPeriod`)
- date, index ([RFC 5260])
- duplicate ([RFC 7352])
- editheader ([RFC 5293])
- mailbox, mboxmetadata, servermetadata ([RFC 5490])
- ihave ([RFC 5463])
- include ([RFC 6609])
- regex (draft-ietf-sieve-regex)
- spamtest, spamtestplus, virustest ([RFC 5235])
- special-use ([RFC 8579])
- enotify ([RFC 5435]) with the mailto method ([RFC 5436])
- foreverypart, mime, extracttext ([RFC 5703]; `replace` and `enclose` are not supported)
- fcc ([RFC 8580]): `:fcc` with `:create`, `:flags` and `:specialuse` for vacation and notify
- imapsieve ([RFC 6785]), only when the caller enables it (`imapsieve_enabled`)
- `vnd.dovecot.debug`
- `vnd.dovecot.execute`, `vnd.dovecot.pipe`, `vnd.dovecot.filter` through a caller-provided program runner (no process execution in the interpreter).
- `vnd.dovecot.testsuite`, only in a test environment (`Options.T`), for the Pigeonhole test suite.

## Example

See ./cmd/sieve-run.

## Known issues

- Some invalid scripts are accepted as valid (see tests/compile_test.go)
- Comments in addresses are not ignored when testing equality, etc.
- Source routes in addresses are not ignored when testing equality, etc.

[RFC 5228]: https://datatracker.ietf.org/doc/html/rfc5228
[RFC 5229]: https://datatracker.ietf.org/doc/html/rfc5229
[RFC 5232]: https://datatracker.ietf.org/doc/html/rfc5232
[RFC 5231]: https://datatracker.ietf.org/doc/html/rfc5231
[RFC 3894]: https://datatracker.ietf.org/doc/html/rfc3894
[RFC 5429]: https://datatracker.ietf.org/doc/html/rfc5429
[RFC 5233]: https://datatracker.ietf.org/doc/html/rfc5233
[RFC 5183]: https://datatracker.ietf.org/doc/html/rfc5183
[RFC 5173]: https://datatracker.ietf.org/doc/html/rfc5173
[RFC 4790]: https://datatracker.ietf.org/doc/html/rfc4790
[RFC 5051]: https://datatracker.ietf.org/doc/html/rfc5051
[RFC 5230]: https://datatracker.ietf.org/doc/html/rfc5230
[RFC 6131]: https://datatracker.ietf.org/doc/html/rfc6131
[RFC 5260]: https://datatracker.ietf.org/doc/html/rfc5260
[RFC 7352]: https://datatracker.ietf.org/doc/html/rfc7352
[RFC 5293]: https://datatracker.ietf.org/doc/html/rfc5293
[RFC 5490]: https://datatracker.ietf.org/doc/html/rfc5490
[RFC 5463]: https://datatracker.ietf.org/doc/html/rfc5463
[RFC 6609]: https://datatracker.ietf.org/doc/html/rfc6609
[RFC 5235]: https://datatracker.ietf.org/doc/html/rfc5235
[RFC 8579]: https://datatracker.ietf.org/doc/html/rfc8579
[RFC 5435]: https://datatracker.ietf.org/doc/html/rfc5435
[RFC 5436]: https://datatracker.ietf.org/doc/html/rfc5436
[RFC 5703]: https://datatracker.ietf.org/doc/html/rfc5703
[RFC 8580]: https://datatracker.ietf.org/doc/html/rfc8580
[RFC 6785]: https://datatracker.ietf.org/doc/html/rfc6785
