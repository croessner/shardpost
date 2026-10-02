package bench

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shardpost/internal/delivery"
)

// DefaultMailboxes are the mailboxes of a bench account (the six folders
// the scaling plan counts with).
const DefaultMailboxes = "INBOX,Sent,Drafts,Trash,Junk,Archive"

const inbox = "INBOX"

// specialUse is the special use of the default mailboxes.
var specialUse = map[string]string{
	"Sent": `\Sent`, "Drafts": `\Drafts`, "Trash": `\Trash`, "Junk": `\Junk`, "Archive": `\Archive`,
}

// Deliverer delivers one message the way LMTP does (delivery.Service).
type Deliverer interface {
	Deliver(ctx context.Context, req delivery.Request) (*delivery.Result, error)
}

// PopulateOptions describe a population run.
type PopulateOptions struct {
	// Prefix, First and Accounts name the accounts: Prefix followed by
	// the numbers First to First+Accounts-1.
	Prefix          string
	First, Accounts int
	// Messages is the number of messages every account ends up with.
	Messages int
	// Mailboxes are created in every account; InboxShare of the
	// messages go to INBOX through Sieve (the implicit keep), the rest
	// evenly into the other mailboxes, named in the request as
	// dovecot-lda -m does.
	Mailboxes  []string
	InboxShare float64
	Gen        Generator
	// Parallel bounds the accounts filled at the same time.
	Parallel int
	// Progress is the interval of the progress log (0: none).
	Progress time.Duration
	Log      *slog.Logger
}

// Validate checks the options.
func (o PopulateOptions) Validate() error {
	switch {
	case o.Accounts <= 0 || o.First < 0:
		return errors.New("accounts must be positive and first not negative")
	case o.Messages < 0:
		return errors.New("messages must not be negative")
	case len(o.Mailboxes) == 0 || o.Mailboxes[0] != inbox:
		return errors.New("the mailboxes must start with INBOX")
	case o.InboxShare < 0 || o.InboxShare > 1:
		return errors.New("the INBOX share must be between 0 and 1")
	case o.Parallel <= 0:
		return errors.New("parallel must be positive")
	case len(o.Gen.Sizes.Buckets) == 0:
		return errors.New("no size distribution")
	}

	return CheckPrefix(o.Prefix)
}

// ParseMailboxes splits a comma separated list of mailbox names.
func ParseMailboxes(s string) []string {
	var out []string

	for name := range strings.SplitSeq(s, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}

	return out
}

// CASSummary are the compare-and-swap retries of a run.
type CASSummary struct {
	Total       float64            `json:"total"`
	PerDelivery float64            `json:"per_delivery"`
	ByKind      map[string]float64 `json:"by_kind,omitempty"`
	// ByClass are the retries by object class (registry, mbox, fts,
	// ftsmap, ...).
	ByClass map[string]float64 `json:"by_class,omitempty"`
}

// PopulateReport is the result of a population run.
type PopulateReport struct {
	Prefix             string         `json:"prefix"`
	First              int            `json:"first"`
	Accounts           int            `json:"accounts"`
	MessagesPerAccount int            `json:"messages_per_account"`
	Mailboxes          []string       `json:"mailboxes"`
	Sizes              string         `json:"sizes"`
	Seed               uint64         `json:"seed"`
	AccountsComplete   int64          `json:"accounts_already_complete"`
	AccountsFailed     int64          `json:"accounts_failed"`
	Deliveries         int64          `json:"deliveries"`
	Failures           int64          `json:"failures"`
	Bytes              int64          `json:"bytes"`
	Seconds            float64        `json:"seconds"`
	PerSecond          float64        `json:"deliveries_per_second"`
	Latency            LatencySummary `json:"delivery_latency"`
	CASRetries         CASSummary     `json:"cas_retries"`
}

// populator is the state of one run.
type populator struct {
	o     PopulateOptions
	store delivery.Store
	d     Deliverer
	lat   Latency

	complete, failedAccts, delivered, failures, bytes atomic.Int64
}

// Populate creates the accounts and delivers their messages. It
// continues where an earlier run stopped: an account gets the messages
// numbered from its current message count up to Messages, so a re-run
// with the same options ends with the same content. The error reports
// failed accounts; the report is complete either way.
func Populate(ctx context.Context, o PopulateOptions, store delivery.Store, d Deliverer) (*PopulateReport, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}

	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	p := &populator{o: o, store: store, d: d}
	cas0 := CASRetries()
	start := time.Now()

	stop := p.progress(start)
	p.run(ctx)
	stop()

	rep := p.report(time.Since(start), cas0)

	if n := p.failedAccts.Load(); n > 0 {
		return rep, fmt.Errorf("%d of %d accounts failed", n, o.Accounts)
	}

	return rep, ctx.Err()
}

