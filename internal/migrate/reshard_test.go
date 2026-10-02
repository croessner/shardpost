package migrate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"shardpost/internal/adminapi"
)

// reshardTarget counts the hints and reshards of a run and answers the
// first batch as the target does while the session's reshard runs.
type reshardTarget struct {
	*APITarget
	hints    []int
	reshards int
	busy     bool
}

func (t *reshardTarget) Hint(ctx context.Context, n int) error {
	t.hints = append(t.hints, n)
	return t.APITarget.Hint(ctx, n)
}

func (t *reshardTarget) Reshard(ctx context.Context) error {
	t.reshards++
	return t.APITarget.Reshard(ctx)
}

func (t *reshardTarget) Import(ctx context.Context, id string, parts []adminapi.ImportPart) (*adminapi.ImportResult, error) {
	if !t.busy {
		t.busy = true

		return nil, &APIError{Status: http.StatusConflict, Code: string(adminapi.Conflict),
			Msg: "a reshard of the account's full-text index runs for this session"}
	}

	return t.APITarget.Import(ctx, id, parts)
}

// TestMigrateReshards (N6, R4.2): migrate gives the target the number
// of the source's messages as the hint, asks for a reshard after every
// batch, and a batch that meets the session's reshard waits for it and
// is sent again.
func TestMigrateReshards(t *testing.T) {
	e := newEnv(t)
	src := user(t, e.src)

	if err := src.EnsureMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{"one", "two", "three", "four", "five"} {
		if _, err := src.Deliver("INBOX", msg(s), nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	tgt := &reshardTarget{APITarget: &APITarget{C: e.client}}

	rep, err := Run(context.Background(), Options{Account: "alice", Password: "pw", IMAP: Endpoint{Addr: e.addr, TLS: "none", Timeout: 5 * time.Second},
		BatchMessages: 2, POP3UIDLFormat: "%08Xu%08Xv"}, tgt)
	if err != nil {
		t.Fatalf("run: %v (%+v)", err, rep)
	}

	if len(tgt.hints) != 1 || tgt.hints[0] != 5 {
		t.Errorf("hints %v, want [5]", tgt.hints)
	}

	// three batches, and the wait of the batch that met the reshard
	if tgt.reshards != 4 {
		t.Errorf("%d reshard calls, want 4", tgt.reshards)
	}

	sameMailboxes(t, src, user(t, e.tgt))
}

// TestReshardOutcome: a failed reshard is an error the run reports as a
// warning; done and idle are not.
func TestReshardOutcome(t *testing.T) {
	msg := "osd gone"

	if err := reshardOutcome(&adminapi.ImportReshard{State: "failed", Error: &msg}); err == nil {
		t.Error("failed is no error")
	}

	for _, st := range []string{"done", "idle"} {
		if err := reshardOutcome(&adminapi.ImportReshard{State: st}); err != nil {
			t.Errorf("%s: %v", st, err)
		}
	}

	if reshardBusy(&APIError{Status: http.StatusConflict, Code: string(adminapi.Conflict), Msg: "import conflict: uid taken"}) {
		t.Error("an import conflict taken for a running reshard")
	}
}

// TestReshardGenerationCompared (R5.4): after a 202 the importer takes an
// idle state for finished only when the generation changed; an idle
// state with the generation it started from is a reshard that stopped
// without a switch.
func TestReshardGenerationCompared(t *testing.T) {
	poll := reshardPoll
	reshardPoll = time.Millisecond

	t.Cleanup(func() { reshardPoll = poll })

	for _, c := range []struct {
		after string
		fail  bool
	}{{"g2", false}, {"g1", true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"state":"running","g":"g1"}`))

				return
			}

			_, _ = w.Write([]byte(`{"state":"idle","g":"` + c.after + `"}`))
		}))

		client, err := adminapi.NewClientWithResponses(srv.URL)
		if err != nil {
			t.Fatal(err)
		}

		tgt := &APITarget{C: client, account: "alice", session: "session0123456789"}

		err = tgt.Reshard(context.Background())
		if c.fail != errors.Is(err, errReshardFailed) {
			t.Errorf("idle with generation %s after a 202 from g1: %v", c.after, err)
		}

		srv.Close()
	}
}
