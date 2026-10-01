// Package attcache remembers hasAttachment (RFC 8621 4.1.4) for messages
// whose index record does not carry it: records written before the
// record had the field (model.Meta.HasAttachment). The answers live in
// the JMAP locator's index space under "a:<guid>"; a message's content
// never changes under its GUID, so an answer never goes stale, and a
// dropped locator only costs reading the messages again.
//
// The JMAP front-end looks answers up and adds the ones a query computed;
// Backfill computes them for a whole namespace, for the front-end's
// background fill and for the maintenance job attachment_backfill.
package attcache

import (
	"context"
	"errors"
	"sort"

	"shardpost/internal/mimepart"
	"shardpost/internal/model"
)

// Space is the index space the answers are kept in: the JMAP locator's.
const Space = "jmaploc"

// Prefix is the key prefix of an answer.
const Prefix = "a:"

// Batch is how many keys one lookup or write names.
const Batch = 1000

// Value is a remembered answer.
type Value byte

// The answers. Unreadable is a message whose data is gone (no blob): it
// has no attachment a client could fetch, and reading it again would
// not change that.
const (
	No         Value = '0'
	Yes        Value = '1'
	Unreadable Value = 'u'
)

// Has is the hasAttachment the answer stands for.
func (v Value) Has() bool { return v == Yes }

// Lookup reads the remembered answers of messages; a message without one
// is missing from the result.
func Lookup(ix model.Index, guids []string) (map[string]Value, error) {
	out := make(map[string]Value, len(guids))

	for start := 0; start < len(guids); start += Batch {
		chunk := guids[start:min(start+Batch, len(guids))]

		keys := make([]string, len(chunk))
		for i, g := range chunk {
			keys[i] = Prefix + g
		}

		vals, err := ix.IndexGet(Space, keys)
		if err != nil {
			return nil, err
		}

		for k, v := range vals {
			if len(v) == 1 {
				out[k[len(Prefix):]] = Value(v[0])
			}
		}
	}

	return out, nil
}

// Remember writes answers. A key that is already there is left alone:
// it says the same.
func Remember(ix model.Index, vals map[string]Value) error {
	guids := make([]string, 0, len(vals))
	for g := range vals {
		guids = append(guids, g)
	}

	sort.Strings(guids)

	for start := 0; start < len(guids); start += Batch {
		chunk := guids[start:min(start+Batch, len(guids))]

		keys := make([]string, len(chunk))
		for i, g := range chunk {
			keys[i] = Prefix + g
		}

		err := ix.IndexApply(Space, keys, func(keys []string, cur map[string][]byte) (map[string][]byte, []string) {
			set := map[string][]byte{}

			for _, k := range keys {
				if _, ok := cur[k]; !ok {
					set[k] = []byte{byte(vals[k[len(Prefix):]])}
				}
			}

			return set, nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// Compute reads a message and answers hasAttachment for it. A message
// without data is Unreadable; any other failure is returned, since it
// may pass.
func Compute(u model.User, m *model.Meta) (Value, error) {
	raw, err := u.Read(m)

	switch {
	case errors.Is(err, model.ErrNoBlob):
		return Unreadable, nil
	case err != nil:
		return 0, err
	case mimepart.MessageHasAttachment(raw):
		return Yes, nil
	}

	return No, nil
}

// Report is what a Backfill did.
type Report struct {
	// Messages are the distinct messages without the field in their
	// index record; Known those already remembered.
	Messages int `json:"messages"`
	Known    int `json:"known"`
	// Computed were read and remembered now, Unreadable of them had no
	// data; Failed could not be read and are left for another run.
	Computed   int `json:"computed"`
	Unreadable int `json:"unreadable"`
	Failed     int `json:"failed"`
}

// Legacy lists the distinct messages of a namespace whose index record
// does not carry hasAttachment, one record per GUID, in GUID order.
func Legacy(u model.User) ([]model.Meta, error) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	byGUID := map[string]model.Meta{}

	for _, mb := range boxes {
		metas, err := u.ListMessages(mb.ID)
		if errors.Is(err, model.ErrNoMailbox) {
			continue
		}

		if err != nil {
			return nil, err
		}

		for i := range metas {
			if metas[i].HasAttachment == nil {
				byGUID[metas[i].GUID] = metas[i]
			}
		}
	}

	out := make([]model.Meta, 0, len(byGUID))
	for _, m := range byGUID {
		out = append(out, m)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].GUID < out[j].GUID })

	return out, nil
}

// Backfill remembers hasAttachment for every message of a namespace whose
// index record does not carry it. It works in batches and writes each
// one before the next, so what it did survives when it is stopped:
// check is called before every batch and ends the run with its error
// (a cancelled context, a lost job lease); progress, when set, gets the
// messages done and their total.
func Backfill(ctx context.Context, u model.User, check func() error, progress func(done, total int)) (Report, error) {
	var rep Report

	ix, ok := u.(model.Index)
	if !ok {
		return rep, errors.New("the store keeps no index for JMAP")
	}

	legacy, err := Legacy(u)
	if err != nil {
		return rep, err
	}

	rep.Messages = len(legacy)

	for start := 0; start < len(legacy); start += Batch {
		if err := stopped(ctx, check); err != nil {
			return rep, err
		}

		if err := fillBatch(ctx, u, ix, legacy[start:min(start+Batch, len(legacy))], &rep); err != nil {
			return rep, err
		}

		if progress != nil {
			progress(min(start+Batch, len(legacy)), len(legacy))
		}
	}

	return rep, nil
}

// stopped reports why a run has to end, if it has to.
func stopped(ctx context.Context, check func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if check != nil {
		return check()
	}

	return nil
}

// fillBatch computes and remembers the answers one batch lacks.
func fillBatch(ctx context.Context, u model.User, ix model.Index, batch []model.Meta, rep *Report) error {
	guids := make([]string, len(batch))
	for i := range batch {
		guids[i] = batch[i].GUID
	}

	known, err := Lookup(ix, guids)
	if err != nil {
		return err
	}

	fresh := map[string]Value{}

	for i := range batch {
		if _, ok := known[batch[i].GUID]; ok {
			rep.Known++

			continue
		}

		if ctx.Err() != nil {
			break
		}

		v, err := Compute(u, &batch[i])
		if err != nil {
			rep.Failed++

			continue
		}

		if v == Unreadable {
			rep.Unreadable++
		}

		rep.Computed++
		fresh[batch[i].GUID] = v
	}

	return Remember(ix, fresh)
}
