package cephstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// JMAP uploads (scaling plan B6). An upload is a blob that nothing
// points at, so it costs three writes instead of one:
//
//	index pool, namespace "~accounts":
//	  "ix.uploads/head|<n>"  due queue: "<expiry>/<account>/<guid>/0" -> ""
//	  "ix.uploads/lock"      cls_lock of the expiry worker
//	the account's registry:
//	  "up:<guid>"            the upload intent (blobIntent with Expires)
//	the account's data pool:
//	  "msg/<guid>/<n>"       the blob, one reference
//
// The queue entry comes first, then the intent, then the blob (a
// completeness index: an entry without intent is dropped, an intent never
// lacks its entry). The expiry worker deletes what is due; the scrubber
// does the same for an upload whose entry the worker did not get to, and
// a purge finds the uploads through their intents.
//
// Upload blobs are never linked to a message: Email/import and Email/set
// read them and store a new message (model.Blobs has no way to link one),
// so an expired upload has no reference but its own.

// DefaultUploadTTL is how long an upload is kept when the configuration
// does not say (RFC 8620 6.1 asks for at least an hour).
const DefaultUploadTTL = 24 * time.Hour

// uploadsQueue opens the due queue of the uploads once per process.
func (s *Store) uploadsQueue() (*sortedindex.Queue, error) {
	s.uploadsOnce.Do(func() {
		h, err := s.openHandle(model.AccountsNamespace)
		if err != nil {
			s.uploadsErr = err
			return
		}

		s.uploadsQ = sortedindex.NewQueue(&handleOmap{h: h, key: s.indexKey(model.AccountsNamespace)}, model.UploadsQueue, model.DefaultQueueShards)
	})

	return s.uploadsQ, s.uploadsErr
}

// uploadEntryID is the id of an upload in the queue.
func uploadEntryID(account, guid string) string { return account + "/" + guid }

// splitUploadEntry splits a queue id into account and GUID.
func splitUploadEntry(id string) (string, string, bool) {
	i := strings.LastIndexByte(id, '/')
	if i <= 0 || !model.ValidGUID(id[i+1:]) {
		return "", "", false
	}

	return id[:i], id[i+1:], true
}

// PutBlob implements model.Blobs: it stores raw under a fresh GUID with
// one reference and records it as an upload that expires after the
// configured time (see the comment above).
func (u *User) PutBlob(raw []byte) (string, error) {
	guid := newGUID()
	size := int64(len(raw))
	now := time.Now()
	exp := now.Add(u.uploadTTL)

	if u.store != nil {
		q, err := u.store.uploadsQueue()
		if err != nil {
			return "", fmt.Errorf("uploads queue: %w", err)
		}

		if err := q.Move(uploadEntryID(u.name, guid), nil, &model.DueKey{Due: exp}); err != nil {
			return "", fmt.Errorf("uploads queue: %w", err)
		}
	}

	in := blobIntent{Time: now.Unix(), Size: size, Chunks: u.chunkCount(size), Expires: exp.Unix()}
	if err := u.setIntent(map[string][]byte{uploadKey(guid): encode(in)}); err != nil {
		return "", err
	}

	if _, err := u.writeBlob(guid, raw); err != nil {
		if u.deleteBlobAll(guid, in.Chunks) {
			_ = rmKeys(u.idx, oidRegistry, []string{uploadKey(guid)})
		}

		return "", err
	}

	return guid, nil
}

// ExpireUpload deletes the upload guid if it has expired at now. done is
// false when it has not (next is when it does); an upload without intent
// is done (expired before, or never stored).
func (u *User) ExpireUpload(guid string, now time.Time) (done bool, next time.Time, err error) {
	vals, err := getOmapKeys(u.idx, oidRegistry, []string{uploadKey(guid)})
	if err != nil {
		return false, time.Time{}, err
	}

	v, ok := vals[uploadKey(guid)]
	if !ok {
		return true, time.Time{}, nil
	}

	var in blobIntent
	if json.Unmarshal(v, &in) == nil && in.Expires > now.Unix() {
		return false, time.Unix(in.Expires, 0), nil
	}

	gone, err := u.collectBlob(guid, in.Size, in.Chunks, false)
	if err != nil || !gone {
		return false, now.Add(time.Hour), err
	}

	return true, time.Time{}, rmKeys(u.idx, oidRegistry, []string{uploadKey(guid)})
}

