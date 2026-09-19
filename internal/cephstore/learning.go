package cephstore

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"
	"shardpost/internal/imapsieve"
	"shardpost/internal/model"
)

// LearningQueue uses a reserved data-pool namespace. Each object's body and
// metadata are committed in one RADOS write operation. It is independent of
// message blobs, quota and mailbox scrub operations.
type LearningQueue struct{ Store *Store }

// Put enqueues a job with its raw message. A job that is already queued
// (same ID) is left as it is.
func (q LearningQueue) Put(j imapsieve.Job, raw []byte) error {
	u, err := q.Store.User("~learning")
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

	op.Create(rados.CreateExclusive)
	op.WriteFull(raw)
	op.SetXattr("job", b)

	err = op.Operate(u.data, "learning/"+j.ID, rados.OperationNoFlag)
	if errCode(err) == syscall.EEXIST {
		return nil
	}

	return err
}

// All returns every job of the queue, due or not, dead ones included.
func (q LearningQueue) All() ([]imapsieve.Job, error) {
	u, err := q.Store.User("~learning")
	if err != nil {
		return nil, err
	}
	defer u.Close()

	it, err := u.data.Iter()
	if err != nil {
		return nil, err
	}
	defer it.Close()

	var jobs []imapsieve.Job

	for it.Next() {
		if !strings.HasPrefix(it.Value(), "learning/") {
			continue
		}

		buf := make([]byte, 4096)

		n, err := u.data.GetXattr(it.Value(), "job", buf)
		if errCode(err) == syscall.ENOENT {
			continue
		}

		if err != nil {
			return nil, err
		}

		var j imapsieve.Job
		if err = json.Unmarshal(buf[:n], &j); err != nil {
			return nil, err
		}

		jobs = append(jobs, j)
	}

	return jobs, it.Err()
}

// List returns up to 100 live jobs that are due, oldest due time first.
func (q LearningQueue) List() ([]imapsieve.Job, error) {
	all, err := q.All()
	if err != nil {
		return nil, err
	}

	jobs := all[:0]

	for _, j := range all {
		if !j.Dead && !time.Now().Before(j.Next) {
			jobs = append(jobs, j)
		}
	}

	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Next.Equal(jobs[j].Next) {
			return jobs[i].ID < jobs[j].ID
		}

		return jobs[i].Next.Before(jobs[j].Next)
	})

	if len(jobs) > 100 {
		jobs = jobs[:100]
	}

	return jobs, nil
}

// PurgeOwner deletes every learning job of an account (account purge).
func (q LearningQueue) PurgeOwner(account string) (int, error) {
	all, err := q.All()
	if err != nil {
		return 0, err
	}

	n := 0

	for _, j := range all {
		if j.Owner != account {
			continue
		}

		if err := q.Delete(j); err != nil && errCode(err) != syscall.ENOENT {
			return n, err
		}

		n++
	}

	return n, nil
}

// Read returns the raw message stored with the job.
func (q LearningQueue) Read(j imapsieve.Job) ([]byte, error) {
	if j.Size < 0 || j.Size > 64<<20 {
		return nil, fmt.Errorf("invalid learning size")
	}

	u, err := q.Store.User("~learning")
	if err != nil {
		return nil, err
	}
	defer u.Close()

	buf := make([]byte, j.Size)

	n, err := u.data.Read("learning/"+j.ID, buf, 0)
	if err == nil && int64(n) != j.Size {
		err = fmt.Errorf("short learning read")
	}

	return buf, err
}

// Update rewrites the job record of a queued job.
func (q LearningQueue) Update(j imapsieve.Job) error {
	u, err := q.Store.User("~learning")
	if err != nil {
		return err
	}
	defer u.Close()

	b, err := json.Marshal(j)
	if err != nil {
		return err
	}

	return u.data.SetXattr("learning/"+j.ID, "job", b)
}

// Revive makes a dead job due again (manual retry): attempts start over,
// Retries counts the retry. The job must still exist (a purged job is
// not recreated) and be dead; the worker leaves dead jobs alone, so no
// worker write races this one.
func (q LearningQueue) Revive(id string) (imapsieve.Job, error) {
	u, err := q.Store.User("~learning")
	if err != nil {
		return imapsieve.Job{}, err
	}
	defer u.Close()

	buf := make([]byte, 4096)

	n, err := u.data.GetXattr("learning/"+id, "job", buf)
	if errCode(err) == syscall.ENOENT {
		return imapsieve.Job{}, imapsieve.ErrNoJob
	}

	if err != nil {
		return imapsieve.Job{}, err
	}

	var j imapsieve.Job
	if err := json.Unmarshal(buf[:n], &j); err != nil {
		return imapsieve.Job{}, err
	}

	if !j.Dead {
		return j, imapsieve.ErrNotDead
	}

	j.Dead, j.Attempts, j.Next = false, 0, time.Now()
	j.Retries++

	b, err := json.Marshal(j)
	if err != nil {
		return imapsieve.Job{}, err
	}

	op := rados.CreateWriteOp()
	defer op.Release()

	op.AssertExists()
	op.SetXattr("job", b)

	if err := op.Operate(u.data, "learning/"+id, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return imapsieve.Job{}, imapsieve.ErrNoJob
		}

		return imapsieve.Job{}, err
	}

	return j, nil
}

// Delete removes a job and its message from the queue.
func (q LearningQueue) Delete(j imapsieve.Job) error {
	u, err := q.Store.User("~learning")
	if err != nil {
		return err
	}
	defer u.Close()

	return u.data.Delete("learning/" + j.ID)
}

type learningLease struct {
	model.Lease
	u *User
}

func (l *learningLease) Release() { l.Lease.Release(); l.u.Close() }

// Lock takes the queue-wide worker lease; Release on the returned lease
// also closes the queue namespace.
func (q LearningQueue) Lock() (model.Lease, error) {
	u, err := q.Store.User("~learning")
	if err != nil {
		return nil, err
	}

	lease, err := u.Lock("learning-worker", 2*time.Minute)
	if err != nil {
		u.Close()
		return nil, err
	}

	return &learningLease{lease, u}, nil
}

// Ready reports whether the delivery a job belongs to is durable: its
// learning receipt is confirmed or committed in the owner's index.
func (q LearningQueue) Ready(j imapsieve.Job) (bool, error) {
	queued, err := q.Store.User("~learning")
	if err != nil {
		return false, err
	}

	buf := make([]byte, 8)
	_, err = queued.data.GetXattr("learning/"+j.ID, "confirmed", buf)
	queued.Close()

	if err == nil {
		return true, nil
	}

	if errCode(err) != syscall.ENODATA && errCode(err) != syscall.ENOENT {
		return false, err
	}

	u, err := q.Store.User(j.Owner)
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
func (q LearningQueue) Cleanup(j imapsieve.Job) error {
	if !j.Prepared {
		return nil
	}

	u, err := q.Store.User(j.Owner)
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

	q := LearningQueue{Store: u.store}

	queue, err := u.store.User("~learning")
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

		if err := queue.data.SetXattr("learning/"+id, "confirmed", []byte("yes")); err != nil && errCode(err) != syscall.ENOENT {
			return err
		}
	}

	return nil
}
