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
//
// Since 0.47.0 every process that writes the store's own namespaces
// registers for its whole runtime with its role (serve, push, scrub,
// backfill, sweep), its system pool and its start time, and records
// its orderly end (stopped_at): a migration of the system state checks
// that nothing runs. A process that crashed stops counting as running
// model.ServerFresh after its last heartbeat.

// Server is a registration as the admin API shows it.
type Server struct {
	Instance string `json:"instance"`
	model.ServerRecord
	// Fresh: running (model.ServerRecord.Running): seen within
	// model.ServerFresh and not stopped.
	Fresh bool `json:"fresh"`
}

// Registration is what a process registers.
type Registration struct {
	Instance string
	Version  string
	// Role is model.RoleServe, model.RolePush, ...
	Role string
	// SystemPool is the system pool the process works with (nil: none).
	SystemPool *model.PoolRef
	// StartedAt is when the process started.
	StartedAt time.Time
	// Status, when set, supplies the partitions held and the accounts
	// watched (the push worker, stage B18a); it is called at every
	// heartbeat. nil for the other roles.
	Status func() (partitions, watched int)
	// Workers, when set, records whether a server runs the background
	// workers (workers.run, stage B18b).
	Workers *bool
}

// record is the value of the registration at now.
func (r Registration) record(now time.Time) model.ServerRecord {
	rec := model.ServerRecord{Version: r.Version, Caps: []string{model.CapFTSLayout3, model.CapSystemPool, model.CapFTSDictFilter,
		model.CapFTSOnlineBuild}, Seen: now.UTC(),
		Role: r.Role, SystemPool: r.SystemPool, StartedAt: r.StartedAt.UTC()}

	if r.Status != nil {
		partitions, watched := r.Status()
		rec.Partitions, rec.Watched = &partitions, &watched
	}

	rec.Workers = r.Workers

	return rec
}

// RegisterServer writes the registration of a server (role serve) that
// started at now.
func RegisterServer(kv model.AdminKV, instance, version string, now time.Time) error {
	return Register(kv, Registration{Instance: instance, Version: version, Role: model.RoleServe, StartedAt: now}, now)
}

// Register writes the registration of r, seen at now. A new record gets
// its expiry entry first (completeness: the entry before the record).
func Register(kv model.AdminKV, r Registration, now time.Time) error {
	return writeRegistration(kv, r.Instance, r.record(now), now)
}

// Deregister records the orderly end of the process at now: its record
// stays (and ages out like any other) with stopped_at set, so it no
// longer counts as running.
func Deregister(kv model.AdminKV, r Registration, now time.Time) error {
	rec := r.record(now)
	rec.StoppedAt = now.UTC()

	return writeRegistration(kv, r.Instance, rec, now)
}

func writeRegistration(kv model.AdminKV, instance string, rec model.ServerRecord, now time.Time) error {
	key := model.ServersPrefix + instance

	if _, err := kv.Get(key); errors.Is(err, model.ErrNotFound) {
		if err := serverExpiry(kv, key, nil, now.Add(model.ServerExpiry)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	return kv.Update(key, func([]byte) ([]byte, error) { return b, nil })
}

// RunRegistration registers the process now and then every interval
// until ctx ends (the heartbeat of the registration), and then records
// its end (Deregister). Errors are logged; the next beat tries again.
func RunRegistration(ctx context.Context, kv model.AdminKV, r Registration, interval time.Duration, log interface {
	Warn(string, ...any)
},
) {
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		if err := Register(kv, r, time.Now()); err != nil {
			log.Warn("server registration not written", "instance", r.Instance, "role", r.Role, "err", err)
		}

		select {
		case <-ctx.Done():
			if err := Deregister(kv, r, time.Now()); err != nil {
				log.Warn("end of the registration not written", "instance", r.Instance, "role", r.Role, "err", err)
			}

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
			Fresh: rec.Running(now)})
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

// ErrNotReady refuses "fts layout3 enable" and "fts dictfilter enable"
// while the registrations do not show exactly the expected servers.
var ErrNotReady = errors.New("the cluster is not ready for the mark")

// Layout3Ready checks the registrations for "fts layout3 enable --pods
// n": exactly n fresh registrations of the processes that write account
// data under the mark — servers (role serve, or none: before 0.47.0)
// and, since 0.53.0, job runners — each with the layout 3 capability.
// The push worker, scrubs and backfills register too and are not
// counted. A pod replaced within model.ServerFresh that did not record
// its end still has its predecessor's fresh record: wait until it is
// older (F12).
func Layout3Ready(servers []Server, pods int) error {
	return CapReady(servers, pods, model.CapFTSLayout3)
}

// CapReady checks the registrations for an enable of a cluster mark
// with --pods n: exactly n fresh registrations of servers and runners,
// each with the capability c ("fts layout3 enable": model.CapFTSLayout3,
// "fts dictfilter enable": model.CapFTSDictFilter).
func CapReady(servers []Server, pods int, c string) error {
	var fresh []string

	capable := 0

	for _, s := range servers {
		if !s.Fresh || !s.IsStoreWriter() {
			continue
		}

		fresh = append(fresh, fmt.Sprintf("%s (%s, seen %s)", s.Instance, s.Version, s.Seen.UTC().Format(time.RFC3339)))

		if s.HasCap(c) {
			capable++
		}
	}

	if len(fresh) != pods || capable != pods {
		return fmt.Errorf("%w: %d fresh registrations of servers and runners (%d with %s), expected --pods %d: %s; "+
			"a replaced pod's record stays fresh for %s", ErrNotReady, len(fresh), capable, c, pods, strings.Join(fresh, ", "), model.ServerFresh)
	}

	return nil
}
