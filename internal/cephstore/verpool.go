package cephstore

import (
	"sync"

	"github.com/ceph/go-ceph/rados"
)

// Versioned reads
//
// librados reports the version of an object only through
// rados_ioctx_get_last_version(), which is IOContext-wide: it holds the
// version of whatever operation on that context finished last. With
// several goroutines sharing one context (parallel shard writers,
// notifies, log trimming) a compare-and-swap could pick up the version of
// a foreign operation. Versioned reads therefore go through a small pool
// of contexts of which each read owns one exclusively between the read
// and the version query.
type verPool struct {
	mu   sync.Mutex
	free []*rados.IOContext
	all  []*rados.IOContext
	open func() (*rados.IOContext, error)
}

func newVerPool(open func() (*rados.IOContext, error)) *verPool { return &verPool{open: open} }

func (p *verPool) get() (*rados.IOContext, error) {
	p.mu.Lock()
	if n := len(p.free); n > 0 {
		c := p.free[n-1]
		p.free = p.free[:n-1]
		p.mu.Unlock()

		return c, nil
	}
	p.mu.Unlock()

	c, err := p.open()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.all = append(p.all, c)
	p.mu.Unlock()

	return c, nil
}

func (p *verPool) put(c *rados.IOContext) {
	p.mu.Lock()
	p.free = append(p.free, c)
	p.mu.Unlock()
}

func (p *verPool) destroy() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, c := range p.all {
		c.Destroy()
	}

	p.all, p.free = nil, nil
}

// poolFor returns the versioned-read pool matching a shared context.
func (u *User) poolFor(ioctx *rados.IOContext) *verPool {
	if ioctx == u.data {
		return u.dataPool
	}

	return u.idxPool
}

// getKeysVer reads omap keys of oid and returns the object version they
// were read at (0 if the object does not exist), using a private context.
func (u *User) getKeysVer(ioctx *rados.IOContext, oid string, keys []string) (map[string][]byte, uint64, error) {
	pool := u.poolFor(ioctx)

	c, err := pool.get()
	if err != nil {
		return nil, 0, err
	}
	defer pool.put(c)

	return getOmapKeysVer(c, oid, keys)
}

// xattrVer reads one xattr and the object version, using a private context.
func (u *User) xattrVer(ioctx *rados.IOContext, oid, name string) ([]byte, uint64, error) {
	pool := u.poolFor(ioctx)

	c, err := pool.get()
	if err != nil {
		return nil, 0, err
	}
	defer pool.put(c)

	var buf [64]byte

	n, err := c.GetXattr(oid, name, buf[:])
	if err != nil {
		return nil, 0, err
	}

	ver, err := c.GetLastVersion()
	if err != nil {
		return nil, 0, err
	}

	return append([]byte(nil), buf[:n]...), ver, nil
}