// collectBlob deletes a blob that nothing references any more: its
// reference count is set to 0 under the version read, so a COPY that
// took a reference in between makes this fail (gone=false) and the blob
// stays; with unindex its postings go next, then the chunks. A blob
// whose chunk 0 is missing has its other chunks deleted.
func (u *User) collectBlob(guid string, size int64, chunks int, unindex bool) (bool, error) {
	_, ver, err := u.xattrVer(u.data, chunkOID(guid, 0), xattrRefs)

	switch {
	case errCode(err) == syscall.ENOENT:
		return u.deleteBlobAll(guid, chunks), nil
	case err != nil:
		return false, fmt.Errorf("refs of %s: %w", guid, err)
	}

	op := rados.CreateWriteOp()
	op.AssertVersion(ver)
	op.SetXattr(xattrRefs, []byte("0"))
	err = op.Operate(u.data, chunkOID(guid, 0), rados.OperationNoFlag)
	op.Release()

	switch c := errCode(err); {
	case c == syscall.ERANGE || c == syscall.EOVERFLOW:
		return false, nil // changed meanwhile
	case c == syscall.ENOENT:
		return u.deleteBlobAll(guid, chunks), nil
	case err != nil:
		return false, fmt.Errorf("collect %s: %w", guid, err)
	}

	if unindex {
		if err := u.ftsRemove(&Meta{GUID: guid, Size: size, Chunks: chunks}); err != nil {
			u.log.Debug("fts remove of a collected blob failed", "guid", guid, "err", err)
		}
	}

	return u.deleteBlobAll(guid, chunks), nil
}

// uploadLockTTL is the TTL of the upload expiry worker's cls_lock; it is
// renewed every third of it.
const uploadLockTTL = 3 * time.Minute

// RunUploadExpiry is the upload expiry worker: it takes the worker lease
// of the uploads queue when it is free, keeps it while it works and
// deletes what is due every poll, until ctx ends. Every server runs it;
// one holds the lease at a time.
func (s *Store) RunUploadExpiry(ctx context.Context, poll time.Duration) {
	var lease model.Lease

	defer func() {
		if lease != nil {
			lease.Release()
		}
	}()

	tick := time.NewTicker(poll)
	defer tick.Stop()

	for {
		if lease != nil && lease.Lost() {
			lease.Release()
			lease = nil
		}

		if lease == nil {
			l, err := s.holdLock(model.AccountsNamespace, sortedindex.LockOID(model.UploadsQueue), "worker", "upload expiry", uploadLockTTL, true)
			if err == nil {
				lease = l
			} else if !errors.Is(err, ErrLocked) {
				s.log.Warn("uploads: expiry lease", "err", err)
			}
		}

		if lease != nil {
			if _, err := s.ExpireUploads(time.Now(), lease.Lost); err != nil {
				s.log.Warn("uploads: expiry", "err", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ExpireUploads deletes the uploads due at now, page by page, until none
// is due or lost reports the lease gone; it returns how many it deleted.
func (s *Store) ExpireUploads(now time.Time, lost func() bool) (int, error) {
	const page = 100

	q, err := s.uploadsQueue()
	if err != nil {
		return 0, err
	}

	n := 0

	for range 100 {
		due, err := q.Due(now, page)
		if err != nil {
			return n, err
		}

		for _, e := range due {
			if lost() {
				return n, nil
			}

			done, err := s.expireUploadEntry(q, e, now)
			if err != nil {
				s.log.Warn("uploads: expiry", "entry", e.ID, "err", err)
			} else if done {
				n++
			}
		}

		if len(due) < page {
			return n, nil
		}
	}

	return n, nil
}

// expireUploadEntry handles one due entry: the upload is deleted and the
// entry removed, or the entry moved to when the upload expires.
func (s *Store) expireUploadEntry(q *sortedindex.Queue, e model.DueEntry, now time.Time) (bool, error) {
	account, guid, ok := splitUploadEntry(e.ID)
	if !ok || model.CheckNamespace(account) != nil {
		return false, q.Move(e.ID, []model.DueKey{e.DueKey}, nil)
	}

	u, err := s.User(account)
	if err != nil {
		return false, err
	}
	defer u.Close()

	done, next, err := u.ExpireUpload(guid, now)
	if err != nil {
		return false, err
	}

	if !done {
		return false, q.Move(e.ID, []model.DueKey{e.DueKey}, &model.DueKey{Due: next})
	}

	return true, q.Move(e.ID, []model.DueKey{e.DueKey}, nil)
}
