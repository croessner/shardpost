// Package sendqueue holds the delayed JMAP submissions (scaling plan
// B10): EmailSubmission with FUTURERELEASE (RFC 8621 7, RFC 4865).
//
// Every delayed submission is one record in the admin store and one
// entry of the completeness queue "submission" (model.SubmissionQueue):
//
//	index pool, namespace "~admin":
//	  "kv.jmap/sendq/<account>/<id>"   the record (Record)
//	  "ix.submission/head|<nn>"        due queue: "<due>/<account>/<id>/<gen>"
//	  "ix.submission/p<n>/lock"        cls_lock of worker partition n
//	the account's namespaces:
//	  "sub:<guid>" in the registry and "msg/<guid>/<n>": the copy of the
//	  message (model.SubmissionCopies)
//
// The record moves through its states by compare-and-swap, each write
// raising its generation:
//
//	pending -> sending -> final | failed
//	pending -> canceled
//	sending -> pending (a temporary failure before the message was
//	                    transferred, while retries are left)
//	sending -> final (unknown result: the sending timeout passed)
//
// At most once: only the compare-and-swap pending -> sending starts a
// conversation with the relay, and nothing but the worker that made it
// ever returns the record to pending, and only for an error that
// happened before the message was transferred. A record left in sending
// (the server stopped, the relay hung) counts as final with an unknown
// result once the sending timeout has passed; it is never sent again.
// The timeout and the clocks of the servers decide only how soon that
// happens and whether a live conversation is reported as unknown, never
// whether a message goes out twice.
//
// The queue is a completeness index: an entry is written before its
// record and removed after it, so a record is never without an entry.
// The position of an entry follows from its record (Record.position): a
// reader that finds an entry elsewhere moves it there, and one without
// record (or with a newer generation) is removed after model.
// DueQueueGrace.
package sendqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"shardpost/internal/model"
)

// State is the state of a delayed submission.
type State string

// The states of a delayed submission.
const (
	StatePending  State = "pending"
	StateSending  State = "sending"
	StateFinal    State = "final"
	StateCanceled State = "canceled"
	StateFailed   State = "failed"
)

// Terminal reports whether nothing will be sent any more.
func (s State) Terminal() bool { return s == StateFinal || s == StateCanceled || s == StateFailed }

// recordPrefix is the admin store prefix of the records.
const recordPrefix = "jmap/sendq/"

// RecordKey is the admin store key of a submission's record.
func RecordKey(account, id string) string { return recordPrefix + url.PathEscape(account) + "/" + id }

// EntryID is the id of a submission in the queue.
func EntryID(account, id string) string { return account + "/" + id }

// SplitEntryID splits a queue id into account and submission id.
func SplitEntryID(eid string) (account, id string, ok bool) {
	i := strings.LastIndexByte(eid, '/')
	if i <= 0 || i == len(eid)-1 {
		return "", "", false
	}

	return eid[:i], eid[i+1:], true
}

