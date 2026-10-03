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
	"shardpost/internal/jobs"
	"shardpost/internal/learnindex"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/sendqueue"
)

// background are the workers a server runs besides its listeners: the
// IMAPSieve learning worker, the upload expiry, the delayed submission
// queue and the job system. With workers.run false (a front-end in a
// deployment with job runners, stage B18b) the server keeps the writers
// and the APIs and runs none of the workers: the learner only enqueues,
// the submission queue only schedules, the job manager only submits,
// lists and cancels.
type background struct {
	imapSieve *imapsieve.Engine
	sendq     *sendqueue.Service
	// jobs is the job manager (nil without the job system), learning the
	// learning queue (nil without one): what the admin API administers.
	jobs     *jobs.Manager
	learning *learnindex.Queue
	// kv is the admin store of the server registration (startRegistration).
	kv    *cephstore.AdminKV
	stops []func()
}

// stop ends the workers, the last started first.
func (b *background) stop() {
	for i := len(b.stops) - 1; i >= 0; i-- {
		b.stops[i]()
	}
}

// startBackground starts the workers of a server.
func startBackground(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store,
	programs *extprograms.Runner, sieveCfg map[string]string,
) (*background, error) {
	b := &background{}
	run := settings.Workers.Run

	// The registration (and its admin store) stops with the background,
	// before the caller closes the store (R4.8).
	kv, stopRegistration, err := startRegistration(ctx, log, store, model.RoleServe, nil, &run)
	if err != nil {
		return nil, err
	}

	b.kv, b.stops = kv, append(b.stops, stopRegistration)

	learner, stopLearner, err := startLearning(ctx, log, settings, store, programs, run)
	if err != nil {
		b.stop()
		return nil, err
	}

	b.imapSieve, b.stops = newIMAPSieve(log, settings, store, programs, sieveCfg, learner), append(b.stops, stopLearner)

	if run {
		b.stops = append(b.stops, startUploadExpiry(ctx, store))
	}

	sendq, stopSendq, err := startSubmissionQueue(ctx, log, settings, store, run)
	if err != nil {
		b.stop()
		return nil, err
	}

	b.sendq, b.stops = sendq, append(b.stops, stopSendq)

	if err := b.startJobs(ctx, log, settings, store, run); err != nil {
		b.stop()
		return nil, err
	}

	if !run {
		warnNoRunner(log, kv)
	}

	return b, nil
}

// startJobs opens the learning queue the admin API and the job system
// administer and, with jobs.enabled, builds the job manager; with run
// the runner runs in this process.
func (b *background) startJobs(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store, run bool) error {
	learning, err := store.LearningQueue()
	if err != nil {
		return fmt.Errorf("learning queue: %w", err)
	}

	b.learning = learning

	if !settings.Jobs.Enabled {
		return nil
	}

	mgr, jobsKV, err := newJobsManager(log, settings, store, learning, b.sendq)
	if err != nil {
		return err
	}

	b.jobs, b.stops = mgr, append(b.stops, jobsKV.Close)

	if run {
		b.stops = append(b.stops, runJobs(ctx, mgr))
	}

	return nil
}

// runJobs runs the job manager (the runner with the expiry and fts_due
// workers) until the returned function is called; running jobs are
// handed over to the other runners then.
func runJobs(ctx context.Context, mgr *jobs.Manager) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		mgr.Run(ctx)
	}()

	return func() {
		cancel()
		<-done
	}
}

// startLearning builds the learning worker of the IMAPSieve learning
// rules (nil without them) and, with run, runs it until the returned
// function is called. The worker enqueues and prepares in every case;
// only the holder of the lease runs the queue.
func startLearning(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store,
	programs *extprograms.Runner, run bool,
) (*imapsieve.LearningWorker, func(), error) {
	if !settings.IMAPSieve.Enabled || !settings.IMAPSieve.Learning.Enabled {
		return nil, func() {}, nil
	}

	queue, err := store.LearningQueue()
	if err != nil {
		return nil, nil, fmt.Errorf("learning queue: %w", err)
	}

	queue.Mode() // logs at startup when read mode waits for the backfill

	worker := imapsieve.NewLearningWorker(settings.IMAPSieve.Learning, queue, programs)
	if !run {
		return worker, func() {}, nil
	}

	warnLearningParallel(log, settings)

	workerCtx, workerCancel := context.WithCancel(ctx)
	workerDone := make(chan struct{})

	go func() { defer close(workerDone); worker.Run(workerCtx) }()

	return worker, func() { workerCancel(); <-workerDone }, nil
}

