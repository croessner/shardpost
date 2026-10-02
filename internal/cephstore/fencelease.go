package cephstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// The tool lease in the fence
//
// A tool that changes the placement (the init; the migration of a later
// release) holds a lease in the fence for as long as it works:
//
//	tool_instance  who holds it: host name and a random part
//	tool_epoch     counted up by every write of a holder
//	tool_seen      the holder's clock at its last write (informational)
//
// A tool takes the lease before its checks, without changing what the
// fence means to the servers: a fence that was absent or reverted gets
// the neutral state "leased" (with the state before in "prev_state"),
// which Open and the periodic check treat as no fence. Only the tool's
// first real step sets its start state ("initializing") by
// compare-and-swap. A tool that ends before that (a refusal) gives the
// lease up by deleting the fence it created, or by restoring the state
// before; a "leased" fence that a crash left behind is harmless and
// taken over by the next tool.
//
// Every write of the fence is a compare-and-swap on its version, the
// holder's renewals as well as its state changes, and all of them go
// through one writer per process (fenceLease.update): a write that lost
// against the holder's own renewal reads the fence again and retries.
// Only a foreign holder ends it.
//
// The lease does not depend on synchronized clocks. A free lease is
// taken at once. An occupied one only when the taker has seen
// tool_instance and tool_epoch unchanged for toolLeaseTakeover of its
// own monotonic clock; a tool_seen more than toolLeaseFuture ahead of
// the taker's clock is refused (a clock that far off is a fault to fix
// first). The holder trusts its lease only for toolLeaseValid after it
// sent its last successful write, measured on its own monotonic clock
// and well below the takeover time: after that it stops writing and
// ends (the work is resumable). OSD operations of a tool time out within
// 60 s (checkToolClientOptions), so a write sent within the window lands
// before a taker can act.

// Timing of the tool lease (variables for the tests).
var (
	toolLeaseRenew    = 30 * time.Second
	toolLeaseValid    = 90 * time.Second
	toolLeaseTakeover = 3 * time.Minute
	toolLeaseFuture   = 30 * time.Second
	toolLeasePoll     = 5 * time.Second
)

// Fence keys of the lease.
const (
	keyToolInstance = "tool_instance"
	keyToolEpoch    = "tool_epoch"
	keyToolSeen     = "tool_seen"
	keyPrevState    = "prev_state"
)

// ErrFenceHeld refuses a tool while another one holds the fence.
var ErrFenceHeld = errors.New("another tool holds the fence")

// ErrLeaseLost ends a tool whose lease is no longer valid: it must stop
// writing at once; running it again continues the work.
var ErrLeaseLost = errors.New("the fence lease was lost")

// fenceLease is the lease of one tool process.
type fenceLease struct {
	conn     *rados.Conn
	pool     string
	instance string
	log      *slog.Logger

	// mu serializes the writes of the fence; lastOK is when the last
	// successful write was sent (monotonic).
	mu     sync.Mutex
	lastOK time.Time
	held   bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// newFenceLease prepares the lease of this process on the fence of the
// index pool.
func newFenceLease(conn *rados.Conn, pool string, log *slog.Logger) *fenceLease {
	host, _ := os.Hostname()

	var b [6]byte

	_, _ = rand.Read(b[:])

	return &fenceLease{conn: conn, pool: pool, instance: host + "-" + hex.EncodeToString(b[:]), log: log}
}

// read reads the fence.
func (l *fenceLease) read() (Fence, error) {
	vals, ver, err := readObjectOmap(l.conn, l.pool, model.AdminNamespace, FenceOID, fenceKeys)
	if err != nil {
		return Fence{}, fmt.Errorf("read the fence: %w", err)
	}

	return fenceOf(vals, ver), nil
}

// write writes the fence: created exclusively at version 0, otherwise
// replacing every value at that version (vals nil: removing the fence
// at that version). A conflict reports false.
func (l *fenceLease) write(ver uint64, vals map[string]string) (bool, error) {
	ioctx, err := l.conn.OpenIOContext(l.pool)
	if err != nil {
		return false, fmt.Errorf("open pool %q: %w", l.pool, err)
	}
	defer ioctx.Destroy()

	ioctx.SetNamespace(model.AdminNamespace)

	op := rados.CreateWriteOp()
	defer op.Release()

	switch {
	case ver == 0:
		op.Create(rados.CreateExclusive)
	case vals == nil:
		op.AssertVersion(ver)
		op.Remove()
	default:
		op.AssertVersion(ver)
		op.CleanOmap()
	}

	if vals != nil {
		op.SetOmap(toBytes(vals))
	}

	err = op.Operate(ioctx, FenceOID, rados.OperationNoFlag)

	switch errCode(err) {
	case 0:
		return err == nil, err
	case syscall.EEXIST, syscall.ERANGE, syscall.EOVERFLOW, syscall.ENOENT:
		return false, nil
	default:
		return false, fmt.Errorf("write the fence: %w", err)
	}
}

func toBytes(vals map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(vals))
	for k, v := range vals {
		out[k] = []byte(v)
	}

	return out
}

