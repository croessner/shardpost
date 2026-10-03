//go:build tentacle

package cephstore

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"shardpost/internal/model"
)

// pieceLog records the pieces a fake writer was handed.
type pieceLog struct {
	mu     sync.Mutex
	keys   map[string]int // "<oid>\x00<key>" → times written
	pieces []int
	fences atomic.Int64
}

func (l *pieceLog) write(p piece) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for k := range p.set {
		l.keys[p.oid+"\x00"+k]++
	}

	l.pieces = append(l.pieces, len(p.set))

	return nil
}

func (l *pieceLog) fence() error {
	l.fences.Add(1)
	return nil
}

// TestPieceWriterBuffer (temp/13 §4.7): the buffer never holds more than
// its limit (the largest piece goes early), full pieces go at
// dictWriteChunk keys, every key is written exactly once, and every write
// is fenced first.
func TestPieceWriterBuffer(t *testing.T) {
	log := &pieceLog{keys: map[string]int{}}
	w := startPieceWriter(log.write, log.fence, 5000, 4)

	want := 0

	// 40 objects with a few keys each, one with many: the buffer fills.
	for i := range 20000 {
		oid := fmt.Sprintf("o%02d", i%40)
		if i%3 == 0 {
			oid = "big"
		}

		if err := w.add(oid, fmt.Sprintf("k%d", i), nil); err != nil {
			t.Fatal(err)
		}

		want++

		if w.buffered > w.limit {
			t.Fatalf("buffer %d above the limit %d after key %d", w.buffered, w.limit, i)
		}
	}

	if err := w.finish(); err != nil {
		t.Fatal(err)
	}

	if len(log.keys) != want {
		t.Errorf("%d keys written, want %d", len(log.keys), want)
	}

	for k, n := range log.keys {
		if n != 1 {
			t.Fatalf("key %q written %d times", k, n)
		}
	}

	for _, n := range log.pieces {
		if n > dictWriteChunk {
			t.Fatalf("piece of %d keys", n)
		}
	}

	if f := log.fences.Load(); f != int64(len(log.pieces)) || w.writes.Load() != f {
		t.Errorf("fences %d, pieces %d, writes %d", f, len(log.pieces), w.writes.Load())
	}

	if w.peak > w.limit+1 {
		t.Errorf("peak %d above the limit %d", w.peak, w.limit)
	}
}

// TestPieceWriterLimit: the limit is coupled to the targets, at least
// fts.convert_buffer_keys.
func TestPieceWriterLimit(t *testing.T) {
	c := convertCfg{}.withDefaults()
	if c.bufferKeys != 262144 || c.writers != 8 {
		t.Errorf("defaults %+v", c)
	}

	if l := c.limit(100); l != 262144 {
		t.Errorf("limit for 100 targets %d", l)
	}

	if l := c.limit(644); l != 644*1024 {
		t.Errorf("limit for 644 targets %d, want 659456", l)
	}
}

// TestPieceWriterError (§10): an error of a writer ends the copy: add
// and finish return it, nothing hangs.
func TestPieceWriterError(t *testing.T) {
	boom := errors.New("boom")

	var n atomic.Int64

	write := func(piece) error {
		if n.Add(1) == 3 {
			return boom
		}

		return nil
	}

	w := startPieceWriter(write, func() error { return nil }, 100, 2)

	var addErr error

	for i := range 100000 {
		if addErr = w.add(fmt.Sprintf("o%d", i%7), fmt.Sprint(i), nil); addErr != nil {
			break
		}
	}

	if !errors.Is(addErr, boom) {
		t.Errorf("add after the failed write: %v", addErr)
	}

	if err := w.finish(); !errors.Is(err, boom) {
		t.Errorf("finish: %v", err)
	}

	// A fence that fails (a lost lease) writes nothing.
	log := &pieceLog{keys: map[string]int{}}
	lost := errors.New("lease lost")
	w = startPieceWriter(log.write, func() error { return lost }, 10, 2)

	for i := range 50 {
		if err := w.add("o", fmt.Sprint(i), nil); err != nil {
			break
		}
	}

	if err := w.finish(); !errors.Is(err, lost) || len(log.keys) != 0 {
		t.Errorf("fenced out: %v, %d keys written", err, len(log.keys))
	}
}

// TestLayoutParallelCopy (§4.7): a reshard with many writers and the
// smallest buffer keeps every search answer and reports its writes.
func TestLayoutParallelCopy(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(40))

	want := searchState(t, u)

	for _, writers := range []int{1, 16} {
		u.ftsCfg.convert = convertCfg{bufferKeys: 1, writers: writers}

		rep := convert(t, u, model.FTSLayoutRequest{Layout: 3, Shape: &model.FTSForcedShape{S: 4, D: 8, H: 2}})
		if rep.Writes == 0 || rep.BufferKeys == 0 || rep.BufferKeys > 14*bufferPerTarget+1 {
			t.Errorf("writers %d: %+v", writers, rep)
		}

		sameSearches(t, fmt.Sprintf("%d writers", writers), want, searchState(t, u))
	}
}
