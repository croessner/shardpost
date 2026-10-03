package cephstore

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/metrics"
)

// The writes of a conversion's copy (P5, temp/13 §4.7). The copy reads
// the sources in one goroutine and adds every key of a target object to
// a buffer; a piece of an object that reaches dictWriteChunk keys is
// taken out of the buffer and handed to a queue that convert_writers
// writers work off, each write fenced first. The writers share nothing
// with the copy but the pieces they are handed. The whole buffer holds
// at most max(fts.convert_buffer_keys, 1024 × target objects) keys;
// above it the largest pending piece goes early. Coupled to the number
// of targets, a piece carries about 1024 keys or more on average, so the
// number of writes and fences stays near what full pieces need.

// Defaults of the copy's writes.
const (
	defaultConvertBufferKeys = 262144
	// bufferPerTarget is what the buffer may hold per target object.
	bufferPerTarget = 1024
)

// convertCfg are the write settings of a conversion's copy
// (fts.convert_buffer_keys, fts.convert_writers).
type convertCfg struct {
	bufferKeys, writers int
}

func (c convertCfg) withDefaults() convertCfg {
	if c.bufferKeys <= 0 {
		c.bufferKeys = defaultConvertBufferKeys
	}

	if c.writers <= 0 {
		c.writers = dictWriters
	}

	return c
}

// limit is the buffer of a copy into targets objects.
func (c convertCfg) limit(targets int) int { return max(c.bufferKeys, targets*bufferPerTarget) }

// piece is a set of keys of one object, written in one operation.
type piece struct {
	oid string
	set map[string][]byte
}

// pieceWriter buffers the keys of the target objects and has them
// written in pieces by its writers. add, finish and stop belong to the
// copy's goroutine.
type pieceWriter struct {
	write func(piece) error
	fence func() error
	limit int

	keys     map[string]map[string][]byte
	buffered int
	peak     int
	// reported is the part of imapceph_fts_convert_buffer_keys this
	// writer added: the gauge is the sum over the conversions running in
	// the process.
	reported int

	work     chan piece
	wg       sync.WaitGroup
	closed   bool
	failOnce sync.Once
	stopped  chan struct{}
	err      atomic.Pointer[error]
	writes   atomic.Int64
}

// newPieceWriter is a pieceWriter into the objects of target (the
// objects must exist in layout 3).
func (u *User) newPieceWriter(target ftsLoc, fence func() error) *pieceWriter {
	exists := target.v3()

	write := func(p piece) error {
		op := rados.CreateWriteOp()
		defer op.Release()

		if exists {
			op.AssertExists()
		}

		op.SetOmap(p.set)

		if err := op.Operate(u.idx, p.oid, rados.OperationNoFlag); err != nil {
			return fmt.Errorf("write %s: %w", p.oid, err)
		}

		return nil
	}

	cfg := u.ftsCfg.convert.withDefaults()

	return startPieceWriter(write, fence, cfg.limit(len(target.objects())), cfg.writers)
}

// startPieceWriter starts n writers that fence and write the pieces
// handed to them.
func startPieceWriter(write func(piece) error, fence func() error, limit, n int) *pieceWriter {
	w := &pieceWriter{write: write, fence: fence, limit: limit, keys: map[string]map[string][]byte{},
		work: make(chan piece), stopped: make(chan struct{})}

	for range max(n, 1) {
		w.wg.Go(w.writer)
	}

	return w
}

// writer writes the pieces it is handed until the queue closes; after
// the first error it drops them.
func (w *pieceWriter) writer() {
	for p := range w.work {
		if w.failed() != nil {
			continue
		}

		err := w.fence()
		if err == nil {
			err = w.write(p)
		}

		if err != nil {
			w.fail(err)
			continue
		}

		w.writes.Add(1)
	}
}

// fail records the first error and stops the copy.
func (w *pieceWriter) fail(err error) {
	w.failOnce.Do(func() {
		w.err.Store(&err)
		close(w.stopped)
	})
}

// failed is the first error of a writer, nil while there is none.
func (w *pieceWriter) failed() error {
	if p := w.err.Load(); p != nil {
		return *p
	}

	return nil
}

// add buffers one key of object oid; a full piece is handed over, and
// over the limit the largest pending piece. An error of a writer ends
// the copy here.
func (w *pieceWriter) add(oid, key string, v []byte) error {
	if err := w.failed(); err != nil {
		return err
	}

	set := w.keys[oid]
	if set == nil {
		set = map[string][]byte{}
		w.keys[oid] = set
	}

	if _, dup := set[key]; !dup {
		w.buffered++
		w.peak = max(w.peak, w.buffered)

		if w.buffered-w.reported >= bufferPerTarget {
			w.report()
		}
	}

	set[key] = v

	switch {
	case len(set) >= dictWriteChunk:
		return w.handOver(oid)
	case w.buffered > w.limit:
		return w.handOver(w.largest())
	}

	return nil
}

// largest is the object with the largest pending piece.
func (w *pieceWriter) largest() string {
	var (
		top  string
		size int
	)

	for oid, set := range w.keys {
		if len(set) > size || (len(set) == size && oid < top) {
			top, size = oid, len(set)
		}
	}

	return top
}

// handOver takes the pending piece of oid out of the buffer and hands it
// to a writer.
func (w *pieceWriter) handOver(oid string) error {
	set := w.keys[oid]
	delete(w.keys, oid)

	if len(set) == 0 {
		return nil
	}

	w.buffered -= len(set)
	w.report()

	select {
	case w.work <- piece{oid: oid, set: set}:
		return nil
	case <-w.stopped:
		return w.failed()
	}
}

// finish hands over what is pending, in the order of the objects, and
// waits for the writers: the first error of any of them.
func (w *pieceWriter) finish() error {
	for _, oid := range slices.Sorted(maps.Keys(w.keys)) {
		if err := w.handOver(oid); err != nil {
			break
		}
	}

	w.stop()

	return w.failed()
}

// stop ends the writers after the pieces handed over (what is still
// buffered is dropped) and waits for them.
func (w *pieceWriter) stop() {
	if w.closed {
		return
	}

	w.closed = true
	close(w.work)
	w.wg.Wait()
	w.buffered = 0
	w.report()
}

// report brings this writer's part of the buffer gauge up to date.
func (w *pieceWriter) report() {
	metrics.FTSConvertBufferKeys.Add(float64(w.buffered - w.reported))
	w.reported = w.buffered
}

// chunkWriter is the copy's view of the writer for one kind of object:
// it counts the keys per object for imapceph_fts_shard_keys.
type chunkWriter struct {
	w     *pieceWriter
	sizes map[string]int64
}

func (c *chunkWriter) add(oid, key string, v []byte) error {
	c.sizes[oid]++

	return c.w.add(oid, key, v)
}

func newChunkWriter(w *pieceWriter) *chunkWriter {
	return &chunkWriter{w: w, sizes: map[string]int64{}}
}
