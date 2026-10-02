package cephstore

import (
	"sync"
	"sync/atomic"

	"github.com/ceph/go-ceph/rados"
)

// ioCache keeps a worker's contexts, one per slot (a worker reads from
// one namespace and writes into another at the same time).
type ioCache struct {
	t    *sysTool
	ctxs map[string]*rados.IOContext
}

func (t *sysTool) newIOCache() *ioCache { return &ioCache{t: t, ctxs: map[string]*rados.IOContext{}} }

// at returns the context of slot on pool, set to namespace ns.
func (w *ioCache) at(slot, pool, ns string) (*rados.IOContext, error) {
	key := slot + "\x00" + pool

	c := w.ctxs[key]
	if c == nil {
		var err error
		if c, err = w.t.openCtx(pool, ns); err != nil {
			return nil, err
		}

		w.ctxs[key] = c
	}

	c.SetNamespace(ns)

	return c, nil
}

func (w *ioCache) close() {
	for _, c := range w.ctxs {
		c.Destroy()
	}
}

// Slots of a worker's contexts.
const (
	slotSrc = "src"
	slotDst = "dst"
)

// eachParallel runs fn for every item on t.o.Parallel workers and returns
// the first error; once one failed, or the lease is no longer valid, no
// further item starts.
func eachParallel[T any](t *sysTool, items []T, fn func(*ioCache, T) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		next  atomic.Int64
	)

	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()

		return first != nil
	}

	for range min(t.o.Parallel, max(len(items), 1)) {
		wg.Go(func() {
			w := t.newIOCache()
			defer w.close()

			for !failed() && (t.ctx == nil || t.ctx.Err() == nil) {
				i := int(next.Add(1)) - 1
				if i >= len(items) {
					return
				}

				if err := fn(w, items[i]); err != nil {
					mu.Lock()
					first = cmpFirst(first, err)
					mu.Unlock()

					return
				}
			}
		})
	}

	wg.Wait()

	if first == nil && t.ctx != nil && t.ctx.Err() != nil {
		first = t.guard()
	}

	return first
}

// cmpFirst keeps the first error.
func cmpFirst(first, err error) error {
	if first != nil {
		return first
	}

	return err
}
