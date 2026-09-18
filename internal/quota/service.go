package quota

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/model"
)

var events = promauto.NewCounterVec(prometheus.CounterOpts{Name: "imap_quota_events_total", Help: "Quota warnings and over-quota flag mismatches by outcome."}, []string{"event", "result"})

const (
	eventWarning  = "warning"
	eventOverFlag = "over_flag"

	resourceStorage = "storage"

	directionOver  = "over"
	directionUnder = "under"

	// maxPending caps the program runs queued or running at once; more
	// are dropped (and counted) rather than piling up goroutines.
	maxPending = 64
	// maxSeen bounds the over-flag debounce table.
	maxSeen = 10000
	// maxValue bounds a userdb value passed to a program.
	maxValue = 256
)

// Service runs the quota warning and over-quota flag programs. Runs are
// asynchronous: the store and the login never wait for them.
type Service struct {
	runner   *extprograms.Runner
	log      *slog.Logger
	warnings []config.QuotaWarning
	over     config.QuotaOverFlag
	recheck  time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	seen   map[string]flagSeen
	now    func() time.Time
}

// flagSeen is the last over-flag mismatch run for an account.
type flagSeen struct {
	over bool
	at   time.Time
}

// NewService builds the service for cfg; it returns nil when neither
// warnings nor the over-quota flag are configured.
func NewService(cfg config.Quota, runner *extprograms.Runner, log *slog.Logger) *Service {
	if len(cfg.Warnings) == 0 && !cfg.OverFlag.Enabled {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Service{runner: runner, log: log, warnings: cfg.Warnings, over: cfg.OverFlag,
		recheck: time.Duration(cfg.OverFlag.RecheckSeconds) * time.Second,
		ctx:     ctx, cancel: cancel, slots: make(chan struct{}, maxPending),
		seen: map[string]flagSeen{}, now: time.Now}
}

// Close waits for running programs until ctx ends, then kills the rest.
func (s *Service) Close(ctx context.Context) {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	done := make(chan struct{})

	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}

	s.cancel()
	<-done
}

// Changed is called by the store after the usage of account changed
// from before to after under limit (the base limit, without extras). It
// runs the first warning, in configuration order, whose threshold the
// change crossed, and corrects the over-quota flag when the change
// crossed the limit itself (an IMAP expunge makes room without a login
// or delivery that would check the flag).
func (s *Service) Changed(account string, before, after, limit model.Quota) {
	if s == nil {
		return
	}

	s.warn(account, before, after, limit)

	if s.over.Enabled {
		if wasOver, isOver := overLimit(before, limit), overLimit(after, limit); wasOver != isOver {
			s.runFlag(account, isOver, "", after, limit)
		}
	}
}

// warn runs the first warning whose threshold the change crossed.
func (s *Service) warn(account string, before, after, limit model.Quota) {
	for _, w := range s.warnings {
		b, a, l := before.Bytes, after.Bytes, limit.Bytes
		if w.Resource != resourceStorage {
			b, a, l = before.Msgs, after.Msgs, limit.Msgs
		}

		if l <= 0 {
			continue
		}

		th := config.Amount{Value: w.Percent, Percent: true}.Of(l)

		dir := w.Direction
		if dir == "" {
			dir = directionOver
		}

		crossed := b < th && a >= th
		if dir == directionUnder {
			crossed = b >= th && a < th
		}

		if !crossed {
			continue
		}

		c := runContext(account, after, limit)
		c.Vars["warning"], c.Vars["resource"], c.Vars["percent"], c.Vars["direction"] = w.Name, w.Resource, strconv.FormatInt(w.Percent, 10), dir
		c.Env["QUOTA_EVENT"], c.Env["QUOTA_WARNING"], c.Env["QUOTA_RESOURCE"] = eventWarning, w.Name, w.Resource
		c.Env["QUOTA_PERCENT"], c.Env["QUOTA_DIRECTION"] = c.Vars["percent"], dir

		s.dispatch(eventWarning, w.Program, c)

		return
	}
}

// overLimit reports whether usage reached the limit of either resource.
func overLimit(usage, limit model.Quota) bool {
	return (limit.Bytes > 0 && usage.Bytes >= limit.Bytes) || (limit.Msgs > 0 && usage.Msgs >= limit.Msgs)
}

