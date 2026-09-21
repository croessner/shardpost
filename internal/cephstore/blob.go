package cephstore

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// Message blobs are split into fixed size chunks "msg/<guid>/<n>". Chunks
// are written and read in parallel, so a large message is spread over
// several OSDs/PGs and the latency of one message does not grow linearly
// with its size. Chunk 0 additionally carries the reference count of the
// blob in the xattr "refs": COPY increments it and MOVE leaves it alone, so
// neither has to touch the payload. The blob is deleted when the count
// drops to zero.
const (
	DefaultChunkSize = 4 << 20
	xattrRefs        = "refs"
	// xattrLen is the total size of the blob, on chunk 0. It lets a
	// reader that has no index entry (a JMAP blob download) know how
	// much there is without probing for the last chunk.
	xattrLen        = "len"
	blobParallelism = 8
)

func chunkOID(guid string, n int) string { return fmt.Sprintf("%s%s/%d", msgPrefix, guid, n) }

func (u *User) chunkCount(size int64) int {
	if size == 0 {
		return 1
	}

	return int((size + u.chunkSize - 1) / u.chunkSize)
}

// writeBlob stores raw under guid and sets refs=1 on chunk 0.
func (u *User) writeBlob(guid string, raw []byte) (chunks int, err error) {
	end := u.trace(spanBlobWrite, attrGUID(guid), attrBytes(len(raw)))
	defer func() { end(err) }()

	chunks = u.chunkCount(int64(len(raw)))
	errs := make(chan error, chunks)
	sem := make(chan struct{}, blobParallelism)

	var wg sync.WaitGroup

	for n := 0; n < chunks; n++ {
		lo := int64(n) * u.chunkSize
		hi := min(lo+u.chunkSize, int64(len(raw)))

		wg.Add(1)

		sem <- struct{}{}

		go func(n int, part []byte) {
			defer wg.Done()
			defer func() { <-sem }()

			op := rados.CreateWriteOp()
			defer op.Release()

			op.Create(rados.CreateExclusive)
			op.WriteFull(part)

			if n == 0 {
				op.SetXattr(xattrRefs, []byte("1"))
				op.SetXattr(xattrLen, []byte(strconv.FormatInt(int64(len(raw)), 10)))
			}

			if err := op.Operate(u.data, chunkOID(guid, n), rados.OperationNoFlag); err != nil {
				errs <- fmt.Errorf("write chunk %d: %w", n, err)
			}
		}(n, raw[lo:hi])
	}

	wg.Wait()
	close(errs)

	if err := <-errs; err != nil {
		u.deleteBlob(guid, chunks)
		return 0, err
	}

	return chunks, nil
}

// readBlob reassembles the blob described by m.
func (u *User) readBlob(m *Meta) (raw []byte, err error) {
	end := u.trace(spanBlobRead, attrGUID(m.GUID), attrBytes(int(m.Size)))
	defer func() { end(err) }()

	buf := make([]byte, m.Size)

	chunks := m.Chunks
	if chunks == 0 {
		chunks = 1
	}

	errs := make(chan error, chunks)
	sem := make(chan struct{}, blobParallelism)

	var wg sync.WaitGroup

	for n := 0; n < chunks; n++ {
		lo := int64(n) * u.chunkSize
		hi := min(lo+u.chunkSize, m.Size)

		wg.Add(1)

		sem <- struct{}{}

		go func(n int, part []byte) {
			defer wg.Done()
			defer func() { <-sem }()

			got, err := u.data.Read(chunkOID(m.GUID, n), part, 0)
			if err != nil {
				errs <- fmt.Errorf("read chunk %d of %s: %w", n, m.GUID, err)
			} else if got != len(part) {
				errs <- fmt.Errorf("short read chunk %d of %s: %d/%d", n, m.GUID, got, len(part))
			}
		}(n, buf[lo:hi])
	}

	wg.Wait()
	close(errs)

	if err := <-errs; err != nil {
		return nil, err
	}

	return buf, nil
}

// deleteBlob removes all chunks (best effort, ENOENT ignored).
func (u *User) deleteBlob(guid string, chunks int) {
	for n := 0; n < max(chunks, 1); n++ {
		if err := u.data.Delete(chunkOID(guid, n)); err != nil && errCode(err) != syscall.ENOENT {
			u.log.Warn("orphan chunk", "guid", guid, "chunk", n, "err", err)
		}
	}
}

// ErrGone is returned when a blob is being garbage collected.
var ErrGone = errors.New("message is gone")

