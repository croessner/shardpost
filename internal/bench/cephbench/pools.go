// Package cephbench is the cluster side of the bench (internal/bench):
// it checks and marks the bench pools, populates them through the
// RADOS store, takes the inventory of the bench accounts and counts the
// operations of deliveries and account opens with the client library's
// objecter log.
package cephbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/bench"
	"shardpost/internal/cephstore"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// The marker: an object in a namespace of its own in both bench pools,
// written by the first populate into each pool that is still empty.
// measure refuses pools without it, and populate refuses a pool without
// it that holds anything (a pool some server uses).
const (
	MarkerNamespace = "~bench"
	markerOID       = "marker"
	directoryHead   = "ix.accounts/head"
	// fenceOID is the placement fence of an index pool whose system
	// state lives in a system pool (cephstore.FenceOID).
	fenceOID = cephstore.FenceOID
	// guardSite labels the guard's listing (one object at most) in
	// imapceph_rados_listings_total.
	guardSite = "bench_guard"
)

// ErrNotBenchPool is returned for a pool without the bench marker.
var ErrNotBenchPool = errors.New("not a bench pool")

// checkConfig verifies the guard and that the store configuration uses
// exactly the pools the guard checked, and that the store was opened as
// a tool (it checks the placement itself, once the pools are set up).
func checkConfig(g bench.Guard, cfg cephstore.Config) error {
	if err := g.Check(); err != nil {
		return err
	}

	if cfg.IndexPool != g.BenchIndex || cfg.DataPool != g.BenchData || cfg.SystemPool != g.BenchSystem {
		return fmt.Errorf("%w: the store is configured for %q/%q/%q, not for the bench pools %q/%q/%q",
			bench.ErrGuard, cfg.IndexPool, cfg.DataPool, cfg.SystemPool, g.BenchIndex, g.BenchData, g.BenchSystem)
	}

	if !cfg.PlacementTool {
		return fmt.Errorf("%w: the bench store must be opened in tool mode", bench.ErrGuard)
	}

	return nil
}

// Pool kinds of checkPool.
const (
	poolIndex = iota
	poolData
	poolSystem
)

// checkPools verifies that the pools exist and carry the marker, pool
// by pool (the system pool only when one is given). With create a
// missing marker is written into a pool that is empty; a pool without
// marker that holds any object (an index or system pool with an account
// directory or a placement fence in particular) is refused: some server
// or other tool uses it.
func checkPools(s *cephstore.Store, index, data, system, prefix string, create bool) error {
	return s.WithConn(func(conn *rados.Conn) error {
		pools := []struct {
			name string
			kind int
		}{{index, poolIndex}, {data, poolData}}

		if system != "" {
			pools = append(pools, struct {
				name string
				kind int
			}{system, poolSystem})
		}

		for _, p := range pools {
			if err := checkPool(conn, p.name, p.kind, prefix, create); err != nil {
				return err
			}
		}

		return nil
	})
}

// checkPool checks (and with create marks) one pool.
func checkPool(conn *rados.Conn, pool string, kind int, prefix string, create bool) error {
	ok, err := hasMarker(conn, pool)
	if err != nil || ok {
		return err
	}

	if !create {
		return fmt.Errorf("%w: %s carries no bench marker (run bench populate first)", ErrNotBenchPool, pool)
	}

	if kind != poolData {
		if used, err := hasDirectory(conn, pool, kind); err != nil || used {
			if err == nil {
				err = fmt.Errorf("%w: %s holds an account directory or a placement fence but no bench marker; it is in use", ErrNotBenchPool, pool)
			}

			return err
		}
	}

	if used, err := hasObjects(conn, pool); err != nil || used {
		if err == nil {
			err = fmt.Errorf("%w: %s holds objects but no bench marker; it is in use", ErrNotBenchPool, pool)
		}

		return err
	}

	return writeMarkers(conn, prefix, pool)
}

// rbdInitObjects are the objects "rbd pool init" leaves in the default
// namespace of a new pool; Rook runs it for every CephBlockPool whose
// application is rbd (the default). They do not make a pool one in use.
// The objects of the store's own namespaces do: a pool that holds them
// without the bench marker belongs to a server.
var rbdInitObjects = map[string]bool{"rbd_info": true, "rbd_trash": true}

