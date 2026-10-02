package cephstore

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"maps"
	"slices"
	"strings"
	"syscall"

	"github.com/ceph/go-ceph/rados"
)

// Copying objects of the store's own namespaces (temp/12 §5.4)
//
// An object is read whole: its size, its data in chunks of toolChunk, a
// second stat that shows the size did not change, its xattrs without the
// cls_lock state ("lock.*") and its omap in pages. Its digest is a
// SHA-256 over
//
//	name | size | data | (xattr, value) sorted | (omap key, value) sorted
//
// each field with its length, so that no two objects share an encoding.
// The copy creates the target exclusively with the first chunk, the
// xattrs and the first omap page in one write; further chunks and pages
// follow with the target asserted to exist. Neither the object version,
// the mtime, the omap header (ShardPost writes none) nor the lock state
// is copied: an empty lock object is copied as an empty object.

// omapCopyPage is the number of omap keys read or written at a time.
const omapCopyPage = 1000

// lockXattr is the prefix of the xattrs cls_lock keeps its state in.
const lockXattr = "lock."

// objImage is an object as read.
type objImage struct {
	Size   uint64            `json:"size"`
	Data   []byte            `json:"data,omitempty"`
	Xattrs map[string][]byte `json:"xattrs,omitempty"`
	Omap   map[string][]byte `json:"omap,omitempty"`
	// locks are the cls_lock names whose state the object carries.
	locks []string
}

// errObjectChanges is the error of an object that kept changing size
// while it was read.
var errObjectChanges = errors.New("object keeps changing while it is read")

// readImage reads oid on c; ENOENT is returned as it is (errCode).
func readImage(c *rados.IOContext, oid string) (*objImage, error) {
	for range 3 {
		st, err := c.Stat(oid)
		if err != nil {
			return nil, err
		}

		im := &objImage{Size: st.Size}

		complete, err := im.readData(c, oid)
		if err != nil {
			return nil, err
		}

		if err := im.readXattrs(c, oid); err != nil {
			return nil, err
		}

		if err := im.readOmap(c, oid); err != nil {
			return nil, err
		}

		after, err := c.Stat(oid)
		if err != nil {
			return nil, err
		}

		if complete && after.Size == st.Size {
			return im, nil
		}
	}

	return nil, fmt.Errorf("%s: %w", oid, errObjectChanges)
}

// readData reads the data in chunks; false when the object ended early.
func (im *objImage) readData(c *rados.IOContext, oid string) (bool, error) {
	if im.Size == 0 {
		return true, nil
	}

	im.Data = make([]byte, im.Size)

	for off := uint64(0); off < im.Size; {
		end := min(off+toolChunk, im.Size)

		n, err := c.Read(oid, im.Data[off:end], off)
		if err != nil {
			return false, err
		}

		if n == 0 {
			return false, nil
		}

		off += uint64(n)
	}

	return true, nil
}

// readXattrs reads the xattrs, keeping the lock state apart.
func (im *objImage) readXattrs(c *rados.IOContext, oid string) error {
	xs, err := c.ListXattrs(oid)
	if err != nil {
		return err
	}

	for k, v := range xs {
		if name, ok := strings.CutPrefix(k, lockXattr); ok {
			im.locks = append(im.locks, name)
			continue
		}

		if im.Xattrs == nil {
			im.Xattrs = map[string][]byte{}
		}

		im.Xattrs[k] = v
	}

	slices.Sort(im.locks)

	return nil
}

// readOmap reads the omap in pages.
func (im *objImage) readOmap(c *rados.IOContext, oid string) error {
	after := ""

	for {
		page, err := c.GetOmapValues(oid, after, "", omapCopyPage)
		if err != nil {
			return err
		}

		for k, v := range page {
			if im.Omap == nil {
				im.Omap = map[string][]byte{}
			}

			im.Omap[k] = v
			after = max(after, k)
		}

		if len(page) < omapCopyPage {
			return nil
		}
	}
}

// digest is the digest of the object under name (its object name; a
// copy elsewhere has the same digest).
func (im *objImage) digest(name string) string {
	h := sha256.New()

	field(h, []byte(name))

	var n [8]byte

	binary.BigEndian.PutUint64(n[:], im.Size)
	h.Write(n[:])
	field(h, im.Data)
	pairs(h, im.Xattrs)
	pairs(h, im.Omap)

	return hex.EncodeToString(h.Sum(nil))
}

// field writes b with its length.
func field(h hash.Hash, b []byte) {
	var n [8]byte

	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	h.Write(n[:])
	h.Write(b)
}

