// Package bench measures what ShardPost costs per account in Ceph
// (scaling plan, stage R4): it fills separate bench pools with accounts
// and messages through the same delivery path LMTP uses, and reports
// objects, bytes and RADOS operations by object class.
//
// This package holds what needs no cluster: the safety guard, the
// message size distribution and generator, the population driver
// (against any store), the latency histogram, the object classes and
// the parser of the client library's objecter log. The cluster side is
// in internal/bench/cephbench.
package bench

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"shardpost/internal/model"
)

// RequiredPrefix is what every bench account name starts with. Nothing
// the bench writes can then be taken for a real account, even in a pool
// that was used for something else.
const RequiredPrefix = "bench-"

// ErrGuard is the error of every refusal of the guard.
var ErrGuard = errors.New("bench guard")

// Guard holds what the bench refuses to run without: bench pools named
// explicitly and different from the production pools of the
// configuration, and an account prefix starting with RequiredPrefix.
type Guard struct {
	// ProdIndex and ProdData are ceph.index_pool and ceph.data_pool of
	// the configuration (the production pools).
	ProdIndex, ProdData string
	// Reserved are further pools the bench never uses: the built-in
	// defaults of ceph.index_pool and ceph.data_pool, which a server
	// uses when its configuration names none.
	Reserved []string
	// BenchIndex and BenchData are the pools given on the command line.
	BenchIndex, BenchData string
	// Prefix is the account name prefix.
	Prefix string
}

// Check returns nil when the bench may run, else an error wrapping
// ErrGuard that says why not.
func (g Guard) Check() error {
	if g.BenchIndex == "" || g.BenchData == "" {
		return fmt.Errorf("%w: the bench pools must be given explicitly (--index-pool, --data-pool)", ErrGuard)
	}

	prod := append([]string{g.ProdIndex, g.ProdData}, g.Reserved...)

	for _, bench := range []string{g.BenchIndex, g.BenchData} {
		if slices.Contains(prod, bench) {
			return fmt.Errorf("%w: pool %q is a production pool (configured or the default)", ErrGuard, bench)
		}
	}

	if g.BenchIndex == g.BenchData {
		return fmt.Errorf("%w: the index and the data pool must differ (omap needs a replicated pool, the data pool is erasure coded in production)", ErrGuard)
	}

	return CheckPrefix(g.Prefix)
}

// CheckPrefix refuses a prefix that does not start with RequiredPrefix or
// that cannot start an account name.
func CheckPrefix(prefix string) error {
	if !strings.HasPrefix(prefix, RequiredPrefix) {
		return fmt.Errorf("%w: the account prefix %q does not start with %q", ErrGuard, prefix, RequiredPrefix)
	}

	if strings.ContainsAny(prefix, "/@ \t\r\n") {
		return fmt.Errorf("%w: the account prefix %q contains a character an account name cannot have", ErrGuard, prefix)
	}

	return CheckAccount(AccountName(prefix, 0), prefix)
}

// CheckAccount refuses an account name outside the bench: one that does
// not start with prefix (and so with RequiredPrefix) or that the store
// would refuse.
func CheckAccount(name, prefix string) error {
	if !strings.HasPrefix(prefix, RequiredPrefix) || !strings.HasPrefix(name, prefix) {
		return fmt.Errorf("%w: account %q is not a bench account (prefix %q)", ErrGuard, name, prefix)
	}

	if err := model.CheckNamespace(name); err != nil {
		return fmt.Errorf("%w: %w", ErrGuard, err)
	}

	return nil
}

// AccountName is the name of bench account i.
func AccountName(prefix string, i int) string { return fmt.Sprintf("%s%06d", prefix, i) }

// IsAccount reports whether a namespace is a bench account of prefix:
// the prefix followed by digits only.
func IsAccount(ns, prefix string) bool {
	rest, ok := strings.CutPrefix(ns, prefix)
	if !ok || rest == "" || !strings.HasPrefix(prefix, RequiredPrefix) {
		return false
	}

	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}