// hasObjects reports whether a pool holds any object, in any namespace,
// other than the objects of an RBD pool initialisation. The listing stops
// at the first such object, so it is cheap for a pool in use as well as
// for an empty one (whose placement groups are empty). The cluster's pool
// statistics would lag behind a pool that was just written to.
func hasObjects(conn *rados.Conn, pool string) (bool, error) {
	ioctx, err := openNS(conn, pool, rados.AllNamespaces)
	if err != nil {
		return false, err
	}
	defer ioctx.Destroy()

	metrics.RadosListings.WithLabelValues(guardSite).Inc()

	it, err := ioctx.Iter()
	if err != nil {
		return false, fmt.Errorf("list pool %q: %w", pool, err)
	}
	defer it.Close()

	for it.Next() {
		if it.Namespace() == "" && rbdInitObjects[it.Value()] {
			continue
		}

		return true, nil
	}

	return false, it.Err()
}

func openNS(conn *rados.Conn, pool, ns string) (*rados.IOContext, error) {
	ioctx, err := conn.OpenIOContext(pool)
	if err != nil {
		return nil, fmt.Errorf("open pool %q (create the bench pools first): %w", pool, err)
	}

	ioctx.SetNamespace(ns)

	return ioctx, nil
}

func exists(conn *rados.Conn, pool, ns, oid string) (bool, error) {
	ioctx, err := openNS(conn, pool, ns)
	if err != nil {
		return false, err
	}
	defer ioctx.Destroy()

	if _, err := ioctx.Stat(oid); err != nil {
		if errorCode(err) == syscall.ENOENT {
			return false, nil
		}

		return false, fmt.Errorf("stat %s/%s/%s: %w", pool, ns, oid, err)
	}

	return true, nil
}

func hasMarker(conn *rados.Conn, pool string) (bool, error) {
	return exists(conn, pool, MarkerNamespace, markerOID)
}

// hasDirectory reports whether a pool shows that a server uses it: an
// account directory (index or system pool) or a placement fence (an
// index pool whose system state lives in a system pool).
func hasDirectory(conn *rados.Conn, pool string, kind int) (bool, error) {
	ok, err := exists(conn, pool, model.AccountsNamespace, directoryHead)
	if err != nil || ok || kind != poolIndex {
		return ok, err
	}

	return exists(conn, pool, model.AdminNamespace, fenceOID)
}

func writeMarkers(conn *rados.Conn, prefix string, pools ...string) error {
	b, _ := json.Marshal(map[string]string{"prefix": prefix, "created": time.Now().UTC().Format(time.RFC3339)})

	for _, pool := range pools {
		ioctx, err := openNS(conn, pool, MarkerNamespace)
		if err != nil {
			return err
		}

		err = ioctx.WriteFull(markerOID, b)
		ioctx.Destroy()

		if err != nil {
			return fmt.Errorf("write the bench marker into %q: %w", pool, err)
		}
	}

	return nil
}

// errorCode extracts the errno of a go-ceph error (0 if none).
func errorCode(err error) syscall.Errno {
	var ec interface{ ErrorCode() int }
	if errors.As(err, &ec) {
		return syscall.Errno(max(ec.ErrorCode(), -ec.ErrorCode()))
	}

	return 0
}

// poolIDs maps the ids of the pools to their names.
func poolIDs(conn *rados.Conn, pools ...string) (map[int64]string, error) {
	ids := map[int64]string{}

	for _, pool := range pools {
		id, err := conn.GetPoolByName(pool)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", pool, err)
		}

		ids[id] = pool
	}

	return ids, nil
}

// poolStats reads the cluster's statistics of the pools.
func poolStats(conn *rados.Conn, pools ...string) (map[string]bench.PoolStats, error) {
	out := map[string]bench.PoolStats{}

	for _, pool := range pools {
		ioctx, err := conn.OpenIOContext(pool)
		if err != nil {
			return nil, err
		}

		st, err := ioctx.GetPoolStats()
		ioctx.Destroy()

		if err != nil {
			return nil, fmt.Errorf("pool stats of %q: %w", pool, err)
		}

		out[pool] = bench.PoolStats{Objects: st.Num_objects, Bytes: st.Num_bytes, Reads: st.Num_rd, Writes: st.Num_wr}
	}

	return out, nil
}