// CheckOverFlag compares the over-quota flag in the userdb attributes
// with the real state and runs the over-flag program when they differ.
// A run for the same account and state is not repeated within the
// recheck interval.
func (s *Service) CheckOverFlag(account string, attrs map[string][]string, usage, limit model.Quota) {
	if s == nil || !s.over.Enabled {
		return
	}

	over := overLimit(usage, limit)

	flag := ""
	if vals := attrs[s.over.Attribute]; len(vals) > 0 {
		flag = printable(vals[0])
	}

	if over == strings.EqualFold(strings.TrimSpace(flag), s.over.OverValue) {
		s.forget(account)
		return
	}

	s.runFlag(account, over, flag, usage, limit)
}

// runFlag runs the over-flag program for the state over (flag is the
// value the userdb returned, "" when unknown), unless it ran for the
// same account and state within the recheck interval.
func (s *Service) runFlag(account string, over bool, flag string, usage, limit model.Quota) {
	if !s.remember(account, over) {
		return
	}

	state := directionUnder
	if over {
		state = directionOver
	}

	c := runContext(account, usage, limit)
	c.Vars["state"], c.Vars["flag"] = state, flag
	c.Env["QUOTA_EVENT"], c.Env["QUOTA_OVER"], c.Env["QUOTA_FLAG"] = eventOverFlag, strconv.FormatBool(over), flag

	if !s.dispatch(eventOverFlag, s.over.Program, c) {
		s.forget(account) // not run: the next check may try again
	}
}

// forget drops the debounce entry of account.
func (s *Service) forget(account string) {
	s.mu.Lock()
	delete(s.seen, account)
	s.mu.Unlock()
}

// remember records a mismatch run for account and state; false means
// one ran within the recheck interval.
func (s *Service) remember(account string, over bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if prev, ok := s.seen[account]; ok && prev.over == over && now.Sub(prev.at) < s.recheck {
		return false
	}

	if len(s.seen) >= maxSeen {
		for k, v := range s.seen {
			if now.Sub(v.at) >= s.recheck {
				delete(s.seen, k)
			}
		}

		if len(s.seen) >= maxSeen {
			clear(s.seen)
		}
	}

	s.seen[account] = flagSeen{over: over, at: now}

	return true
}

// printable drops control characters from a userdb value and bounds its
// length before it goes into an argument or the environment.
func printable(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}

		return r
	}, v)

	if len(v) > maxValue {
		v = strings.ToValidUTF8(v[:maxValue], "")
	}

	return v
}

// statusLine is the first line of a program's output, cleaned and
// bounded, for the log (stderr of programs is discarded).
func statusLine(out []byte) string {
	line, _, _ := strings.Cut(string(out), "\n")

	return printable(line)
}

// runContext is the program context shared by both events: the account
// and the current usage and limit of both resources.
func runContext(account string, usage, limit model.Quota) extprograms.Context {
	vars := map[string]string{
		"storage_usage":  strconv.FormatInt(usage.Bytes, 10),
		"storage_limit":  strconv.FormatInt(limit.Bytes, 10),
		"messages_usage": strconv.FormatInt(usage.Msgs, 10),
		"messages_limit": strconv.FormatInt(limit.Msgs, 10),
	}

	account = printable(account)

	env := map[string]string{"QUOTA_ACCOUNT": account}
	for k, v := range vars {
		env["QUOTA_"+strings.ToUpper(k)] = v
	}

	return extprograms.Context{Account: account, Vars: vars, Env: env}
}

// dispatch starts program in the background unless maxPending runs are
// already queued or running or the service is closed (false).
func (s *Service) dispatch(event, program string, c extprograms.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return false
	}

	select {
	case s.slots <- struct{}{}:
	default:
		events.WithLabelValues(event, "dropped").Inc()
		s.log.Warn("quota program dropped, too many pending runs", "event", event, "program", program, "user", c.Account)

		return false
	}

	events.WithLabelValues(event, "dispatched").Inc()
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()

		res, err := s.runner.Run(s.ctx, extprograms.KindQuota, program, nil, nil, c)
		status := statusLine(res.Output)

		switch {
		case err != nil:
			s.log.Warn("quota program failed", "event", event, "program", program, "user", c.Account, "err", err, "status", status)
		case res.ExitCode != 0:
			s.log.Warn("quota program failed", "event", event, "program", program, "user", c.Account, "exit", res.ExitCode, "status", status)
		default:
			s.log.Info("quota program ran", "event", event, "program", program, "user", c.Account, "status", status)
		}
	}()

	return true
}
