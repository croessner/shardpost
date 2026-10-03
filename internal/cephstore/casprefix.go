package cephstore

import (
	"fmt"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"
)

// casOmapPrefix: a compare-and-swap whose read covers keys and prefix
// ranges in one read op (stage B16, temp/16 §4.4). casOmapOp reads only
// keys under the version; a prefix range read inside fn would be a read
// op of its own. Here the keys and the first page of every prefix come
// from one op, the version is read with them, and the write is guarded
// by that version: the "+0 ops" of the prefix reads of the run key, the
// switch, the import lock against mailbox locks and the clean-up rest
// on it.

// prefixPage is the first page of one prefix range of a casOmapPrefix
// read: the values by full key and whether the range has more.
type prefixPage struct {
	vals map[string][]byte
	more bool
}

// prefixPageSize is how many keys of a prefix one read step returns.
const prefixPageSize = omapPageSize

// getKeysPrefixVer reads keys and the first page of every prefix of oid
// in one read op and returns the object version they were read at (0 if
// the object does not exist). The context must not be shared (see
// User.getKeysVer).
func getKeysPrefixVer(ioctx *rados.IOContext, oid string, keys, prefixes []string) (map[string][]byte, map[string]prefixPage, uint64, error) {
	defer mh.timer("omap_read")()

	if len(keys) == 0 && len(prefixes) == 0 {
		return map[string][]byte{}, emptyPages(prefixes), 0, nil
	}

	op := rados.CreateReadOp()
	defer op.Release()

	var keyStep *rados.ReadOpOmapGetValsByKeysStep
	if len(keys) > 0 {
		keyStep = op.GetOmapValuesByKeys(keys)
	}

	steps := make([]*rados.GetOmapStep, len(prefixes))
	for i, p := range prefixes {
		steps[i] = op.GetOmapValues("", p, prefixPageSize)
	}

	if err := op.Operate(ioctx, oid, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return map[string][]byte{}, emptyPages(prefixes), 0, nil
		}

		return nil, nil, 0, err
	}

	ver, err := ioctx.GetLastVersion()
	if err != nil {
		return nil, nil, 0, err
	}

	cur := map[string][]byte{}

	if keyStep != nil {
		if err := eachKeyValue(keyStep.Next, cur); err != nil {
			return nil, nil, 0, err
		}
	}

	pages := make(map[string]prefixPage, len(prefixes))

	for i, p := range prefixes {
		page := prefixPage{vals: map[string][]byte{}}
		if err := eachKeyValue(steps[i].Next, page.vals); err != nil {
			return nil, nil, 0, err
		}

		page.more = steps[i].More()
		pages[p] = page
	}

	return cur, pages, ver, nil
}

// emptyPages returns an empty page per prefix (an absent object).
func emptyPages(prefixes []string) map[string]prefixPage {
	pages := make(map[string]prefixPage, len(prefixes))
	for _, p := range prefixes {
		pages[p] = prefixPage{vals: map[string][]byte{}}
	}

	return pages
}

// eachKeyValue drains a read step into out.
func eachKeyValue(next func() (*rados.OmapKeyValue, error), out map[string][]byte) error {
	for {
		kv, err := next()
		if err != nil {
			return err
		}

		if kv == nil {
			return nil
		}

		out[kv.Key] = append([]byte(nil), kv.Value...)
	}
}

// getKeysPrefix is getKeysPrefixVer with a private context of the view.
func (u *User) getKeysPrefix(ioctx *rados.IOContext, oid string, keys, prefixes []string) (map[string][]byte, map[string]prefixPage, uint64, error) {
	pool := u.poolFor(ioctx)

	c, err := pool.get()
	if err != nil {
		return nil, nil, 0, err
	}
	defer pool.put(c)

	return getKeysPrefixVer(c, oid, keys, prefixes)
}

// casOmapPrefix is casOmap whose fn also sees the first page of every
// prefix range, read in the same op as the keys and so covered by the
// version check. A prefix with more keys than one page reports more; fn
// decides whether that needs a follow-up CAS. Like casOmapOp it creates
// a missing registry (with the account's directory entry) and retries a
// round lost to another writer with a backoff.
func casOmapPrefix(u *User, ioctx *rados.IOContext, oid string, keys, prefixes []string,
	fn func(cur map[string][]byte, pages map[string]prefixPage) (set map[string][]byte, rm []string),
) error {
	return casOmapPrefixAs(u, ioctx, oid, keys, prefixes, "", fn)
}

// casOmapPrefixAs is casOmapPrefix on behalf of an import lock session
// (see casOmapOpAs).
func casOmapPrefixAs(u *User, ioctx *rados.IOContext, oid string, keys, prefixes []string, session string,
	fn func(cur map[string][]byte, pages map[string]prefixPage) (set map[string][]byte, rm []string),
) (err error) {
	end := u.trace(spanOmapCAS, attrOID(oid), attrCount(len(keys)+len(prefixes)))
	defer func() { end(err) }()

	for attempt := 0; attempt < casRetries; attempt++ {
		cur, pages, ver, err := u.getKeysPrefix(ioctx, oid, keys, prefixes)
		if err != nil {
			return err
		}

		set, rm := fn(cur, pages)
		if len(set) == 0 && len(rm) == 0 {
			return nil
		}

		if ver == 0 && oid == oidRegistry && ioctx == u.idx {
			if err := u.registerAccount(session); err != nil {
				return err
			}
		}

		op := casWriteOp(ver, set, rm, nil)
		err = timedOp("omap_cas", func() error { return op.Operate(ioctx, oid, rados.OperationNoFlag) })

		op.Release()

		if err == nil {
			return nil
		}

		switch errCode(err) {
		case syscall.ERANGE, syscall.EOVERFLOW, syscall.EEXIST:
			mh.casRetry("omap", casClass(oid))

			if attempt+1 < casRetries {
				time.Sleep(casBackoff(attempt))
			}

			continue
		}

		return err
	}

	return fmt.Errorf("%s: too many concurrent writers", oid)
}