// stamp sets the lease fields of a write by this holder.
func (l *fenceLease) stamp(f Fence, vals map[string]string) {
	epoch, _ := strconv.ParseUint(f.Values[keyToolEpoch], 10, 64)

	vals[keyToolInstance] = l.instance
	vals[keyToolEpoch] = strconv.FormatUint(epoch+1, 10)
	vals[keyToolSeen] = time.Now().UTC().Format(time.RFC3339Nano)
}

// leasedValues are the values a free fence gets with the lease: the
// neutral state "leased", the state before kept in "prev_state" (none
// for an absent fence).
func leasedValues(f Fence, by string) map[string]string {
	if !f.Exists {
		return map[string]string{pkState: FenceLeased, pkBy: by, pkAt: time.Now().UTC().Format(time.RFC3339)}
	}

	vals := maps.Clone(f.Values)
	if f.State != FenceLeased {
		vals[keyPrevState] = f.State
	}

	vals[pkState] = FenceLeased

	return vals
}

// errFenceStop ends acquire without an error of its own: plan decided
// that there is nothing to take (the caller looks at the fence).
var errFenceStop = errors.New("nothing to take")

// acquire takes the lease. plan sees every fence read and returns the
// values the fence gets with the lease, or an error that ends acquire
// (errFenceStop: nothing to do). A fence held by another instance is
// taken only once its holder has been seen idle for toolLeaseTakeover;
// a holder seen writing meanwhile refuses with ErrFenceHeld. The fence
// as read after the write is returned; the lease is renewed in the
// background until close.
func (l *fenceLease) acquire(plan func(Fence) (map[string]string, error)) (Fence, error) {
	var w holderWatch

	for {
		f, err := l.read()
		if err != nil {
			return f, err
		}

		vals, err := plan(f)
		if err != nil {
			return f, err
		}

		if wait, err := w.idle(l, f); err != nil || wait {
			if err != nil {
				return f, err
			}

			time.Sleep(toolLeasePoll)

			continue
		}

		l.stamp(f, vals)

		sent := time.Now()

		ok, err := l.write(f.Version, vals)
		if err != nil {
			return f, err
		}

		if !ok {
			continue // written meanwhile: read it again
		}

		l.mu.Lock()
		l.lastOK, l.held = sent, true
		l.mu.Unlock()

		l.renew()

		nf, err := l.read()
		if err == nil && nf.Values[keyToolInstance] != l.instance {
			err = fmt.Errorf("%w: the fence was taken by %s", ErrLeaseLost, nf.Values[keyToolInstance])
		}

		return nf, err
	}
}

// holderWatch watches the holder of a fence a tool wants to take.
type holderWatch struct {
	key  string
	from time.Time
}

// idle reports whether the tool has to wait for the holder of f (wait),
// or refuses: a holder that wrote while it was watched, or one whose
// clock is ahead. A free fence, or one of this instance, needs no wait;
// a holder seen idle for toolLeaseTakeover is taken over.
func (w *holderWatch) idle(l *fenceLease, f Fence) (bool, error) {
	holder := f.Values[keyToolInstance]
	if holder == "" || holder == l.instance {
		return false, nil
	}

	if seen, err := time.Parse(time.RFC3339Nano, f.Values[keyToolSeen]); err == nil && seen.After(time.Now().Add(toolLeaseFuture)) {
		return false, fmt.Errorf("%w: its last write (%s, by %s) lies in the future of this host's clock; fix the clocks first",
			ErrFenceHeld, f.Values[keyToolSeen], holder)
	}

	key := holder + "\x00" + f.Values[keyToolEpoch]

	switch {
	case w.key == "":
		w.key, w.from = key, time.Now()

		l.log.Info("the fence is held by another tool, waiting to see whether it is alive", "holder", holder,
			"takeover_after", toolLeaseTakeover)
	case w.key != key:
		return false, fmt.Errorf("%w: %s is working on it (state %s, run %s)", ErrFenceHeld, holder, f.State, f.Run)
	}

	if time.Since(w.from) < toolLeaseTakeover {
		return true, nil
	}

	l.log.Warn("taking over the fence lease of a tool that stopped", "holder", holder, "epoch", f.Values[keyToolEpoch])

	return false, nil
}

// valid reports whether the holder may still write: within
// toolLeaseValid of sending its last successful write.
func (l *fenceLease) valid() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.validLocked()
}

func (l *fenceLease) validLocked() bool { return l.held && time.Since(l.lastOK) < toolLeaseValid }

// check returns ErrLeaseLost when the lease is no longer valid; a
// holder calls it before every write of its work.
func (l *fenceLease) check() error {
	if !l.valid() {
		return fmt.Errorf("%w: no successful write of the fence for %s; run the tool again to continue", ErrLeaseLost, toolLeaseValid)
	}

	return nil
}