// warnLearningParallel warns when a learning program's max_parallel is
// below imapsieve.learning.concurrency: the program phase then bounds
// the learning jobs, and the concurrency is not reached.
func warnLearningParallel(log *slog.Logger, settings *config.Config) {
	want := settings.IMAPSieve.Learning.Concurrency

	for _, rule := range settings.IMAPSieve.Learning.Rules {
		p, ok := settings.ExtPrograms.Programs[rule.Program]
		if !ok {
			continue
		}

		parallel := p.MaxParallel
		if parallel <= 0 {
			parallel = extprograms.DefaultParallel
		}

		if parallel < want {
			log.Warn("the learning program's max_parallel is below imapsieve.learning.concurrency: the program phase bounds the learning jobs",
				"program", rule.Program, "max_parallel", parallel, "concurrency", want)
		}
	}
}

// warnNoRunner warns a front-end (workers.run false) once at startup when
// no process that runs the workers is registered: neither a job runner
// nor a server with the workers. Jobs, learning and delayed sending then
// wait until one runs.
func warnNoRunner(log *slog.Logger, kv *cephstore.AdminKV) {
	servers, err := jobs.Servers(kv, time.Now())
	if err != nil {
		return
	}

	// A server without the workers field (0.51 and older) runs them.
	for _, s := range servers {
		if s.Fresh && (s.Role == model.RoleRunner || (s.IsServe() && (s.Workers == nil || *s.Workers))) {
			return
		}
	}

	log.Warn("workers.run is false and no job runner (shardpost runner) or server with workers is registered: "+
		"jobs, learning and delayed sending wait until one runs", "event", "no_runner")
}

// startSubmissionQueue opens the delayed submission queue (scaling plan
// B10) and, with run, a relay and jmap.submission.delayed.worker, runs
// its worker until the returned function is called. Without the admin
// store indexes there is no queue (nil): delayed sending is then not
// offered, and a purge has no submissions to remove. The queue is opened
// even when delayed sending is not offered, so that the admin API and a
// purge see what an earlier configuration left, and the worker sends it.
func startSubmissionQueue(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store, run bool) (*sendqueue.Service, func(), error) {
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

	warnSendingTimeout(log, settings)

	o := sendqueue.Options{KV: kv, Accounts: importStore{store}, Directory: store, Log: log.With("component", "sendqueue"),
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

	// workers.run false takes precedence over delayed.worker: a front-end
	// runs no worker, whatever delayed.worker says, and is not warned
	// (the runners send).
	if !run {
		return svc, kv.Close, nil
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

// longestConversation bounds a conversation with the relay for a
// message of size bytes: a step timeout for each command but the
// recipients (connect, greeting, EHLO, STARTTLS, EHLO, XCLIENT,
// greeting, MAIL, DATA, the upload's own), the upload time at
// 256 KiB/s, and the ten minutes the relay may take to answer the
// message (outbound.Relay). Each RCPT may add a step timeout more; a
// relay that is that slow for every recipient is not budgeted.
func longestConversation(step time.Duration, size int64) time.Duration {
	const dataAnswer = 10 * time.Minute

	return 10*step + time.Duration(size/(256<<10))*time.Second + dataAnswer
}

// warnSendingTimeout warns when the sending timeout of delayed
// submissions does not exceed the longest conversation with the relay:
// such a conversation would be reported as interrupted (never sent
// twice, but the user sees "unknown").
func warnSendingTimeout(log *slog.Logger, settings *config.Config) {
	c := settings.JMAP.Submission
	if !c.Delayed.Enabled() {
		return
	}

	step := sec(c.TimeoutSeconds)
	if step <= 0 {
		step = 30 * time.Second
	}

	size := c.MaxSize
	if size <= 0 {
		size = settings.JMAP.MaxSizeUpload
	}

	if size <= 0 {
		size = jmap.DefaultLimits.MaxSizeUpload
	}

	if need, have := longestConversation(step, size), sec(c.Delayed.SendingTimeoutSeconds); have <= need {
		log.Warn("jmap.submission.delayed.sending_timeout_seconds does not exceed the longest conversation with the relay; "+
			"such a submission would end as unknown", "sending_timeout", have, "longest_conversation", need)
	}
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
