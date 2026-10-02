package cephbench

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/bench"
	"shardpost/internal/metrics"
)

// The inventory lists both bench pools across all namespaces (a listing
// is what the running server must not do; in the bench pools it is the
// point) and reads the size and the omap of every object of the bench
// accounts and of the store's own namespaces.

// listSite is the label of the inventory's listings in
// imapceph_rados_listings_total.
const listSite = "bench_inventory"

// regMsgsKey is the registry key of an account's message count.
const regMsgsKey = "q:msgs"

// omapPage is the page size of the omap reads.
const omapPage = 1000

// inventoryJob is one object to measure.
type inventoryJob struct {
	pool, ns, oid string
	index         bool
}

// inventory collects the usage.
type inventory struct {
	prefix string
	every  int

	mu        sync.Mutex
	accounts  map[string]*bench.AccountUsage
	storeWide map[string]bench.PoolUsage
	ec        bench.ECEstimate
	skipped   map[string]bool
	errs      []error
}

// InventoryResult is what the inventory found.
type InventoryResult struct {
	Accounts  []bench.AccountUsage
	StoreWide map[string]bench.PoolUsage
	DataRaw   bench.ECEstimate
	// Skipped are namespaces that are neither a sampled bench account
	// of the prefix nor the store's own (other prefixes, accounts
	// outside the sample).
	Skipped int
}

// selected reports whether an account namespace is in the sample: its
// number is a multiple of every.
func (inv *inventory) selected(ns string) bool {
	if !bench.IsAccount(ns, inv.prefix) {
		return false
	}

	n, err := strconv.Atoi(strings.TrimPrefix(ns, inv.prefix))

	return err == nil && n%inv.every == 0
}

// takeInventory measures the bench accounts of prefix (every every-th
// by number) and the store's own namespaces in the pools (the system
// pool only when one is given).
func takeInventory(conn *rados.Conn, index, data, system, prefix string, every, parallel int, ec bench.ECEstimate) (*InventoryResult, error) {
	inv := &inventory{prefix: prefix, every: max(every, 1), accounts: map[string]*bench.AccountUsage{},
		storeWide: map[string]bench.PoolUsage{}, ec: ec, skipped: map[string]bool{}}
	jobs := make(chan inventoryJob, 1024)

	var wg sync.WaitGroup

	for range max(parallel, 1) {
		wg.Go(func() { inv.worker(conn, jobs) })
	}

	err := inv.list(conn, jobs, index, true)
	if err == nil {
		err = inv.list(conn, jobs, data, false)
	}

	if err == nil && system != "" {
		err = inv.list(conn, jobs, system, true)
	}

	close(jobs)
	wg.Wait()

	if err != nil {
		return nil, err
	}

	if len(inv.errs) > 0 {
		return nil, fmt.Errorf("inventory: %d objects could not be read, first: %w", len(inv.errs), inv.errs[0])
	}

	return inv.result(), nil
}

// list lists a pool and queues the objects to measure.
func (inv *inventory) list(conn *rados.Conn, jobs chan<- inventoryJob, pool string, index bool) error {
	ioctx, err := conn.OpenIOContext(pool)
	if err != nil {
		return fmt.Errorf("open pool %q: %w", pool, err)
	}
	defer ioctx.Destroy()

	ioctx.SetNamespace(rados.AllNamespaces)
	metrics.RadosListings.WithLabelValues(listSite).Inc()

	it, err := ioctx.Iter()
	if err != nil {
		return fmt.Errorf("list pool %q: %w", pool, err)
	}
	defer it.Close()

	for it.Next() {
		ns := it.Namespace()

		switch {
		case inv.selected(ns), storeNamespace(ns):
			jobs <- inventoryJob{pool: pool, ns: ns, oid: it.Value(), index: index}
		case ns == MarkerNamespace:
		default:
			inv.mu.Lock()
			inv.skipped[ns] = true
			inv.mu.Unlock()
		}
	}

	return it.Err()
}

// storeNamespace reports whether ns is one of the store's own
// namespaces (not the bench marker's).
func storeNamespace(ns string) bool {
	return strings.HasPrefix(ns, "~") && ns != MarkerNamespace
}

func (inv *inventory) worker(conn *rados.Conn, jobs <-chan inventoryJob) {
	ctxs := map[string]*rados.IOContext{}

	defer func() {
		for _, c := range ctxs {
			c.Destroy()
		}
	}()

	for j := range jobs {
		ioctx := ctxs[j.pool]
		if ioctx == nil {
			c, err := conn.OpenIOContext(j.pool)
			if err != nil {
				inv.fail(err)
				continue
			}

			ctxs[j.pool], ioctx = c, c
		}

		ioctx.SetNamespace(j.ns)
		inv.measure(ioctx, j)
	}
}

func (inv *inventory) fail(err error) {
	inv.mu.Lock()
	defer inv.mu.Unlock()

	inv.errs = append(inv.errs, err)
}

// measure reads one object's size and, in the index pool, its omap.
func (inv *inventory) measure(ioctx *rados.IOContext, j inventoryJob) {
	st, err := ioctx.Stat(j.oid)
	if err != nil {
		if errorCode(err) != syscall.ENOENT { // gone since the listing
			inv.fail(fmt.Errorf("stat %s/%s/%s: %w", j.pool, j.ns, j.oid, err))
		}

		return
	}

	u := bench.Usage{Objects: 1, Bytes: int64(st.Size)}
	msgs := int64(-1)

	if j.index {
		vals, err := ioctx.GetAllOmapValues(j.oid, "", "", omapPage)
		if err != nil && errorCode(err) != syscall.ENOENT {
			inv.fail(fmt.Errorf("omap %s/%s/%s: %w", j.pool, j.ns, j.oid, err))
			return
		}

		for k, v := range vals {
			u.OmapKeys++
			u.OmapBytes += int64(len(k) + len(v))
		}

		if v, ok := vals[regMsgsKey]; ok && bench.Classify(j.oid) == bench.ClassRegistry {
			msgs, _ = strconv.ParseInt(string(v), 10, 64)
		}
	}

	inv.add(j, u, msgs)
}

func (inv *inventory) add(j inventoryJob, u bench.Usage, msgs int64) {
	class := bench.Classify(j.oid)

	inv.mu.Lock()
	defer inv.mu.Unlock()

	if storeNamespace(j.ns) {
		if inv.storeWide[j.pool] == nil {
			inv.storeWide[j.pool] = bench.PoolUsage{}
		}

		inv.storeWide[j.pool].Add(class, u)

		return
	}

	a := inv.accounts[j.ns]
	if a == nil {
		a = &bench.AccountUsage{Account: j.ns, Pools: map[string]bench.PoolUsage{}}
		inv.accounts[j.ns] = a
	}

	if a.Pools[j.pool] == nil {
		a.Pools[j.pool] = bench.PoolUsage{}
	}

	a.Pools[j.pool].Add(class, u)

	if msgs >= 0 {
		a.Messages = msgs
	}

	if !j.index && class == bench.ClassMsg {
		inv.ec.Add(u.Bytes)
	}
}

func (inv *inventory) result() *InventoryResult {
	r := &InventoryResult{StoreWide: inv.storeWide, DataRaw: inv.ec, Skipped: len(inv.skipped)}

	for _, a := range inv.accounts {
		r.Accounts = append(r.Accounts, *a)
	}

	slices.SortFunc(r.Accounts, func(a, b bench.AccountUsage) int { return strings.Compare(a.Account, b.Account) })

	return r
}