// blobRef adds delta to the reference count of the blob and returns the
// new count. Compare-and-swap on the object version of chunk 0 (read on a
// private context). A count of zero is final: it is written before the
// chunks are deleted, and an increment against zero fails with ErrGone, so
// a COPY racing with the last EXPUNGE cannot resurrect a blob that is
// about to disappear.
func (u *User) blobRef(guid string, delta int) (int, error) {
	oid := chunkOID(guid, 0)
	for attempt := 0; attempt < casRetries; attempt++ {
		val, ver, err := u.xattrVer(u.data, oid, xattrRefs)
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				return 0, ErrGone
			}

			return 0, fmt.Errorf("refs of %s: %w", guid, err)
		}

		refs, _ := strconv.Atoi(string(val))
		if refs <= 0 && delta > 0 {
			return 0, ErrGone
		}

		refs += delta
		if refs < 0 {
			refs = 0
		}

		op := rados.CreateWriteOp()
		op.AssertVersion(ver)
		op.SetXattr(xattrRefs, []byte(strconv.Itoa(refs)))
		err = op.Operate(u.data, oid, rados.OperationNoFlag)
		op.Release()

		if err == nil {
			return refs, nil
		}

		if c := errCode(err); c == syscall.ERANGE || c == syscall.EOVERFLOW {
			mh.casRetry("refs")
			continue
		}

		return 0, fmt.Errorf("update refs of %s: %w", guid, err)
	}

	return 0, fmt.Errorf("update refs of %s: too many concurrent writers", guid)
}

// releaseBlob drops one reference and deletes the blob (and its FTS
// postings) when nobody references it any more.
func (u *User) releaseBlob(m *Meta) error {
	refs, err := u.blobRef(m.GUID, -1)
	if err != nil {
		if errors.Is(err, ErrGone) {
			return nil
		}

		return err
	}

	if refs > 0 {
		return nil
	}

	if err := u.ftsRemove(m); err != nil {
		u.log.Warn("fts remove failed", "guid", m.GUID, "err", err)
	}

	u.deleteBlob(m.GUID, m.Chunks)

	return nil
}

// PutBlob implements model.Blobs: it stores raw under a fresh GUID with
// one reference, so that nothing collects it while the client is still
// looking at the response. Nothing points at the blob yet, so the
// scrubber sees it as an orphan and removes it after the two runs its
// orphan rule requires — which is what an expiring JMAP upload is.
func (u *User) PutBlob(raw []byte) (string, error) {
	guid := newGUID()

	if _, err := u.writeBlob(guid, raw); err != nil {
		return "", err
	}

	return guid, nil
}

// BlobSize implements model.Blobs. A blob written before the length was
// recorded as an xattr has none, which RADOS reports as ENODATA; its
// size is then summed from the chunks.
func (u *User) BlobSize(guid string) (int64, error) {
	if !model.ValidGUID(guid) {
		return 0, model.ErrNoBlob
	}

	val, _, err := u.xattrVer(u.data, chunkOID(guid, 0), xattrLen)

	switch {
	case errCode(err) == syscall.ENOENT:
		return 0, model.ErrNoBlob
	case errCode(err) == syscall.ENODATA:
		return u.blobSizeByChunks(guid)
	case err != nil:
		return 0, fmt.Errorf("size of %s: %w", guid, err)
	}

	if n, perr := strconv.ParseInt(string(val), 10, 64); perr == nil {
		return n, nil
	}

	return u.blobSizeByChunks(guid)
}

// blobSizeByChunks sums the chunk sizes of a blob written before the
// length was recorded as an xattr.
func (u *User) blobSizeByChunks(guid string) (int64, error) {
	var total int64

	for n := 0; ; n++ {
		st, err := u.data.Stat(chunkOID(guid, n))
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				if n == 0 {
					return 0, model.ErrNoBlob
				}

				return total, nil
			}

			return 0, fmt.Errorf("stat chunk %d of %s: %w", n, guid, err)
		}

		total += int64(st.Size)

		if int64(st.Size) < u.chunkSize {
			return total, nil
		}
	}
}

// ReadBlob implements model.Blobs.
func (u *User) ReadBlob(guid string, size, offset, length int64) ([]byte, error) {
	lo, hi := clampRange(size, offset, length)
	if lo >= hi {
		return []byte{}, nil
	}

	buf := make([]byte, hi-lo)

	for pos := lo; pos < hi; {
		n := int(pos / u.chunkSize)
		within := pos - int64(n)*u.chunkSize
		want := min(u.chunkSize-within, hi-pos)

		got, err := u.data.Read(chunkOID(guid, n), buf[pos-lo:pos-lo+want], uint64(within))
		if err != nil {
			return nil, fmt.Errorf("read chunk %d of %s: %w", n, guid, err)
		}

		if got == 0 {
			return nil, fmt.Errorf("short read chunk %d of %s", n, guid)
		}

		pos += int64(got)
	}

	return buf, nil
}

// clampRange turns a requested range into the part of size it covers; a
// length below zero means "to the end".
func clampRange(size, offset, length int64) (lo, hi int64) {
	lo = max(offset, 0)
	if lo > size {
		lo = size
	}

	hi = size
	if length >= 0 {
		hi = min(lo+length, size)
	}

	return lo, hi
}
