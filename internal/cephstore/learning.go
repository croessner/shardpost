package cephstore

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/imapsieve"
	"shardpost/internal/learnindex"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// The learning queue (scaling plan B5):
//
//	data pool, namespace "~learning":
//	  "learning/<id>"              the message; xattr "job" (the job as
//	                               JSON), xattr "confirmed" (receipt kept
//	                               when the mailbox was deleted)
//	index pool, namespace "~learning":
//	  "ix.learning/head|<n>"       due queue: "<next>/<id>/<gen>" -> ""
//	  "ix.learning/owner/head|<nn>" hint index: "<account>/<id>" -> ""
//	  "ix.learning/lock"           cls_lock of the backfill
//	  "mbox/learning-worker"       cls_lock "maildrop" of the worker (the
//	                               object every version locks, so servers
//	                               of different versions exclude each other)
//
// Each record's body and job are committed in one RADOS write operation.
// The queue is independent of message blobs, quota and mailbox scrub
// operations. learnindex keeps the indexes.

// learningWorkerOID and learningLockTTL are the worker lease: the object
// and TTL every version uses (renewed every third of it while held).
const (
	learningWorkerOID = "mbox/learning-worker"
	learningLockTTL   = 2 * time.Minute
)

// maxLearningJob bounds the job attribute of a record.
const maxLearningJob = 4096

// LearningQueue returns the learning queue of this process in the
// configured index mode (one per store, it follows key rotations).
func (s *Store) LearningQueue() (*learnindex.Queue, error) {
	s.learnOnce.Do(func() {
		s.mu.RLock()
		mode, shards := s.cfg.LearningIndex, s.cfg.LearningQueueShards
		s.mu.RUnlock()

		o := learnindex.Options{Mode: mode, QueueShards: shards, RequireBackfill: true, Log: s.log.With("component", "learning")}

		if mode != model.KVIndexOff {
			h, err := s.openHandle(model.LearningNamespace)
			if err != nil {
				s.learnErr = err
				return
			}

			o.Backend = &handleOmap{h: h, key: s.indexKey(model.LearningNamespace)}
		}

		s.learnQ = learnindex.New(learningRecords{s: s}, o)
	})

	return s.learnQ, s.learnErr
}

// LockLearningBackfill takes the exclusive cls_lock of the learning
// queue backfill ("ix.learning/lock" in "~learning") like
// LockAccountIndex.
func (s *Store) LockLearningBackfill(ttl time.Duration) (model.Lease, error) {
	return s.holdLock(model.LearningNamespace, sortedindex.LockOID(model.LearningQueue), "backfill", "learning queue backfill", ttl, false)
}

// learningRecords is the record layer of the learning queue.
type learningRecords struct{ s *Store }

var _ learnindex.Records = learningRecords{}

func learningOID(id string) string { return "learning/" + id }

// Create implements learnindex.Records.
func (q learningRecords) Create(j imapsieve.Job, raw []byte) (bool, error) {
	u, err := q.s.User(model.LearningNamespace)
	if err != nil {
		return false, err
	}
	defer u.Close()

	b, err := json.Marshal(j)
	if err != nil {
		return false, err
	}

	op := rados.CreateWriteOp()
	defer op.Release()

	op.Create(rados.CreateExclusive)
	op.WriteFull(raw)
	op.SetXattr("job", b)

	err = op.Operate(u.data, learningOID(j.ID), rados.OperationNoFlag)
	if errCode(err) == syscall.EEXIST {
		return false, nil
	}

	return err == nil, err
}

// Get implements learnindex.Records.
func (q learningRecords) Get(id string) (imapsieve.Job, error) {
	u, err := q.s.User(model.LearningNamespace)
	if err != nil {
		return imapsieve.Job{}, err
	}
	defer u.Close()

	return getLearningJob(u, id)
}

// getLearningJob reads the job attribute of a record.
func getLearningJob(u *User, id string) (imapsieve.Job, error) {
	buf := make([]byte, maxLearningJob)

	n, err := u.data.GetXattr(learningOID(id), "job", buf)

	switch c := errCode(err); {
	case c == syscall.ENOENT || c == syscall.ENODATA:
		return imapsieve.Job{}, imapsieve.ErrNoJob
	case err != nil:
		return imapsieve.Job{}, err
	}

	var j imapsieve.Job
	if err := json.Unmarshal(buf[:n], &j); err != nil {
		return imapsieve.Job{}, fmt.Errorf("learning job %s: %w", id, err)
	}

	return j, nil
}

// List implements learnindex.Records.
func (q learningRecords) List(site string) ([]imapsieve.Job, error) {
	u, err := q.s.User(model.LearningNamespace)
	if err != nil {
		return nil, err
	}
	defer u.Close()

	it, err := listObjects(u.data, site)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	var jobs []imapsieve.Job

	for it.Next() {
		id, ok := strings.CutPrefix(it.Value(), "learning/")
		if !ok {
			continue
		}

		j, err := getLearningJob(u, id)
		if err == imapsieve.ErrNoJob {
			continue
		}

		if err != nil {
			return nil, err
		}

		jobs = append(jobs, j)
	}

	return jobs, it.Err()
}

// Read implements learnindex.Records.
func (q learningRecords) Read(j imapsieve.Job) ([]byte, error) {
	if j.Size < 0 || j.Size > 64<<20 {
		return nil, fmt.Errorf("invalid learning size")
	}

	u, err := q.s.User(model.LearningNamespace)
	if err != nil {
		return nil, err
	}
	defer u.Close()

	buf := make([]byte, j.Size)

	n, err := u.data.Read(learningOID(j.ID), buf, 0)
	if err == nil && int64(n) != j.Size {
		err = fmt.Errorf("short learning read")
	}

	return buf, err
}