// update changes the fence as the holder: it reads it, lets mutate
// change the values (an error of mutate ends update) and writes them at
// the version read, again after a conflict as long as the lease is
// ours. release gives the lease up with the write.
func (l *fenceLease) update(mutate func(map[string]string) error, release bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for range fenceRetries {
		if !l.validLocked() {
			return fmt.Errorf("%w: no successful write of the fence for %s; run the tool again to continue", ErrLeaseLost, toolLeaseValid)
		}

		f, err := l.read()
		if err != nil {
			return err
		}

		if f.Values[keyToolInstance] != l.instance {
			l.held = false
			return fmt.Errorf("%w: the fence is held by %q", ErrLeaseLost, f.Values[keyToolInstance])
		}

		vals := maps.Clone(f.Values)
		if mutate != nil {
			if err := mutate(vals); err != nil {
				return err
			}
		}

		l.stamp(f, vals)

		if release {
			delete(vals, keyToolInstance)
			delete(vals, keyToolSeen)
		}

		sent := time.Now()

		ok, err := l.write(f.Version, vals)
		if err != nil {
			return err
		}

		if ok {
			l.lastOK, l.held = sent, !release
			return nil
		}
	}

	return fmt.Errorf("%w: the fence keeps changing", ErrLeaseLost)
}

// release gives the lease up without a state change of its own: a fence
// still "leased" goes back to what it was (deleted when there was none,
// its previous state otherwise); any other keeps its state. A lease
// that is no longer ours is left alone.
func (l *fenceLease) release() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.held {
		return nil
	}

	for range fenceRetries {
		f, err := l.read()
		if err != nil {
			return err
		}

		if !f.Exists || f.Values[keyToolInstance] != l.instance {
			l.held = false
			return nil
		}

		var vals map[string]string

		switch prev := f.Values[keyPrevState]; {
		case f.State == FenceLeased && prev == "":
			// nil: the fence goes
		case f.State == FenceLeased:
			vals = maps.Clone(f.Values)
			vals[pkState] = prev
		default:
			vals = maps.Clone(f.Values)
		}

		if vals != nil {
			delete(vals, keyPrevState)
			delete(vals, keyToolInstance)
			delete(vals, keyToolSeen)
		}

		ok, err := l.write(f.Version, vals)
		if err != nil {
			return err
		}

		if ok {
			l.held = false
			return nil
		}
	}

	return fmt.Errorf("%w: the fence keeps changing", ErrLeaseLost)
}

// renew renews the lease every toolLeaseRenew until close; a failed
// renewal is logged and the lease runs out after toolLeaseValid.
func (l *fenceLease) renew() {
	l.stop, l.done = make(chan struct{}), make(chan struct{})

	go func() {
		defer close(l.done)

		t := time.NewTicker(toolLeaseRenew)
		defer t.Stop()

		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
			}

			if !l.valid() {
				return
			}

			if err := l.update(nil, false); err != nil {
				l.log.Warn("fence lease not renewed", "err", err)

				if errors.Is(err, ErrLeaseLost) {
					return
				}
			}
		}
	}()
}

// close stops the renewal (the lease stays as it is; release first to
// give it up).
func (l *fenceLease) close() {
	if l.stop == nil {
		return
	}

	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done
}

// maxToolOpTimeout bounds the OSD operation timeout of a tool: a write
// sent within the lease window must have landed or failed before a taker
// may act (toolLeaseValid + maxToolOpTimeout < toolLeaseTakeover).
const maxToolOpTimeout = 60

// toolChunk is the largest read or write of a tool (the copy's chunk).
const toolChunk = 4 << 20

// checkToolClientOptions refuses librados options a tool cannot keep its
// lease with: an OSD operation timeout that is off or above 60 s, and an
// objecter byte throttle below parallel chunks (an operation that waits
// in the throttle has not started its timeout).
func checkToolClientOptions(opts map[string]string, parallel int) error {
	if v, ok := opts["rados_osd_op_timeout"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > maxToolOpTimeout {
			return fmt.Errorf("rados_osd_op_timeout %q: a tool needs an OSD operation timeout of 1 to %d seconds", v, maxToolOpTimeout)
		}
	}

	if v, ok := opts["objecter_inflight_ops"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < max(parallel, 1) {
			return fmt.Errorf("objecter_inflight_ops %q: a tool with %d parallel operations needs at least as many", v, max(parallel, 1))
		}
	}

	if v, ok := opts["objecter_inflight_op_bytes"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < int64(max(parallel, 1))*toolChunk {
			return fmt.Errorf("objecter_inflight_op_bytes %q: a tool with %d parallel operations needs at least %d bytes", v, max(parallel, 1), int64(max(parallel, 1))*toolChunk)
		}
	}

	return nil
}