// run fills the accounts with o.Parallel workers.
func (p *populator) run(ctx context.Context) {
	next := make(chan int)

	var wg sync.WaitGroup

	for range p.o.Parallel {
		wg.Go(func() {
			for i := range next {
				if err := p.account(ctx, AccountName(p.o.Prefix, i)); err != nil {
					p.failedAccts.Add(1)
					p.o.Log.Error("bench: account failed", "account", AccountName(p.o.Prefix, i), "err", err)
				}
			}
		})
	}

	for i := p.o.First; i < p.o.First+p.o.Accounts && ctx.Err() == nil; i++ {
		next <- i
	}

	close(next)
	wg.Wait()
}

// progress logs the progress every o.Progress until the returned
// function is called.
func (p *populator) progress(start time.Time) func() {
	if p.o.Progress <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	t := time.NewTicker(p.o.Progress)

	go func() {
		for {
			select {
			case <-done:
				return
			case <-t.C:
				n := p.delivered.Load()
				p.o.Log.Info("bench: populating", "deliveries", n, "failures", p.failures.Load(),
					"per_second", round3(float64(n)/time.Since(start).Seconds()))
			}
		}
	}()

	return func() { t.Stop(); close(done) }
}

// account prepares one account and delivers its missing messages.
func (p *populator) account(ctx context.Context, name string) error {
	have, err := p.prepare(name)
	if err != nil {
		return err
	}

	if have >= p.o.Messages {
		p.complete.Add(1)
		return nil
	}

	for n := have; n < p.o.Messages; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := p.deliver(ctx, name, n); err != nil {
			p.failures.Add(1)
			return fmt.Errorf("message %d: %w", n, err)
		}
	}

	return nil
}

// prepare creates the mailboxes of an account and returns its message
// count.
func (p *populator) prepare(name string) (int, error) {
	if err := CheckAccount(name, p.o.Prefix); err != nil {
		return 0, err
	}

	u, err := p.store.User(name)
	if err != nil {
		return 0, err
	}
	defer u.Close()

	for _, mb := range p.o.Mailboxes {
		var use []string
		if s := specialUse[mb]; s != "" {
			use = []string{s}
		}

		if err := u.EnsureMailbox(mb, use); err != nil {
			return 0, fmt.Errorf("mailbox %q: %w", mb, err)
		}
	}

	q, err := u.QuotaUsage()
	if err != nil {
		return 0, err
	}

	return int(q.Msgs), nil
}

// deliver delivers message n of an account.
func (p *populator) deliver(ctx context.Context, name string, n int) error {
	raw, r := p.o.Gen.Message(name, n)

	mailbox := ""
	if others := p.o.Mailboxes[1:]; len(others) > 0 && r.Float64() >= p.o.InboxShare {
		mailbox = others[r.IntN(len(others))]
	}

	start := time.Now()

	if _, err := p.d.Deliver(ctx, Request(name, raw, mailbox)); err != nil {
		return err
	}

	p.lat.Add(time.Since(start))
	p.delivered.Add(1)
	p.bytes.Add(int64(len(raw)))

	return nil
}

// Request is the delivery request of a bench message: Sieve decides
// unless mailbox names the target.
func Request(account string, raw []byte, mailbox string) delivery.Request {
	return delivery.Request{Account: account, Rcpt: account + "@bench.invalid", From: "bench@example.org", Raw: raw, Mailbox: mailbox}
}

func (p *populator) report(took time.Duration, cas0 CASReading) *PopulateReport {
	n := p.delivered.Load()
	rep := &PopulateReport{Prefix: p.o.Prefix, First: p.o.First, Accounts: p.o.Accounts, MessagesPerAccount: p.o.Messages,
		Mailboxes: p.o.Mailboxes, Sizes: p.o.Gen.Sizes.String(), Seed: p.o.Gen.Seed,
		AccountsComplete: p.complete.Load(), AccountsFailed: p.failedAccts.Load(),
		Deliveries: n, Failures: p.failures.Load(), Bytes: p.bytes.Load(),
		Seconds: round3(took.Seconds()), Latency: p.lat.Summary(), CASRetries: CASDelta(cas0, CASRetries(), n)}

	if took > 0 {
		rep.PerSecond = round3(float64(n) / took.Seconds())
	}

	return rep
}