// ValidID reports whether id can name a submission ("S" and hex digits,
// as the JMAP front-end makes them).
func ValidID(id string) bool {
	if len(id) < 2 || len(id) > 64 || id[0] != 'S' {
		return false
	}

	for _, c := range id[1:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// The values of Delivery.Delivered (RFC 8621 7).
const (
	DeliveredQueued  = "queued"
	DeliveredNo      = "no"
	DeliveredUnknown = "unknown"
)

// Delivery is what the relay said about one recipient.
type Delivery struct {
	SMTPReply string `json:"smtp_reply"`
	// Delivered is "queued", "no" or "unknown" (RFC 8621 7).
	Delivered string `json:"delivered"`
}

// Record is a delayed submission as it is kept.
type Record struct {
	Account string `json:"account"`
	ID      string `json:"id"`
	Gen     uint64 `json:"gen"`
	State   State  `json:"state"`
	// Release is when the client asked the message to be released (the
	// sendAt of the EmailSubmission); Due is when a pending submission is
	// tried next (Release, later after a temporary failure).
	Release time.Time `json:"release"`
	Due     time.Time `json:"due"`
	Created time.Time `json:"created"`
	// SendingSince is when the current attempt began (state sending).
	SendingSince time.Time `json:"sending_since,omitzero"`
	// Finished is when the record reached a terminal state.
	Finished time.Time `json:"finished,omitzero"`
	Attempts int       `json:"attempts"`
	// GUID and Size name the copy of the message.
	GUID string `json:"guid"`
	Size int64  `json:"size"`
	// The envelope.
	MailFrom string   `json:"mail_from"`
	RcptTo   []string `json:"rcpt_to"`
	// XClient names the user to the relay (Login, and ClientAddr if
	// set), as the JMAP front-end would have done.
	XClient    bool   `json:"xclient,omitempty"`
	Login      string `json:"login,omitempty"`
	ClientAddr string `json:"client_addr,omitempty"`
	// Delivery is the outcome per recipient (terminal states).
	Delivery map[string]Delivery `json:"delivery,omitempty"`
	// Unknown is set when the sending was interrupted: the message may
	// or may not have been relayed.
	Unknown bool `json:"unknown,omitempty"`
	// Reason says why a submission failed, has an unknown result, or
	// why the last attempt was retried.
	Reason string `json:"reason,omitempty"`
	// CanceledBy names who canceled ("jmap", "admin:<principal>").
	CanceledBy string `json:"canceled_by,omitempty"`
}

// position is where the record's queue entry belongs: a pending
// submission at its next attempt, one being sent at the end of its
// sending timeout, a terminal one at once (its clean-up is due).
func (r *Record) position(sendingTimeout time.Duration) model.DueKey {
	switch {
	case r.State.Terminal():
		return model.DueKey{Due: r.Finished, Gen: r.Gen}
	case r.State == StateSending:
		return model.DueKey{Due: r.SendingSince.Add(sendingTimeout), Gen: r.Gen}
	default:
		return model.DueKey{Due: r.Due, Gen: r.Gen}
	}
}

// Errors of the record operations.
var (
	// ErrNotFound: there is no record (never created, or ended and
	// cleaned up).
	ErrNotFound = errors.New("no such delayed submission")
	// ErrExists refuses a second record under one id.
	ErrExists = errors.New("the delayed submission exists")
	// errChanged aborts a compare-and-swap whose precondition no longer
	// holds.
	errChanged = errors.New("the delayed submission changed")
)

// NotPendingError refuses to cancel a submission that is no longer
// pending.
type NotPendingError struct{ State State }

func (e *NotPendingError) Error() string {
	return fmt.Sprintf("the delayed submission is %s, not pending", e.State)
}

// parseRecord decodes a record; one that does not decode is an error
// (it is never overwritten blindly).
func parseRecord(raw []byte) (*Record, error) {
	r := &Record{}
	if err := json.Unmarshal(raw, r); err != nil {
		return nil, fmt.Errorf("delayed submission record: %w", err)
	}

	return r, nil
}

func marshal(r *Record) ([]byte, error) { return json.Marshal(r) }

// get reads a record.
func get(kv model.AdminKV, account, id string) (*Record, error) {
	raw, err := kv.Get(RecordKey(account, id))

	switch {
	case errors.Is(err, model.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}

	return parseRecord(raw)
}

// update changes a record by compare-and-swap: fn sees the current
// record (ErrNotFound when there is none) and changes it in place, or
// returns an error to write nothing; it returns the record as written.
func update(kv model.AdminKV, account, id string, fn func(r *Record) error) (*Record, error) {
	var out *Record

	err := kv.Update(RecordKey(account, id), func(cur []byte) ([]byte, error) {
		if cur == nil {
			return nil, ErrNotFound
		}

		r, err := parseRecord(cur)
		if err != nil {
			return nil, err
		}

		if err := fn(r); err != nil {
			return nil, err
		}

		out = r

		return json.Marshal(r)
	})

	return out, err
}

// remove deletes a record if it is still the one given (generation);
// a record that is gone is no error.
func remove(kv model.AdminKV, rec *Record) error {
	return kv.Update(RecordKey(rec.Account, rec.ID), func(cur []byte) ([]byte, error) {
		if cur == nil {
			return nil, nil
		}

		r, err := parseRecord(cur)
		if err != nil {
			return nil, err
		}

		if r.Gen != rec.Gen {
			return nil, errChanged
		}

		return nil, nil
	})
}
