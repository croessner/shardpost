package sendqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
)

// Submitter hands a message to the relay (outbound.Relay).
type Submitter interface {
	Submit(ctx context.Context, as *outbound.XClient, from string, to []string, raw []byte) (*outbound.Report, error)
}

// Opener opens accounts. The worker reads and deletes the copies with
// it, so it must not refuse an account under an import or maintenance
// lock.
type Opener interface {
	User(name string) (model.User, error)
}

// Book is the JMAP view of an account's submissions: when a submission
// ends, its undoStatus and deliveryStatus are written there. Settle
// changes an existing entry only (a destroyed EmailSubmission stays
// destroyed) and is idempotent.
type Book interface {
	Settle(rec *Record) error
}

// Defaults of the options.
const (
	DefaultPartitions     = 1
	MaxPartitions         = 64
	DefaultPoll           = 30 * time.Second
	DefaultSendingTimeout = time.Hour
	DefaultConcurrency    = 4
	DefaultRetryAttempts  = 5
	DefaultRetryBase      = time.Minute
	// maxRetryDelay caps the backoff between attempts.
	maxRetryDelay = 30 * time.Minute
	// CopyHold is how long after the release time the copy of a message
	// is kept for sure: a scrub takes it for debris only after that (the
	// worker deletes it as soon as the submission ends).
	CopyHold = 7 * 24 * time.Hour
	// leaseTTL is the TTL of a partition's cls_lock; it is renewed every
	// third of it.
	leaseTTL = 3 * time.Minute
	// countLimit bounds the entries counted for the queue gauge.
	countLimit = 100000
)

// Outcomes (label "outcome" of imapceph_jmap_delayed_submissions_total).
const (
	outcomeScheduled = "scheduled"
	outcomeSent      = "sent"
	outcomeCanceled  = "canceled"
	outcomeFailed    = "failed"
	outcomeUnknown   = "unknown"
	outcomeRetried   = "retried"
	outcomeConflict  = "conflict"
)

// Stale entry actions (label "action" of imapceph_queue_stale_entries_total).
const (
	staleRescheduled = "rescheduled"
	staleRemoved     = "removed"
	staleSkipped     = "skipped"
)

// Options configure a Service.
type Options struct {
	// KV keeps the records and the queue; its index mode must not be
	// off.
	KV model.IndexedKV
	// Accounts opens the accounts whose copies are read and deleted.
	Accounts Opener
	// Relay sends (the worker only).
	Relay Submitter
	// Book is the JMAP view (nil: none).
	Book Book
	// Directory is the account directory: a submission of an account
	// that is gone or being purged is not sent (nil: not checked).
	Directory model.AccountDirectory
	Log       *slog.Logger
	// Partitions is the number of worker partitions (accounts by hash);
	// MaxPartitions bounds how many one server holds (0: all).
	Partitions, MaxPartitions int
	// Poll is how often a worker looks without a notification.
	Poll time.Duration
	// SendingTimeout is how long a submission may stay in sending before
	// it counts as final with an unknown result.
	SendingTimeout time.Duration
	// Concurrency bounds the conversations with the relay one worker
	// holds at once.
	Concurrency int
	// RetryAttempts bounds the attempts of a submission whose relay
	// failed temporarily before the message was transferred; RetryBase is
	// the first backoff, doubled per attempt (capped at 30 minutes).
	RetryAttempts int
	RetryBase     time.Duration
	// Grace is how long an entry without record is left alone
	// (model.DueQueueGrace when 0).
	Grace time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// Service schedules, cancels and sends delayed submissions.
type Service struct {
	o Options
	q model.DueQueue
}

// ErrNoQueue is returned by New when the admin store has no queues.
var ErrNoQueue = errors.New("delayed sending needs the admin store indexes (admin.kv_index write or read)")

// New returns the service over the admin store.
func New(o Options) (*Service, error) {
	if o.KV == nil || o.KV.IndexMode() == model.KVIndexOff {
		return nil, ErrNoQueue
	}

	q, err := o.KV.Queue(model.SubmissionQueue)
	if err != nil {
		return nil, err
	}

	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	if o.Now == nil {
		o.Now = time.Now
	}

	o.Partitions = min(max(o.Partitions, 0), MaxPartitions)
	if o.Partitions == 0 {
		o.Partitions = DefaultPartitions
	}

	defaults := []struct {
		v *time.Duration
		d time.Duration
	}{{&o.Poll, DefaultPoll}, {&o.SendingTimeout, DefaultSendingTimeout}, {&o.RetryBase, DefaultRetryBase},
		{&o.Grace, model.DueQueueGrace}}
	for _, x := range defaults {
		if *x.v <= 0 {
			*x.v = x.d
		}
	}

	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}

	if o.RetryAttempts <= 0 {
		o.RetryAttempts = DefaultRetryAttempts
	}

	return &Service{o: o, q: q}, nil
}

