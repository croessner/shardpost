//go:build tentacle

package cephstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/imapsieve"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// Tests of the system tools (migrate, verify, abort, revert, cleanup,
// status) against the cluster, on the test pools of system_test.go.

// plainConfig is the configuration of a server of the test pools before
// the migration: the store's own namespaces in the index pool.
func plainConfig(cfg Config) Config {
	c := serverConfig(cfg)
	c.SystemPool = ""

	return c
}

// quickChecks makes the watcher notify short for a test.
func quickChecks(t *testing.T) {
	t.Helper()

	shortLease(t)

	old := watchTimeout
	watchTimeout = 200 * time.Millisecond

	t.Cleanup(func() { watchTimeout = old })
}

// bigData is the size of the raw object that the copy reads and writes
// in chunks.
const bigData = 9<<20 + 123

// populate writes the store's own state through a server without a
// system pool, as an existing cluster has it, and closes the server: an
// admin record, an expired push lease, a finished job, a directory entry,
// a learning job, queue entries, server metadata, an unlocked lock, and
// raw objects with chunked data, many omap keys and an xattr. It returns
// the account.
func populate(t *testing.T, tool *Store, cfg Config) string {
	t.Helper()

	s, err := Open(plainConfig(cfg), quiet())
	if err != nil {
		t.Fatal(err)
	}

	account := testAccount(t)
	populateRecords(t, s)
	populateAccount(t, s, account)

	l, err := s.holdLock(model.LearningNamespace, learningWorkerOID, "maildrop", "test", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}

	l.Release()

	if !s.Drain(10 * time.Second) {
		t.Fatal("populate: the server did not drain")
	}

	s.Close()

	populateRaw(t, tool, cfg.IndexPool)

	return account
}

// populateRecords writes admin records.
func populateRecords(t *testing.T, s *Store) {
	t.Helper()

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	past := time.Now().Add(-time.Hour).UTC()
	records := map[string]string{
		"routing/x":       `{"x":1}`,
		"jmap/push/lease": fmt.Sprintf(`{"holder":"old","until":%q}`, past.Format(time.RFC3339)),
		"job/j1":          fmt.Sprintf(`{"id":"j1","state":"succeeded","lease":{"until":%q}}`, past.Format(time.RFC3339)),
	}

	for k, v := range records {
		if err := kv.Update(k, func([]byte) ([]byte, error) { return []byte(v), nil }); err != nil {
			t.Fatal(err)
		}
	}
}

// populateAccount registers an account and fills the queues and the
// server metadata.
func populateAccount(t *testing.T, s *Store, account string) {
	t.Helper()

	u, err := s.User(account)
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	u.Close()

	q, err := s.LearningQueue()
	if err != nil {
		t.Fatal(err)
	}

	job := imapsieve.Job{ID: newGUID(), Owner: account, MailboxID: "m", Next: time.Now(), Action: "learn"}
	if err := q.Put(job, []byte("message")); err != nil {
		t.Fatal(err)
	}

	queueWrites(t, s, account)

	server, err := s.User(model.ServerNamespace)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	val := "global"
	if err := server.SetMetadata("", "", map[string]*string{"/shared/imapsieve/script": &val}); err != nil {
		t.Fatal(err)
	}
}

