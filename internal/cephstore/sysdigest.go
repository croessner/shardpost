package cephstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// The digests of a migration (temp/12 §5.4, R3.12)
//
// Every object copied has its digest in the run state's digest objects
// "system.migrate/<ns>/<nnn>" of the system pool, one omap key per name;
// M4, M5, abort and verify read them there. Their number per namespace
// is fixed in the run state (digest_shards) when the copy begins, so
// that a resumed run finds every digest where it was written. A
// namespace digest is the SHA-256 over the sorted names with their
// digests, the total one over the namespace digests.

// digestShardKeys is the number of digests a digest object holds on
// average: the hash spreads the names unevenly, and no object may come
// near the large omap warning of the OSDs (200 000 keys); temp/12
// names 50 000 as the bound for the largest one.
const digestShardKeys = 40000

// maxDigestShards bounds the digest objects per namespace.
const maxDigestShards = 256

// digestFlush is the number of digests buffered per object before they
// are written.
const digestFlush = 512

// digestTotal is the key of the total digest in m4_digest.
const digestTotal = "total"

// digestShards is the number of digest objects per namespace for a run
// of objects objects.
func digestShards(objects int) int {
	return min(max(1, (objects+digestShardKeys-1)/digestShardKeys), maxDigestShards)
}

// digestOID names digest object shard of namespace ns.
func digestOID(ns string, shard int) string { return fmt.Sprintf("%s/%s/%03d", runStateOID, ns, shard) }

// nsDigests are digests by namespace and object name.
type nsDigests map[string]map[string]string

// set records a digest.
func (d nsDigests) set(ns, oid, digest string) {
	if d[ns] == nil {
		d[ns] = map[string]string{}
	}

	d[ns][oid] = digest
}

// sums are the namespace digests and the total one (digestTotal), over
// the four namespaces (an empty one included).
func (d nsDigests) sums() map[string]string {
	out := map[string]string{}
	total := sha256.New()

	for _, ns := range model.SystemNamespaces() {
		h := sha256.New()

		for _, oid := range slices.Sorted(maps.Keys(d[ns])) {
			field(h, []byte(oid))
			field(h, []byte(d[ns][oid]))
		}

		out[ns] = hex.EncodeToString(h.Sum(nil))

		field(total, []byte(ns))
		field(total, []byte(out[ns]))
	}

	out[digestTotal] = hex.EncodeToString(total.Sum(nil))

	return out
}

// sumsText is the JSON of the sums.
func sumsText(sums map[string]string) string {
	b, _ := json.Marshal(sums)
	return string(b)
}

// parseSums reads sums written by sumsText.
func parseSums(s string) (map[string]string, error) {
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("%w: unreadable digest %q", ErrSystemMismatch, s)
	}

	return out, nil
}

// digestWriter buffers digests and writes them to the digest objects.
type digestWriter struct {
	t      *sysTool
	shards int
	mu     sync.Mutex
	buf    map[string]map[string][]byte
}

func (t *sysTool) newDigestWriter(shards int) *digestWriter {
	return &digestWriter{t: t, shards: shards, buf: map[string]map[string][]byte{}}
}

// put records the digest of oid of ns; a full buffer is written.
func (w *digestWriter) put(ns, oid, digest string) error {
	obj := digestOID(ns, model.IndexShard(oid, w.shards))

	w.mu.Lock()

	if w.buf[obj] == nil {
		w.buf[obj] = map[string][]byte{}
	}

	w.buf[obj][oid] = []byte(digest)

	var full map[string][]byte
	if len(w.buf[obj]) >= digestFlush {
		full, w.buf[obj] = w.buf[obj], nil
	}

	w.mu.Unlock()

	if full != nil {
		return w.write(obj, full)
	}

	return nil
}

// flush writes what is buffered.
func (w *digestWriter) flush() error {
	w.mu.Lock()
	buf := w.buf
	w.buf = map[string]map[string][]byte{}
	w.mu.Unlock()

	for _, obj := range slices.Sorted(maps.Keys(buf)) {
		if err := w.write(obj, buf[obj]); err != nil {
			return err
		}
	}

	return nil
}

func (w *digestWriter) write(obj string, vals map[string][]byte) error {
	if len(vals) == 0 {
		return nil
	}

	c, err := w.t.openCtx(w.t.sysPool, model.AdminNamespace)
	if err != nil {
		return err
	}
	defer c.Destroy()

	err = operate(c, obj, w.t.guard, func(op *rados.WriteOp) {
		op.Create(rados.CreateIdempotent)
		op.SetOmap(vals)
	})
	if err != nil {
		return fmt.Errorf("write the digests %s: %w", obj, err)
	}

	return nil
}

// readDigests reads the digest objects of the run.
func (t *sysTool) readDigests(shards int) (nsDigests, error) {
	c, err := t.openCtx(t.sysPool, model.AdminNamespace)
	if err != nil {
		return nil, err
	}
	defer c.Destroy()

	out := nsDigests{}

	for _, ns := range model.SystemNamespaces() {
		for shard := range shards {
			vals, err := c.GetAllOmapValues(digestOID(ns, shard), "", "", omapCopyPage)
			if errCode(err) == syscall.ENOENT {
				continue
			}

			if err != nil {
				return nil, fmt.Errorf("read the digests %s: %w", digestOID(ns, shard), err)
			}

			for oid, d := range vals {
				out.set(ns, oid, string(d))
			}
		}
	}

	return out, nil
}

// runShards is the digest object count of the run state (0: not set).
func runShards(rs runState) int {
	var n int

	_, _ = fmt.Sscanf(rs.Values[rsDigestShards], "%d", &n)

	return min(max(n, 0), maxDigestShards)
}