// Spec is a submission to schedule.
type Spec struct {
	Account, ID string
	Release     time.Time
	MailFrom    string
	RcptTo      []string
	XClient     bool
	Login       string
	ClientAddr  string
	// View is the JMAP view of the submission (opaque here), kept so
	// that the view can be written when the submission ends even if its
	// entry is missing (Book.Settle).
	View []byte
	// ViewSeq is the sequence of the view's creation in the account's
	// change log: Settle re-creates a missing view only while the log
	// still reaches back to it (else it may have been destroyed).
	ViewSeq uint64
	// ViewEpoch is the epoch of the account's records the view was
	// created in: an account purged and created again under the same
	// name has records of another epoch.
	ViewEpoch string
	// Raw is the message as it is sent (Bcc removed).
	Raw []byte
}

// ErrNoCopies is returned when the store cannot keep message copies.
var ErrNoCopies = errors.New("the store cannot keep submission copies")

// Schedule stores a delayed submission: the copy of the message, the
// queue entry, then the record (entry before record). u is the account,
// opened by the caller.
func (s *Service) Schedule(u model.User, sp Spec) (*Record, error) {
	copies, ok := u.(model.SubmissionCopies)
	if !ok {
		return nil, ErrNoCopies
	}

	if !ValidID(sp.ID) {
		return nil, fmt.Errorf("invalid submission id %q", sp.ID)
	}

	guid, err := copies.PutSubmissionCopy(sp.ID, sp.Raw, sp.Release.Add(CopyHold))
	if err != nil {
		return nil, fmt.Errorf("submission copy: %w", err)
	}

	now := s.o.Now().UTC()
	rec := &Record{Account: sp.Account, ID: sp.ID, Gen: 1, State: StatePending, Release: sp.Release.UTC(),
		Due: sp.Release.UTC(), Created: now, GUID: guid, Size: int64(len(sp.Raw)), MailFrom: sp.MailFrom,
		RcptTo: sp.RcptTo, XClient: sp.XClient, Login: sp.Login, ClientAddr: sp.ClientAddr, View: sp.View, ViewSeq: sp.ViewSeq,
		ViewEpoch: sp.ViewEpoch}

	eid, pos := EntryID(sp.Account, sp.ID), rec.position(s.o.SendingTimeout)

	undo := func() {
		if err := copies.DropSubmissionCopy(guid); err != nil {
			s.o.Log.Warn("sendqueue: copy of a submission that was not scheduled left behind", "guid", guid, "err", err)
		}
	}

	if err := s.q.Move(eid, nil, &pos); err != nil {
		undo()
		return nil, fmt.Errorf("submission queue: %w", err)
	}

	err = s.o.KV.Update(RecordKey(sp.Account, sp.ID), func(cur []byte) ([]byte, error) {
		if cur != nil {
			return nil, ErrExists
		}

		return marshal(rec)
	})
	if err != nil {
		// The entry stays: a write that failed may still have stored the
		// record (a timeout after the OSD applied it), and a record must
		// not lose its entry. Without the copy a worker fails such a
		// record and cleans it up, or drops the entry after the grace
		// when there is none.
		undo()

		return nil, fmt.Errorf("submission record: %w", err)
	}

	count(outcomeScheduled)
	s.q.Notify()

	return rec, nil
}

// Get reads the record of a submission (ErrNotFound when there is none).
func (s *Service) Get(account, id string) (*Record, error) { return get(s.o.KV, account, id) }