// populateRaw writes raw objects into the admin namespace of pool: one
// with data over several chunks and an xattr, one with more omap keys
// than a page.
func populateRaw(t *testing.T, s *Store, pool string) {
	t.Helper()

	data := bytes.Repeat([]byte("0123456789abcdef"), bigData/16+1)[:bigData]

	keys := map[string][]byte{}
	for i := range 2500 {
		keys[fmt.Sprintf("k%05d", i)] = []byte(strings.Repeat("v", i%40))
	}

	err := s.WithConn(func(conn *rados.Conn) error {
		c, err := conn.OpenIOContext(pool)
		if err != nil {
			return err
		}
		defer c.Destroy()

		c.SetNamespace(model.AdminNamespace)

		if err := c.WriteFull("kv.raw/big", data); err != nil {
			return err
		}

		if err := c.SetXattr("kv.raw/big", "user.test", []byte("x")); err != nil {
			return err
		}

		return c.SetOmap("ix.raw/00", keys)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// snapshot is the digest of every movable object of pool by name (the
// quarantine of run counted under its namespace when run is set).
func snapshot(t *testing.T, s *Store, pool, run string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := s.WithConn(func(conn *rados.Conn) error {
		objs, err := listAllOf(conn, pool, func(ns, oid string) bool {
			return (run == "" && movable(ns, oid)) || (run != "" && isQuarantine(ns, run))
		})
		if err != nil {
			return err
		}

		c, err := conn.OpenIOContext(pool)
		if err != nil {
			return err
		}
		defer c.Destroy()

		for _, o := range objs {
			c.SetNamespace(o[0])

			im, err := readImage(c, o[1])
			if err != nil {
				return err
			}

			ns, _, _ := strings.Cut(o[0], ".moved/")
			out[objName(ns, o[1])] = im.digest(o[1])
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// crashStep makes the n-th call of step fail like a crash (the lease is
// left as it is).
func crashStep(t *testing.T, step string, n int) error {
	t.Helper()

	crash := fmt.Errorf("%w: crash at %s", ErrLeaseLost, step)

	var (
		mu    sync.Mutex
		calls int
	)

	systemStep = func(s string) error {
		mu.Lock()
		defer mu.Unlock()

		if s != step {
			return nil
		}

		if calls++; calls == n {
			return crash
		}

		return nil
	}

	t.Cleanup(func() { systemStep = func(string) error { return nil } })

	return crash
}

// noCrash ends crashStep.
func noCrash() { systemStep = func(string) error { return nil } }

// migrate runs a migration of the test pools.
func migrate(tool *Store, cfg Config, o SystemOptions) (*SystemReport, error) {
	o.Parallel = max(o.Parallel, 4)
	o.Log = quiet()

	return tool.MigrateSystem(cfg.SystemPool, o)
}

// mustMigrate runs a migration that must succeed.
func mustMigrate(t *testing.T, tool *Store, cfg Config, o SystemOptions) *SystemReport {
	t.Helper()

	rep, err := migrate(tool, cfg, o)
	if err != nil {
		t.Fatalf("migrate: %v (report %+v)", err, rep)
	}

	return rep
}

// wantErr checks that err wraps target.
func wantErr(t *testing.T, err, target error, what string) {
	t.Helper()

	if !errors.Is(err, target) {
		t.Fatalf("%s: %v, want %v", what, err, target)
	}
}

// checkMigrated checks the end of a migration: the fence done, the mark
// active, the copy equal to the source before it, the old place holding
// the fence and the poisoned heads only, the quarantine holding the
// source.
func checkMigrated(t *testing.T, tool *Store, cfg Config, before map[string]string) Fence {
	t.Helper()

	f := checkCommitted(t, tool)

	if got := snapshot(t, tool, cfg.SystemPool, ""); !maps.Equal(got, before) {
		t.Fatalf("the system pool differs from the source: %d objects, %d before", len(got), len(before))
	}

	if got := snapshot(t, tool, cfg.IndexPool, f.Run); !maps.Equal(got, before) {
		t.Fatalf("the quarantine differs from the source: %d objects, %d before", len(got), len(before))
	}

	heads, err := fenceHeads(f)
	if err != nil || len(heads) == 0 {
		t.Fatalf("heads %v %v", heads, err)
	}

	for name := range snapshot(t, tool, cfg.IndexPool, "") {
		if !slices.Contains(heads, name) || !isHead(strings.SplitN(name, "/", 2)[1]) {
			t.Errorf("left at the old place: %s", name)
		}
	}

	return f
}

// checkCommitted checks the fence done without a lease and the mark
// active of the same run.
func checkCommitted(t *testing.T, tool *Store) Fence {
	t.Helper()

	_, f, m := inputs(t, tool)
	if f.State != FenceDone || f.Values[keyToolInstance] != "" {
		t.Fatalf("fence %+v", f.Values)
	}

	if !m.Exists || m.State != MarkActive || m.Origin != OriginMigrated || m.Run != f.Run {
		t.Fatalf("mark %+v", m.Values)
	}

	return f
}

// TestMigrateSystem: a migration copies everything, a server on the
// system pool reads the same state, the old place keeps the fence and
// the poisoned heads, and verify and status agree.
func TestMigrateSystem(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	account := populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	rep := mustMigrate(t, tool, cfg, SystemOptions{})
	if !rep.Done || rep.Objects != len(before) || rep.Copied != len(before) {
		t.Fatalf("report %+v", rep)
	}

	if rep.Poisoned == 0 || rep.Digests[digestTotal] == "" {
		t.Fatalf("report %+v", rep)
	}

	f := checkMigrated(t, tool, cfg, before)

	wantRefusal(t, plainConfig(cfg), "set ceph.system_pool")

	s := mustOpen(t, serverConfig(cfg))
	checkServerState(t, s, account)

	if rep, err := tool.VerifySystem(SystemOptions{Log: quiet(), Parallel: 4, SourceOnly: true}); err != nil {
		t.Fatalf("verify --source-only: %v %+v", err, rep)
	}

	checkStatus(t, tool, f.Run)

	again := mustMigrate(t, tool, cfg, SystemOptions{})
	if !again.Already || again.Run != f.Run {
		t.Fatalf("second migrate: %+v", again)
	}
}

// checkStatus: status shows the run, its phase and the mark, and counts
// the namespaces.
func checkStatus(t *testing.T, tool *Store, run string) {
	t.Helper()

	st, err := tool.SystemStatus(SystemOptions{Log: quiet(), Deep: true})
	if err != nil || st.Run != run || st.Phase != PhaseM7 {
		t.Fatalf("status %+v %v", st, err)
	}

	if st.Mark[pkState] != MarkActive || len(st.Namespaces) == 0 {
		t.Fatalf("status %+v", st)
	}
}

// checkServerState reads what populate wrote through a server on the
// system pool.
func checkServerState(t *testing.T, s *Store, account string) {
	t.Helper()

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	if v, err := kv.Get("routing/x"); err != nil || string(v) != `{"x":1}` {
		t.Fatalf("record: %q %v", v, err)
	}

	dir, release, err := s.AccountIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := dir.Get(account); err != nil {
		t.Fatalf("directory entry of %s: %v", account, err)
	}

	q, err := s.LearningQueue()
	if err != nil {
		t.Fatal(err)
	}

	// The learning copies are in the data pool all tests share.
	jobs, err := q.List()
	if err != nil || !slices.ContainsFunc(jobs, func(j imapsieve.Job) bool { return j.Owner == account }) {
		t.Fatalf("no learning job of %s: %d jobs, %v", account, len(jobs), err)
	}

	server, err := s.User(model.ServerNamespace)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	if md, err := server.GetMetadata("", ""); err != nil || md["/shared/imapsieve/script"] != "global" {
		t.Fatalf("server metadata %v %v", md, err)
	}
}

// TestMigratePoisonedHeads: a store without the placement check (a
// version before 0.47.0) fails on the poisoned heads instead of reading
// an empty directory, and what it writes raw at the old place makes
// verify --source-only fail.
func TestMigratePoisonedHeads(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	account := populate(t, tool, cfg)
	mustMigrate(t, tool, cfg, SystemOptions{})

	old := plainConfig(cfg)
	old.PlacementTool = true
	s := mustOpen(t, old)

	// A process of its own would not have the heads of this one cached.
	sortedindex.ForgetHeads()

	dir, release, err := s.AccountIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := dir.Get(account); !errors.Is(err, model.ErrIndexFormat) {
		t.Fatalf("directory of an old server: %v", err)
	}

	// Its admin store's gate cannot read the poisoned head and falls back
	// to write mode: the record lands raw at the old place (R2.7).
	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}

	err = kv.Update("jmap/push/lease", func([]byte) ([]byte, error) { return []byte(`{"holder":"old"}`), nil })

	kv.Close()

	if err != nil {
		t.Fatalf("a record of an old server: %v", err)
	}

	rep, err := tool.VerifySystem(SystemOptions{Log: quiet(), SourceOnly: true})
	wantErr(t, err, ErrSystemMismatch, "verify after a raw record")

	if !slices.Contains(rep.Surplus, objName(model.AdminNamespace, pushLeaseOID)) {
		t.Fatalf("surplus %v", rep.Surplus)
	}
}

// TestMigrateCheck: --check reports running processes without refusing
// and writes nothing; a held lock refuses.
func TestMigrateCheck(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	running(t, tool, cfg)

	rep, err := migrate(tool, cfg, SystemOptions{Check: true})
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	checks := map[string]bool{}
	for _, f := range rep.Findings {
		checks[f.Check] = true

		if f.Blocking {
			t.Errorf("blocking finding in --check: %+v", f)
		}
	}

	if !checks[checkRegistrations] || !checks[checkLeases] {
		t.Fatalf("findings %+v", rep.Findings)
	}

	if _, f, _ := inputs(t, tool); f.Exists {
		t.Fatalf("--check wrote the fence: %+v", f)
	}
}

// TestMigrateCheckLocks: a held worker lock is reported in --check, not
// refused (the servers hold their locks while they serve); the
// migration itself refuses it, and a target that is not empty still
// refuses --check.
func TestMigrateCheckLocks(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)

	s := mustOpen(t, plainConfig(cfg))

	l, err := s.holdLock(model.AdminNamespace, "ix.kv/lock", "backfill", "test", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	rep, err := migrate(tool, cfg, SystemOptions{Check: true})
	if err != nil {
		t.Fatalf("check with a held lock: %v", err)
	}

	if !slices.ContainsFunc(rep.Findings, func(f Finding) bool {
		return f.Check == checkLocks && f.Object == objName(model.AdminNamespace, "ix.kv/lock") && !f.Blocking
	}) {
		t.Fatalf("check with a held lock: findings %+v", rep.Findings)
	}

	rep, err = migrate(tool, cfg, SystemOptions{})
	wantErr(t, err, ErrSystemRefused, "migration with a held lock")

	if !slices.ContainsFunc(rep.Findings, func(f Finding) bool { return f.Check == checkLocks && f.Blocking }) {
		t.Fatalf("migration with a held lock: findings %+v", rep.Findings)
	}

	if _, f, _ := inputs(t, tool); f.Exists {
		t.Fatalf("a refused migration left a fence: %+v", f.Values)
	}

	// A wrong target still refuses --check.
	l.Release()
	writeRaw(t, tool, cfg.SystemPool, model.AdminNamespace, "stray", []byte("x"))

	_, err = migrate(tool, cfg, SystemOptions{Check: true})
	wantErr(t, err, ErrSystemRefused, "check with a used target")

	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("check with a used target: %v", err)
	}
}

// running writes the records of processes that run: a server
// registration of a version before 0.47.0, a valid push lease and a
// running job.
func running(t *testing.T, tool *Store, cfg Config) {
	t.Helper()

	now := time.Now().UTC()
	reg, _ := json.Marshal(model.ServerRecord{Version: "0.46.0", Seen: now})
	writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, serverPrefix+"old-pod", reg)
	writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, pushLeaseOID,
		fmt.Appendf(nil, `{"holder":"push-1","until":%q}`, now.Add(time.Minute).Format(time.RFC3339)))
	writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, jobPrefix+"j2",
		fmt.Appendf(nil, `{"id":"j2","state":"running","lease":{"owner":"pod-1","until":%q}}`, now.Add(time.Minute).Format(time.RFC3339)))
}

// writeRaw writes an object with data.
func writeRaw(t *testing.T, s *Store, pool, ns, oid string, data []byte) {
	t.Helper()

	err := s.WithConn(func(conn *rados.Conn) error {
		c, err := conn.OpenIOContext(pool)
		if err != nil {
			return err
		}
		defer c.Destroy()

		c.SetNamespace(ns)

		return c.WriteFull(oid, data)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMigrateRefusesRunning: every sign of a running process refuses the
// migration (exit 2) and leaves no fence in effect (K1); a registration
// is ignored with ForceRegistrations.
func TestMigrateRefusesRunning(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)

	now := time.Now().UTC()
	cases := []struct {
		name, oid string
		data      []byte
	}{
		{"registration", serverPrefix + "pod-a", mustJSON(json.Marshal(model.ServerRecord{Version: "0.47.0", Role: model.RolePush,
			Caps: []string{model.CapSystemPool}, Seen: now}))},
		{"old registration", serverPrefix + "pod-b", mustJSON(json.Marshal(model.ServerRecord{Version: "0.46.0", Seen: now}))},
		{"push lease", pushLeaseOID, fmt.Appendf(nil, `{"holder":"p","until":%q}`, now.Add(time.Minute).Format(time.RFC3339))},
		{"job", jobPrefix + "j3", fmt.Appendf(nil, `{"state":"running","lease":{"until":%q}}`, now.Add(time.Minute).Format(time.RFC3339))},
	}

	for _, c := range cases {
		writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, c.oid, c.data)

		rep, err := migrate(tool, cfg, SystemOptions{})
		wantErr(t, err, ErrSystemRefused, c.name)

		if !slices.ContainsFunc(rep.Findings, func(f Finding) bool { return f.Blocking && f.Object == c.oid }) {
			t.Fatalf("%s: findings %+v", c.name, rep.Findings)
		}

		if _, f, _ := inputs(t, tool); f.Exists {
			t.Fatalf("%s: a refused migration left a fence: %+v", c.name, f.Values)
		}

		mustOpen(t, plainConfig(cfg))

		if strings.Contains(c.name, "registration") {
			if _, err := migrate(tool, cfg, SystemOptions{Check: true, ForceRegistrations: true}); err != nil && !strings.Contains(err.Error(), "lease") {
				t.Fatalf("%s with force: %v", c.name, err)
			}
		}

		deleteRaw(t, tool, cfg.IndexPool, model.AdminNamespace, c.oid)
	}

	refusesWatcher(t, tool, cfg)
}

// mustJSON returns v or panics.
func mustJSON[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}

	return v
}

// deleteRaw deletes an object.
func deleteRaw(t *testing.T, s *Store, pool, ns, oid string) {
	t.Helper()

	err := s.WithConn(func(conn *rados.Conn) error {
		c, err := conn.OpenIOContext(pool)
		if err != nil {
			return err
		}
		defer c.Destroy()

		c.SetNamespace(ns)

		return c.Delete(oid)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// refusesWatcher: a watch on the auth flush log refuses, naming the
// watcher.
func refusesWatcher(t *testing.T, tool *Store, cfg Config) {
	t.Helper()

	putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, authFlushOID, map[string]string{"h:gen": "1"})

	err := tool.WithConn(func(conn *rados.Conn) error {
		c, err := conn.OpenIOContext(cfg.IndexPool)
		if err != nil {
			return err
		}

		c.SetNamespace(model.AdminNamespace)

		w, err := c.Watch(authFlushOID)
		if err != nil {
			c.Destroy()

			return err
		}

		go func() {
			for ev := range w.Events() {
				_ = ev.Ack(nil)
			}
		}()

		t.Cleanup(func() {
			_ = w.Delete()

			c.Destroy()
		})

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := migrate(tool, cfg, SystemOptions{})
	wantErr(t, err, ErrSystemRefused, "a watcher")

	if !slices.ContainsFunc(rep.Findings, func(f Finding) bool { return f.Check == checkWatchers }) {
		t.Fatalf("findings %+v", rep.Findings)
	}
}

// TestMigrateResumes: a migration that crashed at any step is completed
// by the next run, which takes the dead holder's lease over; the result
// is the same as without the crash.
func TestMigrateResumes(t *testing.T) {
	quickChecks(t)

	steps := []struct {
		step string
		n    int
	}{
		{"lease", 1}, {"fence", 1}, {PhaseM1, 1}, {"M3:object", 7}, {PhaseM3, 1}, {PhaseM4, 1}, {"M5:start", 1},
		{"M5:object", 5}, {"M5:head-copied", 2}, {"M5:head-saved", 2}, {"M5:head", 2}, {PhaseM5, 1}, {PhaseM6, 1},
		{"done", 1}, {PhaseM7, 1},
	}

	for _, s := range steps {
		t.Run(fmt.Sprintf("%s-%d", strings.ReplaceAll(s.step, ":", "-"), s.n), func(t *testing.T) {
			tool, cfg := migPools(t)
			populate(t, tool, cfg)
			before := snapshot(t, tool, cfg.IndexPool, "")

			crash := crashStep(t, s.step, s.n)

			if _, err := migrate(tool, cfg, SystemOptions{}); !errors.Is(err, crash) {
				t.Fatalf("crash: %v", err)
			}

			noCrash()

			if _, err := migrate(tool, cfg, SystemOptions{}); !errors.Is(err, ErrFenceHeld) && s.step != "lease" {
				t.Fatalf("resume without --wait against the dead lease: %v", err)
			}

			mustMigrate(t, tool, cfg, SystemOptions{Wait: true})
			checkMigrated(t, tool, cfg, before)
		})
	}
}

// TestMigrateHalfCopy: a copy that stopped half way (part of the data,
// a few omap keys) is replaced; a source that changes between M3 and M4
// is a mismatch (exit 3), and the next run copies it again (M3 is
// repeated until M4 completed).
func TestMigrateHalfCopy(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	crash := crashStep(t, PhaseM1, 1)
	if _, err := migrate(tool, cfg, SystemOptions{}); !errors.Is(err, crash) {
		t.Fatalf("crash: %v", err)
	}

	// What a copy that stopped in the middle leaves: part of the data,
	// a few of the omap keys.
	writeRaw(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.raw/big", []byte("0123"))
	putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, "ix.raw/00", map[string]string{"k00000": ""})

	systemStep = func(step string) error {
		if step == PhaseM3 {
			writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, "kv.routing/x", []byte(`{"x":2}`))
		}

		return nil
	}

	rep, err := migrate(tool, cfg, SystemOptions{Wait: true})
	wantErr(t, err, ErrSystemMismatch, "a source changed after M3")

	if !slices.ContainsFunc(rep.Mismatches, func(s string) bool { return strings.Contains(s, "kv.routing/x") }) {
		t.Fatalf("mismatches %v", rep.Mismatches)
	}

	noCrash()
	writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, "kv.routing/x", []byte(`{"x":1}`))
	mustMigrate(t, tool, cfg, SystemOptions{})
	checkMigrated(t, tool, cfg, before)
}

// TestMigrateHeadCopyTampered: a head already poisoned whose quarantine
// copy was changed is never overwritten: exit 3.
func TestMigrateHeadCopyTampered(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)

	_ = crashStep(t, "M5:head", 1)

	if _, err := migrate(tool, cfg, SystemOptions{}); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	rs := runStateOf(t, tool, cfg)

	var poisoned string

	for k := range rs {
		if name, ok := strings.CutPrefix(k, rsHeadPrefix); ok {
			poisoned = name
		}
	}

	ns, oid := splitName(poisoned)
	putOmap(t, tool, cfg.IndexPool, quarantineNS(ns, rs[rsRun]), oid, map[string]string{"shards": "99"})

	_, err := migrate(tool, cfg, SystemOptions{Wait: true})
	wantErr(t, err, ErrSystemMismatch, "a tampered head copy")
}

// runStateOf reads the run state of the test pools.
func runStateOf(t *testing.T, s *Store, cfg Config) map[string]string {
	t.Helper()

	var out map[string]string

	err := s.WithConn(func(conn *rados.Conn) error {
		c, err := conn.OpenIOContext(cfg.SystemPool)
		if err != nil {
			return err
		}
		defer c.Destroy()

		c.SetNamespace(model.AdminNamespace)

		vals, err := c.GetAllOmapValues(runStateOID, "", "", 1000)
		out = strValues(vals)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// TestMigrateHeadSavedChanged: a head whose original values were saved
// but which changed before it was poisoned is a mismatch (R3.10); an
// unchanged one is poisoned on resume.
func TestMigrateHeadSavedChanged(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)

	_ = crashStep(t, "M5:head-saved", 1)

	if _, err := migrate(tool, cfg, SystemOptions{}); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	rs := runStateOf(t, tool, cfg)

	for k := range rs {
		if name, ok := strings.CutPrefix(k, rsHeadPrefix); ok {
			ns, oid := splitName(name)
			putOmap(t, tool, cfg.IndexPool, ns, oid, map[string]string{"shards": "5", "format": "1", "phase": "stable"})
		}
	}

	_, err := migrate(tool, cfg, SystemOptions{Wait: true})
	wantErr(t, err, ErrSystemMismatch, "a saved head that changed")
}

// TestMigrateLeaseLost: a lease taken by another tool in the middle of
// the copy stops every write; the run is continued later.
func TestMigrateLeaseLost(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	var once sync.Once

	systemStep = func(step string) error {
		if step == "M3:object" {
			once.Do(func() {
				_, f, _ := inputs(t, tool)
				vals := maps.Clone(f.Values)
				vals[keyToolInstance] = "thief"
				putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, FenceOID, vals)
				time.Sleep(4 * toolLeaseRenew)
			})
		}

		return nil
	}

	t.Cleanup(noCrash)

	_, err := migrate(tool, cfg, SystemOptions{})
	wantErr(t, err, ErrLeaseLost, "a stolen lease")

	noCrash()

	if _, f, _ := inputs(t, tool); f.Values[keyToolInstance] != "thief" {
		t.Fatalf("the tool wrote the fence after losing the lease: %+v", f.Values)
	}

	mustMigrate(t, tool, cfg, SystemOptions{Wait: true})
	checkMigrated(t, tool, cfg, before)
}

// TestMigrateParallel: two migrations at once: one works, the other is
// refused or finds it done.
func TestMigrateParallel(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	other := cfg
	other.PlacementTool = true
	second := mustOpen(t, other)

	var (
		wg   sync.WaitGroup
		errs [2]error
	)

	for i, s := range []*Store{tool, second} {
		wg.Go(func() { _, errs[i] = migrate(s, cfg, SystemOptions{}) })
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil && !errors.Is(err, ErrSystemRefused) {
			t.Fatalf("results %v", errs)
		}
	}

	if errs[0] != nil && errs[1] != nil {
		t.Fatalf("both refused: %v", errs)
	}

	checkMigrated(t, tool, cfg, before)
}

// TestMigrateDoneCompletesRunState: a fence already done with the run
// state behind is completed (R3.9).
func TestMigrateDoneCompletesRunState(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)

	_ = crashStep(t, "done", 1)

	if _, err := migrate(tool, cfg, SystemOptions{}); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	if rs := runStateOf(t, tool, cfg); rs[rsPhase] != PhaseM6 {
		t.Fatalf("phase %s", rs[rsPhase])
	}

	rep := mustMigrate(t, tool, cfg, SystemOptions{Wait: true})
	if rs := runStateOf(t, tool, cfg); !rep.Done || rs[rsPhase] != PhaseM7 || rs[rsReport] == "" {
		t.Fatalf("report %+v, run state %v", rep, rs[rsPhase])
	}
}

// TestAbortUnchanged: an abort before the quarantine only reverts the
// fence (case a); a new migration then needs the earlier run confirmed,
// and deletes its copies (R3.8).
func TestAbortUnchanged(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	_ = crashStep(t, PhaseM3, 1)

	if _, err := migrate(tool, cfg, SystemOptions{}); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	_, f, _ := inputs(t, tool)

	rep, err := tool.AbortSystem(SystemOptions{Log: quiet(), Run: f.Run, Wait: true})
	if err != nil || !rep.Done {
		t.Fatalf("abort: %v %+v", err, rep)
	}

	if _, f, _ := inputs(t, tool); f.State != FenceReverted {
		t.Fatalf("fence %+v", f.Values)
	}

	if got := snapshot(t, tool, cfg.IndexPool, ""); !maps.Equal(got, before) {
		t.Fatal("abort changed the source")
	}

	mustOpen(t, plainConfig(cfg))

	_, err = migrate(tool, cfg, SystemOptions{})
	wantErr(t, err, ErrSystemRefused, "a new run over the copies of the aborted one")

	if !strings.Contains(err.Error(), f.Run) {
		t.Fatalf("the refusal does not name the earlier run: %v", err)
	}

	rep = mustMigrate(t, tool, cfg, SystemOptions{Confirm: f.Run})
	if rep.Run == f.Run || rep.Deleted == 0 {
		t.Fatalf("report %+v", rep)
	}

	checkMigrated(t, tool, cfg, before)
}

// TestAbortRestores: an abort after the quarantine (case b) brings the
// old place back to the digest of M4, retires the mark, keeps the
// quarantine; an abort that stopped is continued (R4.1); migrate refuses
// a run being aborted; cleanup deletes the quarantine of an aborted run.
func TestAbortRestores(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	account := populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	_ = crashStep(t, PhaseM6, 1)

	if _, err := migrate(tool, cfg, SystemOptions{}); err == nil {
		t.Fatal("no crash")
	}

	_, f, _ := inputs(t, tool)
	_ = crashStep(t, "abort:object", 3)

	if _, err := tool.AbortSystem(SystemOptions{Log: quiet(), Run: f.Run, Wait: true}); err == nil {
		t.Fatal("no crash in the abort")
	}

	noCrash()

	if rs := runStateOf(t, tool, cfg); rs[rsPhase] != PhaseAbort || rs[rsAbortFrom] != PhaseM6 {
		t.Fatalf("run state %s %s", rs[rsPhase], rs[rsAbortFrom])
	}

	_, err := migrate(tool, cfg, SystemOptions{Wait: true})
	wantErr(t, err, ErrSystemRefused, "migrate during an abort")

	if _, err := tool.AbortSystem(SystemOptions{Log: quiet(), Run: f.Run, Wait: true}); err != nil {
		t.Fatalf("second abort: %v", err)
	}

	_, f2, m := inputs(t, tool)
	if f2.State != FenceReverted || m.State != MarkRetired {
		t.Fatalf("fence %+v, mark %+v", f2.Values, m.Values)
	}

	if got := snapshot(t, tool, cfg.IndexPool, ""); !maps.Equal(got, before) {
		t.Fatal("the old place differs from the source after the abort")
	}

	if len(snapshot(t, tool, cfg.IndexPool, f.Run)) == 0 {
		t.Fatal("the abort deleted the quarantine")
	}

	s := mustOpen(t, plainConfig(cfg))
	checkServerState(t, s, account)

	cleanupAborted(t, tool, cfg, f.Run)
}

// cleanupAborted: cleanup takes the quarantine of an aborted run once it
// is old enough.
func cleanupAborted(t *testing.T, tool *Store, cfg Config, run string) {
	t.Helper()

	_, err := tool.CleanupSystem(SystemOptions{Log: quiet(), Run: run, Confirm: run})
	wantErr(t, err, ErrSystemRefused, "cleanup of a young abort")

	// A late write of a stopped tool removes an original after the
	// abort (K3): cleanup keeps the quarantine, a second abort repairs.
	deleteRaw(t, tool, cfg.IndexPool, model.AdminNamespace, "kv.routing/x")

	_, err = tool.CleanupSystem(SystemOptions{Log: quiet(), Run: run, Confirm: run, MinAge: time.Nanosecond})
	wantErr(t, err, ErrSystemMismatch, "cleanup over a changed old place")

	if rep, err := tool.AbortSystem(SystemOptions{Log: quiet(), Run: run}); err != nil || rep.Restored != 1 || rep.Already {
		t.Fatalf("the repeated abort: %v %+v", err, rep)
	}

	if rep, err := tool.AbortSystem(SystemOptions{Log: quiet(), Run: run}); err != nil || !rep.Already {
		t.Fatalf("a third abort: %v %+v", err, rep)
	}

	rep, err := tool.CleanupSystem(SystemOptions{Log: quiet(), Run: run, Confirm: run, MinAge: time.Nanosecond})
	if err != nil || rep.Deleted == 0 {
		t.Fatalf("cleanup of the abort: %v %+v", err, rep)
	}

	if n := len(snapshot(t, tool, cfg.IndexPool, run)); n != 0 {
		t.Fatalf("%d quarantine objects left", n)
	}
}

// TestAbortRefusesUsedTarget: a system pool that changed since M4 is not
// aborted (exit 3); surplus objects at the old place refuse (exit 2)
// until they are confirmed for deletion (R4.5).
func TestAbortRefusesUsedTarget(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)

	_ = crashStep(t, PhaseM5, 1)

	if _, err := migrate(tool, cfg, SystemOptions{}); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	_, f, _ := inputs(t, tool)
	o := SystemOptions{Log: quiet(), Run: f.Run, Wait: true}

	writeRaw(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.new", []byte("{}"))

	_, err := tool.AbortSystem(o)
	wantErr(t, err, ErrSystemMismatch, "abort over a used system pool")

	deleteRaw(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.new")
	writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, pushLeaseOID+"2", []byte("{}"))

	rep, err := tool.AbortSystem(o)
	wantErr(t, err, ErrSystemRefused, "abort with a surplus object")

	if len(rep.Surplus) != 1 {
		t.Fatalf("surplus %v", rep.Surplus)
	}

	o.DeleteSurplus, o.Confirm = true, f.Run

	if rep, err = tool.AbortSystem(o); err != nil || rep.Deleted != 1 {
		t.Fatalf("abort with --delete-surplus: %v %+v", err, rep)
	}
}

// TestRevert: a revert copies the system pool, with what a server wrote
// there, back into the index pool; surplus objects refuse until
// confirmed; a revert that stopped is continued (R3.5).
func TestRevert(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	f := checkMigrated(t, tool, cfg, func() map[string]string {
		b := snapshot(t, tool, cfg.IndexPool, "")
		mustMigrate(t, tool, cfg, SystemOptions{})

		return b
	}())

	s, err := Open(serverConfig(cfg), quiet())
	if err != nil {
		t.Fatal(err)
	}

	populateRecordsNew(t, s)

	if !s.Drain(10 * time.Second) {
		t.Fatal("not drained")
	}

	s.Close()

	writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, "kv.raw/old-pod", []byte("{}"))

	o := SystemOptions{Log: quiet(), Run: f.Run, Confirm: f.Run, Parallel: 4}

	rep, err := tool.RevertSystem(o)
	wantErr(t, err, ErrSystemRefused, "revert with a surplus object")

	if !slices.Contains(rep.Surplus, objName(model.AdminNamespace, "kv.raw/old-pod")) {
		t.Fatalf("surplus %v", rep.Surplus)
	}

	o.DeleteSurplus = true
	_ = crashStep(t, "revert:object", 5)

	if _, err := tool.RevertSystem(o); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	_, err = migrate(tool, cfg, SystemOptions{Wait: true})
	wantErr(t, err, ErrSystemRefused, "migrate during a revert")

	o.Wait = true

	if _, err := tool.RevertSystem(o); err != nil {
		t.Fatalf("revert: %v", err)
	}

	checkReverted(t, tool, cfg, f.Run)
}

// populateRecordsNew writes a record through a server on the system pool.
func populateRecordsNew(t *testing.T, s *Store) {
	t.Helper()

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	if err := kv.Update("routing/new", func([]byte) ([]byte, error) { return []byte(`{"new":1}`), nil }); err != nil {
		t.Fatal(err)
	}
}

// checkReverted: the index pool holds what the system pool holds, the
// mark is retired, the fence reverted, the quarantine gone, and a server
// without the system pool reads the record written on the system pool.
func checkReverted(t *testing.T, tool *Store, cfg Config, run string) {
	t.Helper()

	_, f, m := inputs(t, tool)
	if f.State != FenceReverted || m.State != MarkRetired {
		t.Fatalf("fence %+v, mark %+v", f.Values, m.Values)
	}

	sys := snapshot(t, tool, cfg.SystemPool, "")
	if got := snapshot(t, tool, cfg.IndexPool, ""); !maps.Equal(got, sys) {
		t.Fatalf("the index pool differs from the system pool: %d and %d objects", len(got), len(sys))
	}

	if n := len(snapshot(t, tool, cfg.IndexPool, run)); n != 0 {
		t.Fatalf("%d quarantine objects left", n)
	}

	s := mustOpen(t, plainConfig(cfg))

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	if v, err := kv.Get("routing/new"); err != nil || string(v) != `{"new":1}` {
		t.Fatalf("record written on the system pool: %q %v", v, err)
	}

	wantRefusal(t, serverConfig(cfg), "withdrawn")
}

// TestRevertNative: a native init is reverted the same way.
func TestRevertNative(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	res := mustInit(t, tool)

	s, err := Open(serverConfig(cfg), quiet())
	if err != nil {
		t.Fatal(err)
	}

	populateRecordsNew(t, s)

	if !s.Drain(10 * time.Second) {
		t.Fatal("not drained")
	}

	s.Close()

	if _, err := tool.RevertSystem(SystemOptions{Log: quiet(), Run: res.Run, Confirm: res.Run}); err != nil {
		t.Fatalf("revert: %v", err)
	}

	checkReverted(t, tool, cfg, res.Run)
}

// TestVerifySourceOnly: objects under the sweep's prefix are allowed at
// the old place; a changed poisoned head or another object is not; the
// surplus is deleted when confirmed.
func TestVerifySourceOnly(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	mustMigrate(t, tool, cfg, SystemOptions{})

	_, f, _ := inputs(t, tool)
	o := SystemOptions{Log: quiet(), SourceOnly: true}

	putOmap(t, tool, cfg.IndexPool, model.AccountsNamespace, "sweep/state", map[string]string{"x": "1"})

	if _, err := tool.VerifySystem(o); err != nil {
		t.Fatalf("verify with a sweep object: %v", err)
	}

	writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, authFlushOID, []byte("x"))

	_, err := tool.VerifySystem(o)
	wantErr(t, err, ErrSystemMismatch, "verify with a new auth flush log")

	o.DeleteSurplus, o.Confirm = true, "wrong"

	_, err = tool.VerifySystem(o)
	wantErr(t, err, ErrSystemRefused, "delete without the right run")

	o.Confirm = f.Run

	if rep, err := tool.VerifySystem(o); err != nil || rep.Deleted != 1 {
		t.Fatalf("verify --delete-surplus: %v %+v", err, rep)
	}

	heads, _ := fenceHeads(f)
	ns, oid := splitName(heads[0])
	putOmap(t, tool, cfg.IndexPool, ns, oid, map[string]string{"format": "1"})

	_, err = tool.VerifySystem(SystemOptions{Log: quiet(), SourceOnly: true})
	wantErr(t, err, ErrSystemMismatch, "verify with a changed poisoned head")
}

// TestVerifyCopy: before the servers use the system pool, verify
// compares the source with the copy.
func TestVerifyCopy(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	mustMigrate(t, tool, cfg, SystemOptions{})

	if rep, err := tool.VerifySystem(SystemOptions{Log: quiet(), Parallel: 4}); err != nil {
		t.Fatalf("verify: %v %+v", err, rep)
	}

	writeRaw(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.routing/x", []byte("changed"))

	_, err := tool.VerifySystem(SystemOptions{Log: quiet()})
	wantErr(t, err, ErrSystemMismatch, "verify with a changed copy")
}

// TestCleanup: cleanup refuses an unconfirmed or young run and a
// remnant that does not check out; it deletes the quarantine and keeps
// fence and heads, after which verify --source-only expects no
// quarantine.
func TestCleanup(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	mustMigrate(t, tool, cfg, SystemOptions{})

	_, f, _ := inputs(t, tool)
	o := SystemOptions{Log: quiet(), Run: f.Run}

	_, err := tool.CleanupSystem(o)
	wantErr(t, err, ErrSystemRefused, "cleanup without --confirm")

	o.Confirm = f.Run

	_, err = tool.CleanupSystem(o)
	wantErr(t, err, ErrSystemRefused, "cleanup of a young run")

	o.MinAge = time.Nanosecond

	writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, "kv.raw/late", []byte("{}"))

	_, err = tool.CleanupSystem(o)
	wantErr(t, err, ErrSystemMismatch, "cleanup with a surplus object")

	o.DeleteSurplus = true

	rep, err := tool.CleanupSystem(o)
	if err != nil || !rep.Done || rep.Deleted <= 1 {
		t.Fatalf("cleanup: %v %+v", err, rep)
	}

	_, f2, _ := inputs(t, tool)
	if f2.State != FenceDone || f2.Values[fkCleanedAt] == "" || len(snapshot(t, tool, cfg.IndexPool, f.Run)) != 0 {
		t.Fatalf("fence after cleanup %+v", f2.Values)
	}

	if _, err := tool.VerifySystem(SystemOptions{Log: quiet(), SourceOnly: true}); err != nil {
		t.Fatalf("verify after cleanup: %v", err)
	}

	putOmap(t, tool, cfg.IndexPool, quarantineNS(model.AdminNamespace, f.Run), "kv.back", map[string]string{"x": "1"})

	_, err = tool.VerifySystem(SystemOptions{Log: quiet(), SourceOnly: true})
	wantErr(t, err, ErrSystemMismatch, "verify with a quarantine after cleanup")

	// The quarantine namespace has a "/" of its own.
	rep, err = tool.VerifySystem(SystemOptions{Log: quiet(), SourceOnly: true, DeleteSurplus: true, Confirm: f.Run})
	if err != nil || rep.Deleted != 1 {
		t.Fatalf("deleting a quarantine object after cleanup: %v %+v", err, rep)
	}

	if n := len(snapshot(t, tool, cfg.IndexPool, f.Run)); n != 0 {
		t.Fatalf("%d quarantine objects left", n)
	}
}

// TestRevertLateObject: an object written into the system pool after the
// first listing and before the fence took effect is copied back too.
func TestRevertLateObject(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	mustMigrate(t, tool, cfg, SystemOptions{})

	_, f, _ := inputs(t, tool)

	systemStep = func(step string) error {
		if step == "revert:fence" {
			writeRaw(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.late", []byte("{}"))
		}

		return nil
	}

	t.Cleanup(noCrash)

	if _, err := tool.RevertSystem(SystemOptions{Log: quiet(), Run: f.Run, Confirm: f.Run}); err != nil {
		t.Fatalf("revert: %v", err)
	}

	if got := names(t, tool, cfg.IndexPool, model.AdminNamespace); !slices.Contains(got, "kv.late") {
		t.Fatalf("the late object was not copied back: %v", got)
	}
}

// TestInitConfirmsEarlierRun: init deletes the copies of an earlier run
// that has no retired mark (a migration aborted before its commit) and
// its quarantine, once that run is confirmed.
func TestInitConfirmsEarlierRun(t *testing.T) {
	tool, cfg := migPools(t)

	putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, runStateOID, map[string]string{rsRun: "old1", rsPhase: PhaseAbort})
	putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.copy", map[string]string{"x": "1"})
	putOmap(t, tool, cfg.IndexPool, quarantineNS(model.AdminNamespace, "old1"), "kv.copy", map[string]string{"x": "1"})

	_, err := tool.InitSystem(InitOptions{})
	if !errors.Is(err, ErrInitRefused) || !strings.Contains(err.Error(), "old1") {
		t.Fatalf("init over an earlier run: %v", err)
	}

	res, err := tool.InitSystem(InitOptions{Confirm: "old1"})
	if err != nil || res.Cleared != 3 {
		t.Fatalf("init with the earlier run confirmed: %+v %v", res, err)
	}

	if n := len(names(t, tool, cfg.IndexPool, quarantineNS(model.AdminNamespace, "old1"))); n != 0 {
		t.Fatalf("%d quarantine objects left", n)
	}
}

// TestStatusWithoutPlacement: status of an index pool without a fence
// lists the registrations there.
func TestStatusWithoutPlacement(t *testing.T) {
	tool, cfg := migPools(t)
	running(t, tool, cfg)

	rep, err := tool.SystemStatus(SystemOptions{Log: quiet()})
	if err != nil || rep.Fence != nil || len(rep.Registrations) != 1 || !rep.Registrations[0].Running {
		t.Fatalf("status %+v %v", rep, err)
	}
}

// TestToolClientOptions: the tools refuse an operation timeout above 60
// seconds (K3).
func TestToolClientOptions(t *testing.T) {
	tool, cfg := migPools(t)
	_ = tool

	cfg.PlacementTool = true
	cfg.ClientOptions = map[string]string{"rados_osd_op_timeout": "120"}
	s := mustOpen(t, cfg)

	_, err := s.MigrateSystem(cfg.SystemPool, SystemOptions{Log: quiet()})
	wantErr(t, err, ErrSystemRefused, "an operation timeout of 120 s")
}

// TestAbortInit: an init that stopped is aborted (R4.6): the fence is
// reverted, a mark it wrote retired, and the next init deletes the heads
// of that run once it is confirmed.
func TestAbortInit(t *testing.T) {
	quickChecks(t)

	for _, step := range []string{"markers", "mark"} {
		t.Run(step, func(t *testing.T) {
			tool, cfg := migPools(t)

			stop := errors.New("stop")
			initStep = func(s string) error {
				if s == step {
					return stop
				}

				return nil
			}

			t.Cleanup(func() { initStep = func(string) error { return nil } })

			if _, err := tool.InitSystem(InitOptions{}); !errors.Is(err, stop) {
				t.Fatalf("init: %v", err)
			}

			initStep = func(string) error { return nil }

			_, f, _ := inputs(t, tool)

			if _, err := tool.AbortSystem(SystemOptions{Log: quiet(), Run: f.Run}); err != nil {
				t.Fatalf("abort: %v", err)
			}

			if _, f2, m := inputs(t, tool); f2.State != FenceReverted || (m.Exists && m.State != MarkRetired) {
				t.Fatalf("fence %+v, mark %+v", f2.Values, m.Values)
			}

			mustOpen(t, plainConfig(cfg))

			if _, err := tool.InitSystem(InitOptions{}); !errors.Is(err, ErrInitRefused) {
				t.Fatalf("init over the aborted run: %v", err)
			}

			if res, err := tool.InitSystem(InitOptions{Confirm: f.Run}); err != nil || res.Cleared == 0 {
				t.Fatalf("init with the aborted run confirmed: %+v %v", res, err)
			}

			mustOpen(t, serverConfig(cfg))
		})
	}
}

// TestAbortDeleteCopiesResumes: an abort that stopped while deleting the
// copies (after the fence was reverted) is continued with
// --delete-copies; the run state and the mark stay as the record, so the
// next run confirms the run and deletes its quarantine.
func TestAbortDeleteCopiesResumes(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	_ = crashStep(t, PhaseM6, 1)

	if _, err := migrate(tool, cfg, SystemOptions{}); err == nil {
		t.Fatal("no crash")
	}

	_, f, _ := inputs(t, tool)
	o := SystemOptions{Log: quiet(), Run: f.Run, Wait: true, DeleteCopies: true, Confirm: f.Run}
	_ = crashStep(t, "abort:copies", 1)

	if _, err := tool.AbortSystem(o); err == nil {
		t.Fatal("no crash in the abort")
	}

	noCrash()

	if _, f2, _ := inputs(t, tool); f2.State != FenceReverted {
		t.Fatalf("fence %+v", f2.Values)
	}

	if rep, err := tool.AbortSystem(o); err != nil || rep.Deleted == 0 {
		t.Fatalf("second abort: %v %+v", err, rep)
	}

	if got := names(t, tool, cfg.SystemPool, model.AdminNamespace); slices.ContainsFunc(got, func(n string) bool { return !strings.HasPrefix(n, "system.") }) {
		t.Fatalf("system pool after --delete-copies: %v", got)
	}

	if len(snapshot(t, tool, cfg.IndexPool, f.Run)) == 0 {
		t.Fatal("the quarantine is gone")
	}

	rep := mustMigrate(t, tool, cfg, SystemOptions{Confirm: f.Run})
	if len(snapshot(t, tool, cfg.IndexPool, f.Run)) != 0 || rep.Deleted == 0 {
		t.Fatalf("the next run left the old quarantine: %+v", rep)
	}

	checkMigrated(t, tool, cfg, before)
}

// TestAbortChangedSource: a source that changed after M4 stops M5 (exit
// 3); the abort restores it from the copy of M4 in the system pool.
func TestAbortChangedSource(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	systemStep = func(step string) error {
		if step == "M5:start" {
			writeRaw(t, tool, cfg.IndexPool, model.AdminNamespace, "kv.routing/x", []byte(`{"x":3}`))
		}

		return nil
	}

	t.Cleanup(noCrash)

	_, err := migrate(tool, cfg, SystemOptions{Parallel: 1})
	wantErr(t, err, ErrSystemMismatch, "a source changed after M4")

	noCrash()

	_, f, _ := inputs(t, tool)
	if _, err := tool.AbortSystem(SystemOptions{Log: quiet(), Run: f.Run}); err != nil {
		t.Fatalf("abort: %v", err)
	}

	if got := snapshot(t, tool, cfg.IndexPool, ""); !maps.Equal(got, before) {
		t.Fatal("the old place differs from the state of M4")
	}
}

// TestMigrateWipeResumes: a migration that stopped before it deleted the
// confirmed earlier run is continued with the same confirmation.
func TestMigrateWipeResumes(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	before := snapshot(t, tool, cfg.IndexPool, "")

	_ = crashStep(t, PhaseM3, 1)

	if _, err := migrate(tool, cfg, SystemOptions{}); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	_, old, _ := inputs(t, tool)
	if _, err := tool.AbortSystem(SystemOptions{Log: quiet(), Run: old.Run, Wait: true}); err != nil {
		t.Fatalf("abort: %v", err)
	}

	_ = crashStep(t, "fence", 1)

	if _, err := migrate(tool, cfg, SystemOptions{Confirm: old.Run}); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	mustMigrate(t, tool, cfg, SystemOptions{Confirm: old.Run, Wait: true})
	checkMigrated(t, tool, cfg, before)
}

// TestCleanupResumes: a cleanup that stopped while deleting the
// quarantine leaves verify green and is completed by running it again.
func TestCleanupResumes(t *testing.T) {
	quickChecks(t)

	tool, cfg := migPools(t)
	populate(t, tool, cfg)
	mustMigrate(t, tool, cfg, SystemOptions{})

	_, f, _ := inputs(t, tool)
	o := SystemOptions{Log: quiet(), Run: f.Run, Confirm: f.Run, MinAge: time.Nanosecond, Wait: true}

	_ = crashStep(t, "cleanup:marked", 1)

	if _, err := tool.CleanupSystem(o); err == nil {
		t.Fatal("no crash")
	}

	noCrash()

	quar := names(t, tool, cfg.IndexPool, quarantineNS(model.AdminNamespace, f.Run))
	deleteRaw(t, tool, cfg.IndexPool, quarantineNS(model.AdminNamespace, f.Run), quar[0])

	if _, err := tool.VerifySystem(SystemOptions{Log: quiet(), SourceOnly: true}); err != nil {
		t.Fatalf("verify during an unfinished cleanup: %v", err)
	}

	if rep, err := tool.CleanupSystem(o); err != nil || !rep.Done {
		t.Fatalf("second cleanup: %v %+v", err, rep)
	}

	if n := len(snapshot(t, tool, cfg.IndexPool, f.Run)); n != 0 {
		t.Fatalf("%d quarantine objects left", n)
	}
}
