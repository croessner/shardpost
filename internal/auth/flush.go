package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"shardpost/internal/model"
)

// The flush log: one omap object in the admin namespace that every
// server watches. A flush raises the generation h:gen and writes the
// entry f:<generation, 20 digits> = "<unix seconds>\t<account or *>" in
// the same compare-and-swap; a server applies every entry after the
// last generation it applied. Watch notifications carry nothing and may
// be coalesced, so they only wake the reader; a poll catches a lost one.
const (
	FlushObject      = "auth-flush"
	flushHeadKey     = "h:gen"
	flushEntryPrefix = "f:"
	// flushEntryEnd sorts after every entry key.
	flushEntryEnd = "f;"
)

// FlushRetention is how long a flush entry is kept; a writer removes
// older ones. A server that has not read the log for longer finds a gap
// and flushes everything.
const FlushRetention = time.Hour

// DefaultFlushPoll is how often a server reads the flush log without a
// notification.
const DefaultFlushPoll = 60 * time.Second

// flushRetries bounds the compare-and-swap rounds of one flush.
const flushRetries = 32

// flushPage is how many entries one read of the log returns.
const flushPage = 500

// FlushLog flushes the auth cache of this server and of every other
// server of the cluster.
type FlushLog struct {
	omap  model.AdminOmap
	cache *Cache
	poll  time.Duration
	log   *slog.Logger
	now   func() time.Time

	mu      sync.Mutex
	lastGen uint64
	started bool
}

// NewFlushLog follows the flush log in omap for cache; poll 0 takes
// DefaultFlushPoll.
func NewFlushLog(omap model.AdminOmap, cache *Cache, poll time.Duration, log *slog.Logger) *FlushLog {
	if poll <= 0 {
		poll = DefaultFlushPoll
	}

	if log == nil {
		log = slog.Default()
	}

	return &FlushLog{omap: omap, cache: cache, poll: poll, log: log, now: time.Now}
}

// Flush flushes the account (all entries for "" or "*") here at once and
// records the flush for the other servers. It returns before they have
// applied it: the notification is best effort, their poll the safety
// net.
func (l *FlushLog) Flush(account string) error {
	account = strings.ToLower(NormalizeAddress(account))
	if account == "" {
		account = FlushAll
	}

	l.cache.flush(account, "local")

	gen, err := l.publish(account)
	if err != nil {
		return err
	}

	// Our own entry needs no second flush when nothing came between.
	l.mu.Lock()
	if l.started && l.lastGen+1 == gen {
		l.lastGen = gen
	}
	l.mu.Unlock()

	l.omap.Notify(FlushObject)

	return nil
}

// publish appends an entry to the log and drops the expired ones.
func (l *FlushLog) publish(account string) (uint64, error) {
	for range flushRetries {
		vals, ver, err := l.omap.ReadKeys(FlushObject, []string{flushHeadKey})
		if err != nil {
			return 0, err
		}

		gen, err := parseGen(vals[flushHeadKey])
		if err != nil {
			return 0, err
		}

		rm, err := l.expired()
		if err != nil {
			return 0, err
		}

		gen++
		now := l.now()
		set := map[string][]byte{
			flushHeadKey:       []byte(strconv.FormatUint(gen, 10)),
			flushEntryKey(gen): []byte(strconv.FormatInt(now.Unix(), 10) + "\t" + account),
		}

		err = l.omap.Write(FlushObject, ver, set, rm)
		if errors.Is(err, model.ErrOmapConflict) {
			continue
		}

		if err != nil {
			return 0, err
		}

		return gen, nil
	}

	return 0, fmt.Errorf("auth flush log: %w", model.ErrOmapConflict)
}

// expired lists the entries older than FlushRetention, oldest first; it
// stops at the first younger one, since entries are written in order.
func (l *FlushLog) expired() ([]string, error) {
	cutoff := l.now().Add(-FlushRetention).Unix()

	var rm []string

	after := flushEntryPrefix

	for {
		page, more, err := l.omap.ReadPage(FlushObject, after, flushPage)
		if err != nil {
			return nil, err
		}

		for _, kv := range page {
			if kv.Key >= flushEntryEnd {
				return rm, nil
			}

			at, _, _ := strings.Cut(string(kv.Value), "\t")
			if t, err := strconv.ParseInt(at, 10, 64); err == nil && t >= cutoff {
				return rm, nil
			}

			rm = append(rm, kv.Key)
			after = kv.Key
		}

		if !more || len(rm) >= flushPage {
			return rm, nil
		}
	}
}

// flushEntryKey is the key of a generation's entry.
func flushEntryKey(gen uint64) string { return fmt.Sprintf("%s%020d", flushEntryPrefix, gen) }