// Set implements learnindex.Records: it rewrites the job of an existing
// record (a removed job is not recreated as an empty object).
func (q learningRecords) Set(j imapsieve.Job) error {
	u, err := q.s.User(model.LearningNamespace)
	if err != nil {
		return err
	}
	defer u.Close()

	b, err := json.Marshal(j)
	if err != nil {
		return err
	}

	op := rados.CreateWriteOp()
	defer op.Release()

	op.AssertExists()
	op.SetXattr("job", b)

	err = op.Operate(u.data, learningOID(j.ID), rados.OperationNoFlag)
	if errCode(err) == syscall.ENOENT {
		return imapsieve.ErrNoJob
	}

	return err
}

// Remove implements learnindex.Records.
func (q learningRecords) Remove(id string) error {
	u, err := q.s.User(model.LearningNamespace)
	if err != nil {
		return err
	}
	defer u.Close()

	if err := u.data.Delete(learningOID(id)); err != nil && errCode(err) != syscall.ENOENT {
		return err
	}

	return nil
}

// Lock implements learnindex.Records: the worker lease is the cls_lock
// "maildrop" on "mbox/learning-worker" in the index pool, the lock every
// version takes (older ones per pass), held and renewed while this
// server is the worker and given up at a key rotation.
func (q learningRecords) Lock() (model.Lease, error) {
	return q.s.holdLock(model.LearningNamespace, learningWorkerOID, lockName, "learning worker", learningLockTTL, true)
}

// Ready reports whether the delivery a job belongs to is durable: its
// learning receipt is confirmed or committed in the owner's index.
func (q learningRecords) Ready(j imapsieve.Job) (bool, error) {
	queued, err := q.s.User(model.LearningNamespace)
	if err != nil {
		return false, err
	}

	buf := make([]byte, 8)
	_, err = queued.data.GetXattr(learningOID(j.ID), "confirmed", buf)
	queued.Close()

	if err == nil {
		return true, nil
	}

	if errCode(err) != syscall.ENODATA && errCode(err) != syscall.ENOENT {
		return false, err
	}

	u, err := q.s.User(j.Owner)
	if err != nil {
		return false, err
	}
	defer u.Close()

	vals, err := getOmapKeys(u.idx, mboxOID(j.MailboxID), []string{"learning:" + j.ID, keyShardSize})
	if errCode(err) == syscall.ENOENT {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	uid, err := strconv.ParseUint(string(vals["learning:"+j.ID]), 10, 32)
	if err != nil {
		return false, nil
	}

	ss, _ := strconv.ParseUint(string(vals[keyShardSize]), 10, 32)

	shard := shardOf(uint32(uid), uint32(ss))
	if shard == 0 {
		return true, nil
	}

	v, err := getOmapKeys(u.idx, u.shardOID(j.MailboxID, shard), []string{"learning:" + j.ID})
	if errCode(err) == syscall.ENOENT {
		return false, nil
	}

	return string(v["learning:"+j.ID]) == "committed", err
}

// Cleanup removes the learning receipt of a prepared job from the
// owner's index.
func (q learningRecords) Cleanup(j imapsieve.Job) error {
	if !j.Prepared {
		return nil
	}

	u, err := q.s.User(j.Owner)
	if err != nil {
		return err
	}
	defer u.Close()

	key := "learning:" + j.ID

	vals, err := getOmapKeys(u.idx, mboxOID(j.MailboxID), []string{key, keyShardSize})
	if errCode(err) == syscall.ENOENT {
		return nil
	}

	if err != nil {
		return err
	}

	uid, _ := strconv.ParseUint(string(vals[key]), 10, 32)

	ss, _ := strconv.ParseUint(string(vals[keyShardSize]), 10, 32)
	if uid > 0 {
		oid := u.shardOID(j.MailboxID, shardOf(uint32(uid), uint32(ss)))
		if err := u.idx.RmOmapKeys(oid, []string{key}); err != nil && errCode(err) != syscall.ENOENT {
			return err
		}
	}

	if err := u.idx.RmOmapKeys(mboxOID(j.MailboxID), []string{key}); err != nil && errCode(err) != syscall.ENOENT {
		return err
	}

	return nil
}

// Transfer commit receipts into their queue objects before deleting a mailbox.
func (u *User) preserveLearningReceipts(mb *Mailbox) error {
	vals, err := u.idx.GetAllOmapValues(mboxOID(mb.ID), "", "learning:", omapPageSize)
	if errCode(err) == syscall.ENOENT {
		return nil
	}

	if err != nil {
		return err
	}

	q := learningRecords{s: u.store}

	queue, err := u.store.User(model.LearningNamespace)
	if err != nil {
		return err
	}
	defer queue.Close()

	for key := range vals {
		id := strings.TrimPrefix(key, "learning:")
		j := imapsieve.Job{ID: id, Owner: u.name, MailboxID: mb.ID}

		ready, err := q.Ready(j)
		if err != nil {
			return err
		}

		if !ready {
			continue
		}

		if err := queue.data.SetXattr(learningOID(id), "confirmed", []byte("yes")); err != nil && errCode(err) != syscall.ENOENT {
			return err
		}
	}

	return nil
}
