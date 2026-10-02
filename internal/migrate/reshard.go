package migrate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"shardpost/internal/adminapi"
)

// The full-text index during an import (stage B14b): the importer tells
// the target how many messages it is going to store (the hint sizes a
// fresh layout 3 index), and after every batch asks the import reshard
// endpoint whether the index outgrew its objects; the target reshards it
// under the import session (a fts_layout job would wait for the import
// to end). While a reshard runs, batches of the session answer 409 and
// the importer waits for it.

// ReshardTarget is a Target whose full-text index can be resharded
// between batches.
type ReshardTarget interface {
	// Hint records the number of messages the import is going to store.
	Hint(ctx context.Context, messages int) error
	// Reshard checks the index and reshards it; it returns when no
	// reshard of the session runs any more.
	Reshard(ctx context.Context) error
}

// reshardPoll is how often the importer asks for a running reshard (a
// variable for the tests).
var reshardPoll = 5 * time.Second

// errReshardFailed is a reshard that ended with an error: the import
// goes on with the index as it is (the fts_due worker reshards later).
var errReshardFailed = errors.New("full-text reshard failed")

// Hint implements ReshardTarget: the session is opened again (renewed)
// with the hint.
func (t *APITarget) Hint(ctx context.Context, messages int) error {
	session := t.session

	resp, err := t.C.OpenImportWithResponse(ctx, t.account, adminapi.ImportSessionRequest{Session: &session, Messages: &messages})
	if err != nil {
		return err
	}

	if resp.JSON200 == nil {
		return apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return nil
}

// Reshard implements ReshardTarget.
func (t *APITarget) Reshard(ctx context.Context) error {
	resp, err := t.C.StartImportReshardWithResponse(ctx, t.account, &adminapi.StartImportReshardParams{XImportSession: t.session})
	if err != nil {
		return err
	}

	switch {
	case resp.JSON200 != nil:
		return reshardOutcome(resp.JSON200)
	case resp.JSON202 != nil, resp.StatusCode() == http.StatusConflict:
		// running here, or another run (of a session before) still works
		return t.waitReshard(ctx)
	default:
		return apiError(resp.StatusCode(), resp.JSONDefault)
	}
}

// waitReshard asks for the session's reshard until it does not run any
// more.
func (t *APITarget) waitReshard(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reshardPoll):
		}

		resp, err := t.C.GetImportReshardWithResponse(ctx, t.account, &adminapi.GetImportReshardParams{XImportSession: t.session})
		if err != nil {
			return err
		}

		if resp.JSON200 == nil {
			return apiError(resp.StatusCode(), resp.JSONDefault)
		}

		if resp.JSON200.State != "running" {
			return reshardOutcome(resp.JSON200)
		}
	}
}

// reshardOutcome maps a finished state to an error.
func reshardOutcome(st *adminapi.ImportReshard) error {
	if st.State == "failed" {
		msg := ""
		if st.Error != nil {
			msg = *st.Error
		}

		return fmt.Errorf("%w: %s", errReshardFailed, msg)
	}

	return nil
}

// reshardBusy reports the 409 of a batch while the session's reshard
// runs.
func reshardBusy(err error) bool {
	var apiErr *APIError

	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Code == string(adminapi.Conflict) &&
		strings.Contains(apiErr.Msg, "reshard")
}

// hint tells the target the number of messages of the source's
// mailboxes (STATUS MESSAGES of each; a mailbox it cannot read counts
// nothing).
func (r *run) hint(ctx context.Context, mailboxes []srcMailbox) {
	rt, ok := r.tgt.(ReshardTarget)
	if !ok {
		return
	}

	total := 0

	for _, mb := range mailboxes {
		if !mb.Selectable || mb.Err != nil {
			continue
		}

		if st, err := r.src.status(mb.Raw); err == nil {
			total += int(st.Messages)
		}
	}

	if err := rt.Hint(ctx, total); err != nil {
		r.o.Log.Warn("import hint not recorded", "err", err)
	}
}

// reshard asks the target to reshard after a batch; a failure is a
// warning (the import goes on).
func (r *run) reshard(ctx context.Context) {
	rt, ok := r.tgt.(ReshardTarget)
	if !ok {
		return
	}

	if err := rt.Reshard(ctx); err != nil {
		r.o.Log.Warn("full-text reshard between batches failed", "err", err)
	}
}

// importBatch imports a batch; one that meets the session's reshard
// waits for it and is sent again.
func (r *run) importBatch(ctx context.Context, id string, parts []adminapi.ImportPart) (*adminapi.ImportResult, error) {
	res, err := r.tgt.Import(ctx, id, parts)

	rt, ok := r.tgt.(ReshardTarget)
	if !ok || !reshardBusy(err) {
		return res, err
	}

	if werr := rt.Reshard(ctx); werr != nil {
		r.o.Log.Warn("full-text reshard during an import batch failed", "err", werr)
	}

	return r.tgt.Import(ctx, id, parts)
}