// parseGen reads h:gen (absent: 0).
func parseGen(v []byte) (uint64, error) {
	if len(v) == 0 {
		return 0, nil
	}

	gen, err := strconv.ParseUint(string(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("auth flush log: bad generation %q", v)
	}

	return gen, nil
}

// Run follows the log until ctx ends: it watches the object first and
// then reads the generation, so no flush between the two is missed. A
// lost watch flushes everything (a flush may have gone by unseen) and
// is set up again; until it is, the poll reads the log.
func (l *FlushLog) Run(ctx context.Context) {
	lost, first := false, true

	for ctx.Err() == nil {
		w, err := l.omap.Watch(FlushObject)
		if err != nil {
			l.log.Warn("auth flush log: watch failed, polling", "err", err)
		}

		l.mu.Lock()
		started := l.started
		l.mu.Unlock()

		// Only the very first start knows that nothing can have been
		// missed; a later one, after a lost watch or a start that
		// failed, flushes everything.
		if lost || !started {
			if err := l.start(lost || !first); err != nil {
				l.log.Warn("auth flush log: read failed", "err", err)
			}
		} else {
			l.apply()
		}

		first = false

		if lost = l.follow(ctx, w); lost {
			// A watch that fails at once must not turn into a loop of
			// full flushes.
			select {
			case <-ctx.Done():
			case <-time.After(watchRetry):
			}
		}
	}
}

// watchRetry is the pause before a lost watch is set up again.
const watchRetry = time.Second

// start reads the generation the server has applied, and with resync
// flushes the whole cache once it has: whatever happened while the
// watch was down, or before a failed start, is unknown. The flush
// follows the read, so a store that cannot be read does not empty the
// cache on every try; nothing is lost by the order, because the watch
// is set up before the read.
func (l *FlushLog) start(resync bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.startLocked(resync)
}

// startLocked is start with l.mu held.
func (l *FlushLog) startLocked(resync bool) error {
	l.started = false

	vals, _, err := l.omap.ReadKeys(FlushObject, []string{flushHeadKey})
	if err != nil {
		return err
	}

	gen, err := parseGen(vals[flushHeadKey])
	if err != nil {
		return err
	}

	if resync {
		l.cache.flush(FlushAll, "resync")
	}

	l.lastGen, l.started = gen, true

	return nil
}

// follow reads the log on every notification and poll until ctx ends or
// the watch is lost, which it reports. Without a watch (nil w) it
// returns after the next poll, to try setting one up again.
func (l *FlushLog) follow(ctx context.Context, w model.Watch) bool {
	ticker := time.NewTicker(l.poll)
	defer ticker.Stop()

	var events <-chan struct{}

	var errs <-chan error

	if w != nil {
		defer w.Close()

		events, errs = w.Events(), w.Errors()
	}

	for {
		select {
		case <-ctx.Done():
			return false
		case <-events:
			l.apply()
		case err := <-errs:
			l.log.Warn("auth flush log: watch lost, flushing the auth cache", "err", err)
			return true
		case <-ticker.C:
			l.cache.Sweep()
			l.apply()

			if w == nil {
				return false
			}
		}
	}
}

// apply reads the entries after the last applied generation and flushes
// what they name. A gap — the next generation is gone, or the log went
// backwards — flushes everything.
func (l *FlushLog) apply() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.started {
		// The generation could not be read when the watch was set up:
		// read it now. Flushes may have gone by meanwhile, unseen, so
		// everything goes.
		if err := l.startLocked(true); err != nil {
			l.log.Warn("auth flush log: read failed", "err", err)
		}

		return
	}

	vals, _, err := l.omap.ReadKeys(FlushObject, []string{flushHeadKey})
	if err != nil {
		l.log.Warn("auth flush log: read failed", "err", err)
		return
	}

	head, err := parseGen(vals[flushHeadKey])
	if err != nil {
		l.log.Warn("auth flush log: read failed", "err", err)
		return
	}

	switch {
	case head == l.lastGen:
		return
	case head < l.lastGen:
		l.cache.flush(FlushAll, "resync")
		l.lastGen = head

		return
	}

	accounts, gap, err := l.read(head)
	if err != nil {
		l.log.Warn("auth flush log: read failed", "err", err)
		return
	}

	if gap {
		l.cache.flush(FlushAll, "resync")
	} else {
		for _, a := range accounts {
			l.cache.flush(a, "remote")
		}
	}

	l.lastGen = head
}

// read returns the accounts of the entries lastGen+1..head, and gap when
// the first of them is missing. l.mu is held.
func (l *FlushLog) read(head uint64) ([]string, bool, error) {
	var accounts []string

	after, want := flushEntryKey(l.lastGen), l.lastGen+1

	for want <= head {
		page, more, err := l.omap.ReadPage(FlushObject, after, flushPage)
		if err != nil {
			return nil, false, err
		}

		for _, kv := range page {
			if kv.Key >= flushEntryEnd {
				return accounts, want <= head, nil
			}

			gen, err := strconv.ParseUint(strings.TrimPrefix(kv.Key, flushEntryPrefix), 10, 64)
			if err != nil || gen != want {
				return nil, true, nil
			}

			_, account, _ := strings.Cut(string(kv.Value), "\t")
			if account == FlushAll {
				return []string{FlushAll}, false, nil
			}

			accounts = append(accounts, account)
			after, want = kv.Key, want+1

			if want > head {
				return accounts, false, nil
			}
		}

		if !more {
			break
		}
	}

	return accounts, want <= head, nil
}
