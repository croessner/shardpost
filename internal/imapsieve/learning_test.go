package imapsieve

import (
	"context"
	"testing"
	"time"

	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/model"
)

type testQueue struct {
	jobs map[string]Job
	raw  map[string][]byte
}

func (q *testQueue) Put(j Job, b []byte) error {
	if _, ok := q.jobs[j.ID]; !ok {
		q.jobs[j.ID] = j

		q.raw[j.ID] = append([]byte{}, b...)
	}

	return nil
}
func (q *testQueue) List() ([]Job, error) {
	var out []Job
	for _, j := range q.jobs {
		out = append(out, j)
	}

	return out, nil
}
func (q *testQueue) Read(j Job) ([]byte, error) { return q.raw[j.ID], nil }
func (q *testQueue) Update(j Job) error         { q.jobs[j.ID] = j; return nil }
func (q *testQueue) Delete(j Job) error         { delete(q.jobs, j.ID); delete(q.raw, j.ID); return nil }

type testLease struct{}

func (testLease) Lost() bool                    { return false }
func (testLease) Release()                      {}
func (q *testQueue) Lock() (model.Lease, error) { return testLease{}, nil }

// fakePrograms records learning runs and answers with scripted exit codes.
type fakePrograms struct {
	calls int
	exits []int
	names []string
}

func (f *fakePrograms) Run(_ context.Context, kind, name string, _ []string, input []byte, _ extprograms.Context) (extprograms.Result, error) {
	f.calls++

	f.names = append(f.names, kind+":"+name+":"+string(input))
	if name == "unknown" {
		return extprograms.Result{}, extprograms.ErrNotAllowed
	}

	code := 0
	if len(f.exits) > 0 {
		code, f.exits = f.exits[0], f.exits[1:]
	}

	return extprograms.Result{ExitCode: code}, nil
}

func TestLearningRetryAndRecovery(t *testing.T) {
	q := &testQueue{map[string]Job{}, map[string][]byte{}}
	cfg := config.Learning{MaxMessageBytes: 100, MaxAttempts: 3}
	progs := &fakePrograms{exits: []int{extprograms.ExitTempFail, 0}}
	w := NewLearningWorker(cfg, q, progs)

	ev := Event{Owner: "test", MailboxID: "id", Message: model.Meta{UID: 1, ModSeq: 1}}
	if err := w.Enqueue(context.Background(), ev, "learn-spam", []byte("message")); err != nil {
		t.Fatal(err)
	}

	_ = w.Enqueue(context.Background(), ev, "learn-spam", []byte("message"))

	if len(q.jobs) != 1 {
		t.Fatal("duplicate event")
	}

	w.Step(context.Background())

	for id, j := range q.jobs {
		if j.Attempts != 1 || j.Dead {
			t.Fatalf("tempfail must retry: %+v", j)
		}

		j.Next = time.Time{}
		q.jobs[id] = j
	}

	w = NewLearningWorker(cfg, q, progs)
	w.Step(context.Background())

	if progs.calls != 2 || len(q.jobs) != 0 || progs.names[0] != "learning:learn-spam:message" {
		t.Fatalf("restart/retry failed: %d %v", progs.calls, progs.names)
	}
}

func TestLearningPermanentFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit int
	}{{"learn-spam", 1}, {"unknown", 0}} {
		q := &testQueue{map[string]Job{}, map[string][]byte{}}
		w := NewLearningWorker(config.Learning{MaxMessageBytes: 100, MaxAttempts: 5}, q, &fakePrograms{exits: []int{tc.exit}})

		ev := Event{Owner: "test", MailboxID: "id", Message: model.Meta{UID: 1, ModSeq: 1}}
		if err := w.Enqueue(context.Background(), ev, tc.name, []byte("msg")); err != nil {
			t.Fatal(err)
		}

		w.Step(context.Background())

		for _, j := range q.jobs {
			if !j.Dead {
				t.Fatalf("%s: permanent failure must not retry: %+v", tc.name, j)
			}
		}
	}
}

func TestLearningRules(t *testing.T) {
	e := &Engine{Config: config.IMAPSieve{Learning: config.Learning{Enabled: true, Rules: []config.LearningRule{{Operations: []string{"MOVE"}, SourceSpecialUse: `\Junk`, DestinationNotSpecialUse: []string{`\Junk`, `\Trash`}, Program: "learn-ham"}}}}}

	ev := Event{Operation: "MOVE", Actor: "a", Owner: "a", SourceUses: []string{`\Junk`}}
	if e.learningAction(ev) != "learn-ham" {
		t.Fatal("ham rule")
	}

	ev.DestinationUses = []string{`\Trash`}
	if e.learningAction(ev) != "" {
		t.Fatal("trash learned as ham")
	}

	ev.DestinationUses = nil

	ev.Operation = "APPEND"
	if e.learningAction(ev) != "" {
		t.Fatal("import learned")
	}
}

func (q *testQueue) Ready(Job) (bool, error) { return true, nil }

func (q *testQueue) Cleanup(Job) error { return nil }

func TestLegacyQueueIdentifiers(t *testing.T) {
	for _, name := range []string{"spam", "ham"} {
		q := &testQueue{map[string]Job{"old": {ID: "old", Action: name, Size: 3}}, map[string][]byte{"old": []byte("msg")}}
		programs := &fakePrograms{}
		w := NewLearningWorker(config.Learning{MaxMessageBytes: 100, MaxAttempts: 5}, q, programs)
		w.Step(context.Background())

		if len(q.jobs) != 0 || len(programs.names) != 1 || programs.names[0] != "learning:"+name+":msg" {
			t.Fatal("legacy job not delivered")
		}
	}
}
