package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"shardpost/internal/model"
)

// Server registrations (stage B14b, §5.5): every server writes
// "servers/<instance>" at startup and then every model.ServerHeartbeat —
// independent of jobs.enabled and of an admin listener — so that
// "fts layout3 enable --pods n" can tell whether n running servers know
// layout 3. Servers before 0.45.0 do not register; that is why enable
// needs the number of pods. A record gets one entry in jobs/expiry when
// it is created, at its first seen + model.ServerExpiry; the expiry
// worker deletes a record not renewed within that time and otherwise
// moves the entry on. The hint index "ix.servers" carries version and
// capabilities only, so a heartbeat (which changes "seen") does not
// write the index.

// Server is a registration as the admin API shows it.
type Server struct {
	Instance string `json:"instance"`
	model.ServerRecord
	// Fresh: seen within model.ServerFresh.
	Fresh bool `json:"fresh"`
}

// RegisterServer writes the registration of instance. A new record gets
// its expiry entry first (completeness: the entry before the record).
func RegisterServer(kv model.AdminKV, instance, version string, now time.Time) error {
	key := model.ServersPrefix + instance

	if _, err := kv.Get(key); errors.Is(err, model.ErrNotFound) {
		if err := serverExpiry(kv, key, nil, now.Add(model.ServerExpiry)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	rec, err := json.Marshal(model.ServerRecord{Version: version, Caps: []string{model.CapFTSLayout3}, Seen: now.UTC()})
	if err != nil {
		return err
	}

	return kv.Update(key, func([]byte) ([]byte, error) { return rec, nil })
}

// RunRegistration registers the server now and then every interval
// until ctx ends (the heartbeat of the registration). Errors are logged;
// the next beat tries again.
func RunRegistration(ctx context.Context, kv model.AdminKV, instance, version string, interval time.Duration, log interface {
	Warn(string, ...any)
},
) {
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		if err := RegisterServer(kv, instance, version, time.Now()); err != nil {
			log.Warn("server registration not written", "instance", instance, "err", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// serverExpiry moves the expiry entry of a registration (none when the
// admin store keeps no queues).
func serverExpiry(kv model.AdminKV, key string, from []model.DueKey, at time.Time) error {
	ix, ok := kv.(model.IndexedKV)
	if !ok || ix.IndexMode() == model.KVIndexOff {
		return nil
	}

	q, err := ix.Queue(model.JobsExpiryQueue)
	if err != nil {
		return err
	}

	return q.Move(key, from, &model.DueKey{Due: at})
}

// Servers lists the registrations (from the hint index in read mode,
// otherwise a listing of the admin namespace) and deletes those older
// than model.ServerExpiry (the expiry worker does that too, but only
// where the job system runs).
func Servers(kv model.AdminKV, now time.Time) ([]Server, error) {
	recs, err := kv.List(model.ServersPrefix, "", 100000)
	if err != nil {
		return nil, err
	}

	out := make([]Server, 0, len(recs))

	for _, r := range recs {
		var rec model.ServerRecord
		if json.Unmarshal(r.Value, &rec) != nil {
			continue
		}

		if now.Sub(rec.Seen) > model.ServerExpiry {
			if err := deleteServer(kv, r.Key, now); err != nil {
				return nil, err
			}

			continue
		}

		out = append(out, Server{Instance: strings.TrimPrefix(r.Key, model.ServersPrefix), ServerRecord: rec,
			Fresh: now.Sub(rec.Seen) <= model.ServerFresh})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })

	return out, nil
}

// deleteServer deletes a registration that is still expired.
func deleteServer(kv model.AdminKV, key string, now time.Time) error {
	return kv.Update(key, func(cur []byte) ([]byte, error) {
		var rec model.ServerRecord
		if cur == nil || json.Unmarshal(cur, &rec) != nil || now.Sub(rec.Seen) > model.ServerExpiry {
			return nil, nil
		}

		return cur, nil
	})
}

// expireServer handles the expiry entry of a registration: a record not
// renewed within model.ServerExpiry goes with its entry, a renewed one
// moves the entry to seen + model.ServerExpiry.
func (m *Manager) expireServer(e model.DueEntry, now time.Time) error {
	var rec model.ServerRecord

	err := m.st.get(e.ID, &rec)

	switch {
	case errors.Is(err, ErrNotFound):
		return m.expiryMove(e, nil)
	case err != nil:
		return err
	case now.Sub(rec.Seen) <= model.ServerExpiry:
		return m.expiryMove(e, &model.DueKey{Due: rec.Seen.Add(model.ServerExpiry)})
	}

	if err := deleteServer(m.st.kv, e.ID, now); err != nil {
		return err
	}

	return m.expiryMove(e, nil)
}

// ErrNotReady refuses "fts layout3 enable" while the registrations do
// not show exactly the expected servers.
var ErrNotReady = errors.New("the cluster is not ready for layout 3")

// Layout3Ready checks the registrations for "fts layout3 enable --pods
// n": exactly n fresh registrations, each with the layout 3 capability.
// A pod replaced within model.ServerFresh still has its predecessor's
// fresh record: wait until it is older (F12).
func Layout3Ready(servers []Server, pods int) error {
	var fresh []string

	capable := 0

	for _, s := range servers {
		if !s.Fresh {
			continue
		}

		fresh = append(fresh, fmt.Sprintf("%s (%s, seen %s)", s.Instance, s.Version, s.Seen.UTC().Format(time.RFC3339)))

		if s.HasCap(model.CapFTSLayout3) {
			capable++
		}
	}

	if len(fresh) != pods || capable != pods {
		return fmt.Errorf("%w: %d fresh registrations (%d with %s), expected --pods %d: %s; a replaced pod's record stays fresh for %s",
			ErrNotReady, len(fresh), capable, model.CapFTSLayout3, pods, strings.Join(fresh, ", "), model.ServerFresh)
	}

	return nil
}