// Cancel cancels a pending submission: the record turns canceled by
// compare-and-swap (a worker that started to send has made it sending
// first, and then the cancel is refused with NotPendingError), then the
// copy, the record and the entry go. by names who canceled.
func (s *Service) Cancel(account, id, by string) (*Record, error) {
	var pre Record

	now := s.o.Now().UTC()

	rec, err := update(s.o.KV, account, id, func(r *Record) error {
		if r.State != StatePending {
			return &NotPendingError{State: r.State}
		}

		pre = *r
		r.State, r.Gen, r.Finished, r.CanceledBy = StateCanceled, r.Gen+1, now, by

		for _, rc := range r.RcptTo {
			if r.Delivery == nil {
				r.Delivery = map[string]Delivery{}
			}

			r.Delivery[rc] = Delivery{Delivered: DeliveredNo}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	count(outcomeCanceled)
	s.move(rec, []model.DueKey{pre.position(s.o.SendingTimeout)})

	if err := s.settle(rec, nil); err != nil {
		// The entry is due at once: a worker finishes the clean-up.
		s.o.Log.Warn("sendqueue: clean-up after a cancel left to the worker", "account", account, "id", id, "err", err)
	}

	return rec, nil
}

// List returns the records of an account's submissions that still hold
// a copy: pending, being sent, or ended and not yet cleaned up. It reads
// the account's intents and one record per copy, no listing.
func (s *Service) List(account string) ([]*Record, error) {
	u, err := s.o.Accounts.User(account)
	if err != nil {
		return nil, err
	}
	defer u.Close()

	copies, ok := u.(model.SubmissionCopies)
	if !ok {
		return nil, ErrNoCopies
	}

	list, err := copies.SubmissionCopies()
	if err != nil {
		return nil, err
	}

	out := make([]*Record, 0, len(list))

	for _, c := range list {
		rec, err := get(s.o.KV, account, c.ID)

		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			return nil, err
		}

		out = append(out, rec)
	}

	return out, nil
}

// ListAll returns up to limit records of every account whose keys sort
// after after, and the key to continue from ("" at the end). It lists
// the admin namespace (a diagnosis tool, not a running path): it finds
// records that List cannot, such as one whose copy and entry are lost.
func (s *Service) ListAll(after string, limit int) ([]*Record, string, error) {
	entries, err := s.o.KV.List(recordPrefix, after, limit)
	if err != nil {
		return nil, "", err
	}

	out := make([]*Record, 0, len(entries))

	for _, e := range entries {
		rec, err := parseRecord(e.Value)
		if err != nil {
			rec = &Record{ID: e.Key, Reason: err.Error()}
		}

		out = append(out, rec)
	}

	next := ""
	if len(entries) == limit && limit > 0 {
		next = entries[len(entries)-1].Key
	}

	return out, next, nil
}

// PurgeAccount removes the records and queue entries of an account's
// submissions, named by its copies (scaling plan B9); the copies
// themselves go with the account's namespaces. A submission being sent
// loses its record all the same: the worker's write after sending then
// fails, and nothing is sent again. It returns how many it removed.
func (s *Service) PurgeAccount(u model.User, account string) (int, error) {
	copies, ok := u.(model.SubmissionCopies)
	if !ok {
		return 0, nil
	}

	list, err := copies.SubmissionCopies()
	if err != nil {
		return 0, err
	}

	n := 0

	for _, c := range list {
		if !ValidID(c.ID) {
			continue
		}

		var gone *Record

		err := s.o.KV.Update(RecordKey(account, c.ID), func(cur []byte) ([]byte, error) {
			gone = nil

			if cur != nil {
				gone, _ = parseRecord(cur)
			}

			return nil, nil
		})
		if err != nil {
			return n, fmt.Errorf("submission record %s: %w", c.ID, err)
		}

		if gone != nil {
			n++

			// A record that did not decode leaves its entry, which a
			// worker removes after the grace.
			if gone.ID == c.ID {
				if err := s.q.Move(EntryID(account, c.ID), []model.DueKey{gone.position(s.o.SendingTimeout)}, nil); err != nil {
					s.o.Log.Warn("sendqueue: purge left a queue entry", "account", account, "id", c.ID, "err", err)
				}
			}
		}
	}

	return n, nil
}

// settle finishes a terminal submission: the JMAP view, the copy, the
// record, the entry (with the further positions in extra), in this
// order, so that whatever a crash interrupts is found again through the
// entry.
func (s *Service) settle(rec *Record, extra []model.DueKey) error {
	if s.o.Book != nil {
		if err := s.o.Book.Settle(rec); err != nil {
			return fmt.Errorf("jmap view: %w", err)
		}
	}

	if err := s.dropCopy(rec); err != nil {
		return err
	}

	if err := remove(s.o.KV, rec); err != nil {
		return fmt.Errorf("remove record: %w", err)
	}

	from := append(extra, rec.position(s.o.SendingTimeout))

	if err := s.q.Move(EntryID(rec.Account, rec.ID), from, nil); err != nil {
		return fmt.Errorf("remove queue entry: %w", err)
	}

	return nil
}

// dropCopy deletes the copy of a submission.
func (s *Service) dropCopy(rec *Record) error {
	u, err := s.o.Accounts.User(rec.Account)
	if err != nil {
		return fmt.Errorf("open account: %w", err)
	}
	defer u.Close()

	copies, ok := u.(model.SubmissionCopies)
	if !ok {
		return ErrNoCopies
	}

	if err := copies.DropSubmissionCopy(rec.GUID); err != nil && !errors.Is(err, model.ErrNoBlob) {
		return fmt.Errorf("drop copy: %w", err)
	}

	return nil
}

// move moves the entry of rec from the positions from to its own.
func (s *Service) move(rec *Record, from []model.DueKey) {
	to := rec.position(s.o.SendingTimeout)

	if err := s.q.Move(EntryID(rec.Account, rec.ID), from, &to); err != nil {
		// The record stands; a reader moves the entry it finds.
		s.o.Log.Warn("sendqueue: queue entry not moved", "account", rec.Account, "id", rec.ID, "err", err)
	}
}

func count(outcome string) { metrics.Submissions.WithLabelValues(outcome).Inc() }

func stale(action string) { metrics.QueueStale.WithLabelValues(model.SubmissionQueue, action).Inc() }
