# Copyright and licence

ShardPost — Copyright © 2026 Christian Rößner

This program is free software: you can redistribute it and/or modify it
under the terms of the GNU General Public License as published by the
Free Software Foundation, either version 3 of the License, or (at your
option) any later version.

This program is distributed in the hope that it will be useful, but
WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU General
Public License for more details.

You should have received a copy of the GNU General Public License along
with this program. If not, see <https://www.gnu.org/licenses/>.

The full text is in [LICENSE](LICENSE).

## What the licence covers

**Every file of this repository is licensed under GPL-3.0-or-later,
whether or not it carries a licence header of its own.** A missing
header changes nothing: this file is the licence statement for the
repository as a whole. That covers the Go sources under `cmd/` and
`internal/`, the API description under `api/`, the deployment files
under `deploy/`, the manual pages and the documentation under `docs/`,
the Makefile and the Dockerfile.

The exceptions are the third-party code listed below, which keeps the
licence it came with. Each of those directories carries its own licence
file.

## Third-party code in this repository

| Path | Origin | Licence |
| --- | --- | --- |
| `vendor/` | the dependencies of `go.mod`, vendored | their own, see each directory |
| `third_party/go-sieve/` | [foxcpp/go-sieve](https://github.com/foxcpp/go-sieve), modified (see `third_party/go-sieve/README.md`) | MIT, `third_party/go-sieve/LICENSE` |
| `internal/inflate/` | `compress/flate` of the Go standard library, one behavioural change (see the package comment) | BSD-3-Clause, `internal/inflate/LICENSE` |

All three are compatible with GPL-3.0: MIT and BSD-3-Clause impose no
conditions that the GPL does not allow, and the vendored modules are
permissive (MIT, BSD, ISC, Apache-2.0) as well. Their notices must be
kept when this program is redistributed, which is what those licence
files are for.

One dependency is itself copyleft: `github.com/croessner/nauthilus/v4`
(GPL-3.0), of which this program uses the gRPC authentication contract.
Licensing ShardPost under GPL-3.0 is what makes that combination
distributable.

## Libraries this program links at runtime

The container image ships the Ceph client libraries (`librados2`,
`libceph-common`), which are under the **LGPL** (v2.1 or later, or v3).
They are linked dynamically, so they can be replaced by another build of
the same library, and their own licence and source are those of the
[Ceph project](https://github.com/ceph/ceph). The image carries the
package metadata that states this.

## Applying the licence to a new file

A header is not required — see above — but it is welcome, and it is what
the GPL itself recommends:

```go
// Copyright (C) 2026 Christian Rößner
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
```

Or, shorter, the SPDX form that tools understand:

```go
// SPDX-FileCopyrightText: 2026 Christian Rößner
// SPDX-License-Identifier: GPL-3.0-or-later
```

A file that belongs to one of the third-party directories above keeps
the header it has; do not replace it with this one.
