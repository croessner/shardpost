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
	"shardpost/internal/model"
)

// The marker: an object in a namespace of its own in both bench pools,
// written by the first populate. measure refuses pools without it, and
// populate refuses an index pool without it that holds an account
// directory (a pool some server uses).
const (
	MarkerNamespace = "~bench"
	markerOID       = "marker"
	directoryHead   = "ix.accounts/head"
)

// ErrNotBenchPool is returned for a pool without the bench marker.
var ErrNotBenchPool = errors.New("not a bench pool")

// checkConfig verifies the guard and that the store configuration uses
// exactly the pools the guard checked.
func checkConfig(g bench.Guard, cfg cephstore.Config) error {
	if err := g.Check(); err != nil {
		return err
	}

	if cfg.IndexPool != g.BenchIndex || cfg.DataPool != g.BenchData {
		return fmt.Errorf("%w: the store is configured for %q/%q, not for the bench pools %q/%q",
			bench.ErrGuard, cfg.IndexPool, cfg.DataPool, g.BenchIndex, g.BenchData)
	}

	return nil
}

// checkPools verifies that both pools exist and carry the marker. With
// create a missing marker is written, unless the index pool holds an
// account directory without one.
func checkPools(s *cephstore.Store, index, data, prefix string, create bool) error {
	return s.WithConn(func(conn *rados.Conn) error {
		marked := 0

		for _, pool := range []string{index, data} {
			ok, err := hasMarker(conn, pool)
			if err != nil {
				return err
			}

			if ok {
				marked++
			}
		}

		if marked == 2 {
			return nil
		}

		if !create {
			return fmt.Errorf("%w: %s/%s carry no bench marker (run bench populate first)", ErrNotBenchPool, index, data)
		}

		if used, err := hasDirectory(conn, index); err != nil || used {
			if err == nil {
				err = fmt.Errorf("%w: %s holds an account directory but no bench marker; it is in use", ErrNotBenchPool, index)
			}

			return err
		}

		return writeMarkers(conn, prefix, index, data)
	})
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

func hasDirectory(conn *rados.Conn, pool string) (bool, error) {
	return exists(conn, pool, model.AccountsNamespace, directoryHead)
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
