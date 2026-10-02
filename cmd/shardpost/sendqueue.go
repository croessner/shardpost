package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/imapsieve"
	"shardpost/internal/jmap"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/sendqueue"
)

// background are the workers a server runs besides its listeners: the
// IMAPSieve learning worker, the upload expiry and the delayed
// submission queue.
type background struct {
	imapSieve *imapsieve.Engine
	sendq     *sendqueue.Service
	stops     []func()
}

// stop ends the workers, the last started first.
func (b *background) stop() {
	for i := len(b.stops) - 1; i >= 0; i-- {
		b.stops[i]()
	}
}

// startBackground starts the workers.
func startBackground(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store,
	programs *extprograms.Runner, sieveCfg map[string]string,
) (*background, error) {
	b := &background{}

	imapSieve, stopWorker, err := newIMAPSieve(ctx, log, settings, store, programs, sieveCfg)
	if err != nil {
		return nil, err
	}

	b.imapSieve, b.stops = imapSieve, append(b.stops, stopWorker, startUploadExpiry(ctx, store))

	sendq, stopSendq, err := startSubmissionQueue(ctx, log, settings, store)
	if err != nil {
		b.stop()
		return nil, err
	}

	b.sendq, b.stops = sendq, append(b.stops, stopSendq)

	return b, nil
}

// startSubmissionQueue opens the delayed submission queue (scaling plan
// B10) and, with a relay and jmap.submission.delayed.worker, runs its
// worker until the returned function is called. Without the admin store
// indexes there is no queue (nil): delayed sending is then not offered,
// and a purge has no submissions to remove. The queue is opened even
// when delayed sending is not offered, so that the admin API and a purge
// see what an earlier configuration left, and the worker sends it.
func startSubmissionQueue(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store) (*sendqueue.Service, func(), error) {
	kv, err := store.AdminKV()
	if err != nil {
		return nil, nil, fmt.Errorf("submission queue: admin store: %w", err)
	}

	if kv.ConfiguredMode() == model.KVIndexOff {
		kv.Close()
		return nil, func() {}, nil
	}

	c := settings.JMAP.Submission
	d := c.Delayed

	o := sendqueue.Options{KV: kv, Accounts: importStore{store}, Log: log.With("component", "sendqueue"),
		Book:       jmap.SubmissionBook(kv, sec(c.RetentionSeconds), int(c.MaxKept)),
		Partitions: int(d.Partitions), MaxPartitions: int(d.MaxPartitions), Poll: sec(d.PollSeconds),
		SendingTimeout: sec(d.SendingTimeoutSeconds), Concurrency: int(d.Concurrency), RetryAttempts: int(d.RetryAttempts)}

	if c.Enabled() {
		o.Relay = submissionRelay(c)
	}

	svc, err := sendqueue.New(o)
	if err != nil {
		kv.Close()
		return nil, nil, fmt.Errorf("submission queue: %w", err)
	}

	if o.Relay == nil || !d.Worker {
		if d.Enabled() {
			log.Warn("delayed sending is offered, but this server runs no submission worker (jmap.submission.delayed.worker)")
		}

		return svc, kv.Close, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		svc.Run(ctx)
	}()

	return svc, func() {
		cancel()
		<-done
		kv.Close()
	}, nil
}

// submissionRelay is the relay of JMAP submissions, immediate and
// delayed alike.
func submissionRelay(c config.JMAPSubmission) *outbound.Relay {
	timeout := sec(c.TimeoutSeconds)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	relay := &outbound.Relay{Addr: c.RelayAddr, HELO: c.Helo, StartTLS: c.Starttls, Implicit: c.TLS,
		ServerName: c.ServerName, Timeout: timeout}

	if !c.XClient {
		relay.User, relay.Password = c.User, c.Password
	}

	return relay
}

// delayedOptions is delayed sending for the JMAP front-end (nil: not
// offered).
func delayedOptions(c config.JMAPDelayedSend, q *sendqueue.Service) *jmap.DelayedOptions {
	if !c.Enabled() || q == nil {
		return nil
	}

	return &jmap.DelayedOptions{Queue: q, MaxDelay: sec(c.MaxDelaySeconds)}
}