// pairs writes the count and the key/value pairs in key order.
func pairs(h hash.Hash, m map[string][]byte) {
	var n [8]byte

	binary.BigEndian.PutUint64(n[:], uint64(len(m)))
	h.Write(n[:])

	for _, k := range slices.Sorted(maps.Keys(m)) {
		field(h, []byte(k))
		field(h, m[k])
	}
}

// omapPages splits the omap into pages in key order.
func (im *objImage) omapPages() []map[string][]byte {
	keys := slices.Sorted(maps.Keys(im.Omap))

	var out []map[string][]byte

	for chunk := range slices.Chunk(keys, omapCopyPage) {
		page := make(map[string][]byte, len(chunk))
		for _, k := range chunk {
			page[k] = im.Omap[k]
		}

		out = append(out, page)
	}

	return out
}

// errObjectExists is the error of a copy whose target exists.
var errObjectExists = errors.New("target exists")

// writeImage creates oid on c as im; guard is checked before every write.
// errObjectExists: the target is there already.
func writeImage(c *rados.IOContext, oid string, im *objImage, guard func() error) error {
	pages := im.omapPages()
	first := im.Data[:min(uint64(len(im.Data)), toolChunk)]

	err := operate(c, oid, guard, func(op *rados.WriteOp) {
		op.Create(rados.CreateExclusive)

		if len(first) > 0 {
			op.WriteFull(first)
		}

		for k, v := range im.Xattrs {
			op.SetXattr(k, v)
		}

		if len(pages) > 0 {
			op.SetOmap(pages[0])
		}
	})
	if errCode(err) == syscall.EEXIST {
		return errObjectExists
	}

	if err != nil {
		return err
	}

	for off := uint64(len(first)); off < uint64(len(im.Data)); off += toolChunk {
		chunk := im.Data[off:min(off+toolChunk, uint64(len(im.Data)))]

		if err := operate(c, oid, guard, func(op *rados.WriteOp) { op.AssertExists(); op.Write(chunk, off) }); err != nil {
			return err
		}
	}

	for _, page := range pages[min(1, len(pages)):] {
		if err := operate(c, oid, guard, func(op *rados.WriteOp) { op.AssertExists(); op.SetOmap(page) }); err != nil {
			return err
		}
	}

	return nil
}

// operate runs one write op built by build, after the guard.
func operate(c *rados.IOContext, oid string, guard func() error, build func(*rados.WriteOp)) error {
	if err := guard(); err != nil {
		return err
	}

	op := rados.CreateWriteOp()
	defer op.Release()

	build(op)

	return op.Operate(c, oid, rados.OperationNoFlag)
}

// Outcomes of putImage.
const (
	putCopied   = "copied"
	putKept     = "kept"
	putReplaced = "replaced"
)

// putImage writes im as oid on c, whose digest under name is want. An
// existing object with that digest is kept; a different one is replaced
// when replace is set, else the copy fails with ErrSystemMismatch (a
// copy that must never be overwritten).
func putImage(c *rados.IOContext, oid, name string, im *objImage, want string, replace bool, guard func() error) (string, error) {
	err := writeImage(c, oid, im, guard)
	if err == nil {
		return putCopied, nil
	}

	if !errors.Is(err, errObjectExists) {
		return "", fmt.Errorf("write %s: %w", name, err)
	}

	have, err := readImage(c, oid)
	if errCode(err) == syscall.ENOENT {
		return putImage(c, oid, name, im, want, replace, guard)
	}

	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}

	if have.digest(oid) == want {
		return putKept, nil
	}

	if !replace {
		return "", fmt.Errorf("%w: %s exists with other content and must not be overwritten", ErrSystemMismatch, name)
	}

	if err := operate(c, oid, guard, func(op *rados.WriteOp) { op.Remove() }); err != nil && errCode(err) != syscall.ENOENT {
		return "", fmt.Errorf("replace %s: %w", name, err)
	}

	if err := writeImage(c, oid, im, guard); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}

	return putReplaced, nil
}

// lockers returns a finding for every cls_lock of im that is held.
func lockers(c *rados.IOContext, oid, name string, im *objImage) ([]Finding, error) {
	var out []Finding

	for _, lock := range im.locks {
		info, err := c.ListLockers(oid, lock)
		if errCode(err) == syscall.ENOENT {
			continue
		}

		if err != nil {
			return out, fmt.Errorf("lockers of %s: %w", name, err)
		}

		if info != nil && info.NumLockers > 0 {
			out = append(out, Finding{Check: "locks", Object: name, Blocking: true,
				Detail: fmt.Sprintf("lock %q held by %s (%s)", lock, strings.Join(info.Clients, ","), strings.Join(info.Cookies, ","))})
		}
	}

	return out, nil
}
