package attcache

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

const (
	rawFile  = "Subject: f\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\ntext\r\n--b\r\nContent-Type: application/pdf\r\n\r\n%PDF\r\n--b--\r\n"
	rawPlain = "Subject: p\r\n\r\ntext\r\n"
)

// oldUser is a view whose index records lack hasAttachment, as records
// written before the field existed do; reads of the GUIDs in gone fail
// with ErrNoBlob.
type oldUser struct {
	model.User
	model.Index
	gone   map[string]bool
	broken map[string]bool // reads fail with another error
	panics map[string]bool // reads panic
	badBox string          // ListMessages of this mailbox fails
	reads  int
}

func strip(metas []model.Meta) []model.Meta {
	for i := range metas {
		metas[i].HasAttachment = nil
	}

	return metas
}

func (u *oldUser) ListMessages(id string) ([]model.Meta, error) {
	if id == u.badBox {
		return nil, errors.New("omap read failed")
	}

	metas, err := u.User.ListMessages(id)

	return strip(metas), err
}

func (u *oldUser) Read(m *model.Meta) ([]byte, error) {
	u.reads++

	switch {
	case u.gone[m.GUID]:
		return nil, model.ErrNoBlob
	case u.broken[m.GUID]:
		return nil, errors.New("rados: timeout")
	case u.panics[m.GUID]:
		panic("bad message")
	}

	return u.User.Read(m)
}

func oldAccount(t *testing.T, msgs ...string) (*oldUser, []*model.Meta) {
	t.Helper()

	u, _ := memstore.New().User("a")
	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	var metas []*model.Meta

	for _, raw := range msgs {
		m, err := u.Deliver("INBOX", []byte(raw), nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}

		metas = append(metas, m)
	}

	return &oldUser{User: u, Index: u.(model.Index), gone: map[string]bool{}, broken: map[string]bool{},
		panics: map[string]bool{}}, metas
}

// TestBackfill: every old message is read once and remembered; a second
// run reads nothing; a message without data is remembered as such.
func TestBackfill(t *testing.T) {
	u, metas := oldAccount(t, rawFile, rawPlain, rawPlain)
	u.gone[metas[2].GUID] = true

	var last [2]int

	rep, err := Backfill(context.Background(), u, nil, func(done, total int) { last = [2]int{done, total} })
	if err != nil || fmt.Sprint(rep) != fmt.Sprint(Report{Messages: 3, Computed: 3, Unreadable: 1}) || last != [2]int{3, 3} {
		t.Fatalf("first run: %+v %v, progress %v", rep, err, last)
	}

	checkRemembered(t, u, metas)

	rep, err = Backfill(context.Background(), u, nil, nil)
	if err != nil || rep.Known != 3 || rep.Computed != 0 || u.reads != 3 {
		t.Errorf("second run: %+v %v after %d reads", rep, err, u.reads)
	}
}

// checkRemembered checks the answers TestBackfill wrote.
func checkRemembered(t *testing.T, u *oldUser, metas []*model.Meta) {
	t.Helper()

	got, err := Lookup(u, []string{metas[0].GUID, metas[1].GUID, metas[2].GUID, "nope"})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]Value{metas[0].GUID: Yes, metas[1].GUID: No, metas[2].GUID: Unreadable}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("remembered %v, want %v", got, want)
	}
}

// TestBackfillFailures: a message that fails to read, or makes the
// parser panic, is counted and left for the next run, which reads it
// again; a mailbox that cannot be listed is skipped and counted.
func TestBackfillFailures(t *testing.T) {
	u, metas := oldAccount(t, rawFile, rawPlain, rawPlain)
	u.broken[metas[0].GUID] = true
	u.panics[metas[1].GUID] = true

	rep, err := Backfill(context.Background(), u, nil, nil)
	if err != nil || rep.Failed != 2 || rep.Computed != 1 || len(rep.Errors) != 2 {
		t.Fatalf("first run: %+v %v", rep, err)
	}

	delete(u.broken, metas[0].GUID)
	delete(u.panics, metas[1].GUID)

	rep, err = Backfill(context.Background(), u, nil, nil)
	if err != nil || rep.Failed != 0 || rep.Computed != 2 || rep.Known != 1 {
		t.Errorf("second run: %+v %v", rep, err)
	}
}

// TestBackfillSkipsMailbox: a mailbox that cannot be listed is skipped
// and counted, not the end of the run.
func TestBackfillSkipsMailbox(t *testing.T) {
	u, _ := oldAccount(t, rawFile)

	boxes, _ := u.ListMailboxes()
	u.badBox = boxes[0].ID

	rep, err := Backfill(context.Background(), u, nil, nil)
	if err != nil || rep.SkippedMailboxes != 1 || rep.Messages != 0 || len(rep.Errors) != 1 {
		t.Errorf("a broken mailbox: %+v %v", rep, err)
	}
}

// TestBackfillStops: a check that fails ends the run before it reads.
func TestBackfillStops(t *testing.T) {
	u, _ := oldAccount(t, rawFile)
	stop := errors.New("lease lost")

	if _, err := Backfill(context.Background(), u, func() error { return stop }, nil); !errors.Is(err, stop) || u.reads != 0 {
		t.Errorf("a stopped run returned %v after %d reads", err, u.reads)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Backfill(ctx, u, nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled run returned %v", err)
	}
}

// TestRememberKeepsAndBatches: an answer already there is not
// overwritten, and more keys than one batch are written and read.
func TestRememberKeepsAndBatches(t *testing.T) {
	u, _ := oldAccount(t)

	vals := map[string]Value{}
	guids := make([]string, 0, Batch+5)

	for i := 0; i < Batch+5; i++ {
		g := fmt.Sprintf("%032x", i)
		vals[g] = Yes
		guids = append(guids, g)
	}

	if err := Remember(u, map[string]Value{guids[0]: No}); err != nil {
		t.Fatal(err)
	}

	if err := Remember(u, vals); err != nil {
		t.Fatal(err)
	}

	got, err := Lookup(u, guids)
	if err != nil || len(got) != Batch+5 || got[guids[0]] != No || got[guids[Batch+4]] != Yes {
		t.Errorf("read back %d answers, first %c (%v)", len(got), got[guids[0]], err)
	}
}
