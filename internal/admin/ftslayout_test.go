package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/jobs"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// fakeIndex is a layout 3 index as the admin tests see it: generation,
// unfinished conversion, whether its counts ask for a reshard, and a
// conversion that blocks until released.
type fakeIndex struct {
	mu      sync.Mutex
	gen     string
	next    string
	nextAt  time.Time
	due     bool
	cleared []string
	runs    atomic.Int32
	started time.Time
	// release, when set, holds a conversion until it is closed; the
	// conversion then fails when its fence does.
	release chan struct{}
	// dueEntered and dueGate, when set, make FTSLayoutDue report that it
	// was called and hold it until dueGate is closed.
	dueEntered chan struct{}
	dueGate    chan struct{}
	// dx is the identifier filter of the current generation (B14h).
	dx int
}

func (f *fakeIndex) snapshot() (gen, next string, cleared []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.gen, f.next, append([]string(nil), f.cleared...)
}

type importerUser interface {
	model.User
	model.Importer
}

// fakeLayoutUser is a memstore account with a fake index.
type fakeLayoutUser struct {
	importerUser
	f *fakeIndex
}

func (u *fakeLayoutUser) FTSLayout() (model.FTSLayoutState, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	st := model.FTSLayoutState{Current: model.FTSLayoutShape{Layout: 3, Gen: u.f.gen, S: 1, D: 1, H: 1, DictIDMinRunes: u.f.dx}}
	if u.f.next != "" {
		st.Next, st.NextTok, st.NextAt = &model.FTSLayoutShape{Layout: 3, Gen: "next"}, u.f.next, u.f.nextAt
	}

	return st, nil
}

func (u *fakeLayoutUser) FTSLayoutDue() (model.FTSLayoutShape, bool, error) {
	if u.f.dueGate != nil {
		u.f.dueEntered <- struct{}{}

		<-u.f.dueGate
	}

	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	return model.FTSLayoutShape{Layout: 3, S: 2, D: 2, H: 1}, u.f.due, nil
}

func (u *fakeLayoutUser) ConvertFTSLayout(req model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
	u.f.runs.Add(1)

	u.f.mu.Lock()
	u.f.started = time.Now()
	u.f.next, u.f.nextAt = req.Tok, time.Now()
	release := u.f.release
	u.f.mu.Unlock()

	if release != nil {
		for done := false; !done; {
			select {
			case <-release:
				done = true
			case <-time.After(5 * time.Millisecond):
				if err := req.Fence(); err != nil {
					return model.FTSLayoutReport{}, err
				}
			}
		}
	}

	if err := req.Fence(); err != nil {
		return model.FTSLayoutReport{}, err
	}

	u.f.mu.Lock()
	u.f.gen, u.f.next, u.f.due = u.f.gen+"+", "", false
	u.f.mu.Unlock()

	return model.FTSLayoutReport{}, nil
}

func (u *fakeLayoutUser) ClearFTSLayoutNext(tok string) (bool, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	if u.f.next == "" || u.f.next != tok {
		return false, nil
	}

	u.f.cleared, u.f.next = append(u.f.cleared, tok), ""

	return true, nil
}

func (u *fakeLayoutUser) ClearForeignFTSLayoutNext(own string) (string, error) {
	u.f.mu.Lock()
	tok := u.f.next
	u.f.mu.Unlock()

	if tok == "" || tok == own {
		return "", nil
	}

	_, err := u.ClearFTSLayoutNext(tok)

	return tok, err
}

func (u *fakeLayoutUser) ClearFTSLayoutOld(string) (bool, error) { return false, nil }

func (u *fakeLayoutUser) HeartbeatFTSLayoutNext(tok string, at time.Time) (bool, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	if u.f.next != tok {
		return false, nil
	}

	u.f.nextAt = at

	return true, nil
}

// fakeOpener opens memstore accounts with the fake index.
type fakeOpener struct {
	s *memstore.Store
	f *fakeIndex
}

func (o fakeOpener) User(name string) (model.User, error) {
	u, err := o.s.User(name)
	if err != nil {
		return nil, err
	}

	return &fakeLayoutUser{importerUser: u.(importerUser), f: o.f}, nil
}

// layoutStore is the layout administration of the tests: a presence
// index in memory.
type layoutStore struct{ b *sortedindex.MemBackend }

func (l layoutStore) FTSLayout3Enabled() (bool, error) { return true, nil }

func (l layoutStore) FTSLayoutIndex() (model.SortedIndex, func(), error) {
	return sortedindex.New(l.b, model.FTSLayoutIndexName, model.FTSLayoutIndexShards), func() {}, nil
}

// layoutFixture is an admin socket over a fake index.
type layoutFixture struct {
	s      *Server
	c      *adminapi.ClientWithResponses
	kv     *memstore.AdminKV
	f      *fakeIndex
	layout layoutStore
}

func serveLayout(t *testing.T, o Options, roles ...string) *layoutFixture {
	t.Helper()

	store := memstore.New()
	f := &fakeIndex{gen: "g1"}
	opener := fakeOpener{s: store, f: f}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	kv := memstore.NewAdminKVMode(model.KVIndexRead)
	ls := layoutStore{b: sortedindex.NewMemBackend()}

	o.Delivery, o.Auth, o.Log, o.Version, o.MaxMessageBytes, o.SocketRoles = &delivery.Service{Store: store, Log: log}, auth.Static{"user": "pw"}, log, "test", 4096, roles
	o.Import, o.Accounts, o.KV, o.FTSLayouts = opener, opener, kv, ls
	o.LayoutDisableWait = cmpDuration(o.LayoutDisableWait, time.Millisecond)
	o.ImportReshardSync = cmpDuration(o.ImportReshardSync, 2*time.Second)
	o.ImportReshardSettle = cmpDuration(o.ImportReshardSettle, time.Millisecond)

	s := New(o)
	path := filepath.Join(socketDir(t), "admin.sock")

	ln, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := s.UnixServer()

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() {
		_ = srv.Close()
		s.Close()
	})

	client, err := adminapi.NewSocketClient(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	return &layoutFixture{s: s, c: client, kv: kv, f: f, layout: ls}
}

func cmpDuration(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}

	return d
}

// TestLayout3EnableDisable (N2, F12, N7): enable needs exactly --pods
// fresh registrations (force skips the check, recorded in the mark);
// disable removes the mark and lists the accounts that still have
// layout 3.
func TestLayout3EnableDisable(t *testing.T) {
	fx := serveLayout(t, Options{}, RoleDiagnose, jobs.RoleRepair)
	ctx := context.Background()

	for _, pod := range []string{"pod-a", "pod-b"} {
		if err := jobs.RegisterServer(fx.kv, pod, "0.45.0", time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	st, err := fx.c.GetFTSLayout3WithResponse(ctx)
	if err != nil || st.JSON200 == nil || st.JSON200.Enabled || len(st.JSON200.Servers) != 2 || !st.JSON200.Servers[0].Fresh {
		t.Fatalf("status %v %+v", err, st.JSON200)
	}

	enableChecks(t, fx)

	disableChecks(t, fx)
}

// TestDictFilterEnableDisable (B14h): the mark of the dictionary filter
// needs exactly --pods fresh registrations with fts_dict_filter; disable
// removes it and lists the accounts whose generation still has the
// filter, read from their layout records; the layout 3 mark is a
// different one.
func TestDictFilterEnableDisable(t *testing.T) {
	fx := serveLayout(t, Options{}, RoleDiagnose, jobs.RoleRepair)
	ctx := context.Background()

	for _, pod := range []string{"pod-a", "pod-b"} {
		if err := jobs.RegisterServer(fx.kv, pod, "0.50.0", time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	st, err := fx.c.GetFTSDictFilterWithResponse(ctx)
	if err != nil || st.JSON200 == nil || st.JSON200.Enabled || len(st.JSON200.Servers) != 2 {
		t.Fatalf("status %v %+v", err, st.JSON200)
	}

	enableDictFilter(t, fx)
	disableDictFilter(t, fx)
}

// enableDictFilter: the wrong pod count is refused naming the
// capability, the right one sets the mark of the filter and not the
// layout 3 mark.
func enableDictFilter(t *testing.T, fx *layoutFixture) {
	t.Helper()

	ctx := context.Background()

	if bad, _ := fx.c.EnableFTSDictFilterWithResponse(ctx, adminapi.FTSLayout3Enable{Pods: 3}); bad.StatusCode() != http.StatusConflict ||
		!strings.Contains(bad.JSONDefault.Message, model.CapFTSDictFilter) {
		t.Fatalf("enable with the wrong count: %d", bad.StatusCode())
	}

	ok, _ := fx.c.EnableFTSDictFilterWithResponse(ctx, adminapi.FTSLayout3Enable{Pods: 2})
	if ok.JSON200 == nil || !ok.JSON200.Enabled || ok.JSON200.Mark.Pods != 2 {
		t.Fatalf("enable: %d %+v", ok.StatusCode(), ok.JSON200)
	}

	if _, err := fx.kv.Get(model.FTSDictFilterMarkKey); err != nil {
		t.Fatalf("mark: %v", err)
	}

	if _, err := fx.kv.Get(model.FTSLayoutMarkKey); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("the layout 3 mark was set: %v", err)
	}
}

// disableDictFilter: disable lists the accounts whose generation has the
// filter (none when none has), list --verify shows it.
func disableDictFilter(t *testing.T, fx *layoutFixture) {
	t.Helper()

	ctx := context.Background()

	ix, _, _ := fx.layout.FTSLayoutIndex()
	for _, a := range []string{"alice", "bob"} {
		if err := ix.Set(a, []byte(`{"since":"2026-10-01T00:00:00Z"}`)); err != nil {
			t.Fatal(err)
		}
	}

	fx.setDX(25)

	dis, _ := fx.c.DisableFTSDictFilterWithResponse(ctx)
	if dis.JSON200 == nil || !slices.Equal(slices.Sorted(slices.Values(dis.JSON200.Accounts)), []string{"alice", "bob"}) {
		t.Fatalf("disable: %d %+v", dis.StatusCode(), dis.JSON200)
	}

	if _, err := fx.kv.Get(model.FTSDictFilterMarkKey); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("mark after disable: %v", err)
	}

	verifiedFilter(t, fx, 25)
	fx.setDX(0)

	if dis, _ := fx.c.DisableFTSDictFilterWithResponse(ctx); dis.JSON200 == nil || len(dis.JSON200.Accounts) != 0 {
		t.Fatalf("disable without filtered generations: %d %+v", dis.StatusCode(), dis.JSON200)
	}
}

// verifiedFilter: list --verify shows the identifier filter dx of the
// first account's generation.
func verifiedFilter(t *testing.T, fx *layoutFixture, dx int) {
	t.Helper()

	verify := true

	list, _ := fx.c.ListFTSLayout3AccountsWithResponse(context.Background(), &adminapi.ListFTSLayout3AccountsParams{Verify: &verify})
	if list.JSON200 == nil || len(list.JSON200.Items) == 0 || list.JSON200.Items[0].Layout == nil {
		t.Fatalf("list --verify: %d %+v", list.StatusCode(), list.JSON200)
	}

	if got := list.JSON200.Items[0].Layout.Current.DictIdMinRunes; got == nil || *got != dx {
		t.Fatalf("list --verify: filter %v, want %d", got, dx)
	}
}

// setDX sets the identifier filter of the fake index.
func (fx *layoutFixture) setDX(n int) {
	fx.f.mu.Lock()
	fx.f.dx = n
	fx.f.mu.Unlock()
}

// disableChecks: disable lists the account that still has layout 3 and
// removes the mark; list --verify finds its index.
func disableChecks(t *testing.T, fx *layoutFixture) {
	t.Helper()

	ctx := context.Background()

	ix, _, _ := fx.layout.FTSLayoutIndex()
	if err := ix.Set("alice", []byte(`{"since":"2026-10-01T00:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}

	dis, _ := fx.c.DisableFTSLayout3WithResponse(ctx)
	if dis.JSON200 == nil || len(dis.JSON200.Accounts) != 1 || dis.JSON200.Accounts[0] != "alice" {
		t.Fatalf("disable: %d %+v", dis.StatusCode(), dis.JSON200)
	}

	if _, err := fx.kv.Get(model.FTSLayoutMarkKey); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("mark after disable: %v", err)
	}

	verify := true

	list, _ := fx.c.ListFTSLayout3AccountsWithResponse(ctx, &adminapi.ListFTSLayout3AccountsParams{Verify: &verify})
	if list.JSON200 == nil || len(list.JSON200.Items) != 1 || list.JSON200.Items[0].Mismatch == nil || *list.JSON200.Items[0].Mismatch {
		t.Fatalf("list --verify: %d %+v", list.StatusCode(), list.JSON200)
	}
}

// enableChecks: the wrong pod count is refused with the instances named,
// the right one sets the mark, force sets it anyway and says so.
func enableChecks(t *testing.T, fx *layoutFixture) {
	t.Helper()

	ctx := context.Background()

	bad, _ := fx.c.EnableFTSLayout3WithResponse(ctx, adminapi.FTSLayout3Enable{Pods: 3})
	if bad.StatusCode() != http.StatusConflict || !strings.Contains(bad.JSONDefault.Message, "pod-a") {
		t.Fatalf("enable with the wrong count: %d %+v", bad.StatusCode(), bad.JSONDefault)
	}

	ok, _ := fx.c.EnableFTSLayout3WithResponse(ctx, adminapi.FTSLayout3Enable{Pods: 2})
	if ok.JSON200 == nil || !ok.JSON200.Enabled || ok.JSON200.Mark.Pods != 2 || *ok.JSON200.Mark.Forced {
		t.Fatalf("enable: %d %+v", ok.StatusCode(), ok.JSON200)
	}

	force := true

	forced, _ := fx.c.EnableFTSLayout3WithResponse(ctx, adminapi.FTSLayout3Enable{Pods: 7, Force: &force})
	if forced.JSON200 == nil || !*forced.JSON200.Mark.Forced {
		t.Fatalf("forced enable: %d", forced.StatusCode())
	}
}

// postReshard posts the reshard of the session.
func postReshard(t *testing.T, fx *layoutFixture, session string) (int, *adminapi.ImportReshard) {
	t.Helper()

	resp, err := fx.c.StartImportReshardWithResponse(context.Background(), "user", &adminapi.StartImportReshardParams{XImportSession: session})
	if err != nil {
		t.Fatal(err)
	}

	switch {
	case resp.JSON200 != nil:
		return resp.StatusCode(), resp.JSON200
	case resp.JSON202 != nil:
		return resp.StatusCode(), resp.JSON202
	default:
		return resp.StatusCode(), nil
	}
}

// getReshard reads the reshard state of the session.
func getReshard(t *testing.T, fx *layoutFixture, session string) *adminapi.ImportReshard {
	t.Helper()

	resp, err := fx.c.GetImportReshardWithResponse(context.Background(), "user", &adminapi.GetImportReshardParams{XImportSession: session})
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("get: %v %d %+v", err, resp.StatusCode(), resp.JSONDefault)
	}

	return resp.JSON200
}

// TestImportReshardSync (F3, R5.4): nothing to do answers idle with the
// generation; a reshard within the synchronous bound answers done with
// the new generation, after the settle time (R4.5).
func TestImportReshardSync(t *testing.T) {
	fx := serveLayout(t, Options{ImportReshardSettle: 50 * time.Millisecond}, RoleImport)
	session := openSession(t, fx.c, "user")

	code, st := postReshard(t, fx, session)
	if code != http.StatusOK || st == nil || st.State != reshardIdle || st.G != "g1" {
		t.Fatalf("idle: %d %+v", code, st)
	}

	fx.f.due = true
	start := time.Now()

	code, st = postReshard(t, fx, session)
	if code != http.StatusOK || st == nil || st.State != reshardDone || st.G != "g1+" {
		t.Fatalf("done: %d %+v", code, st)
	}

	if fx.f.started.Sub(start) < 50*time.Millisecond {
		t.Errorf("no settle wait before the conversion (%v)", fx.f.started.Sub(start))
	}

	if _, next, _ := fx.f.snapshot(); next != "" {
		t.Errorf("next %q after done", next)
	}
}

// TestImportReshardBackground (F3, R4.2, R5.2): past the synchronous
// bound POST answers 202; a second POST of the session (also two at
// once) starts no second run; import batches answer 409 while it runs;
// GET follows it to done; closing the session drops its entry.
func TestImportReshardBackground(t *testing.T) {
	fx := serveLayout(t, Options{ImportReshardSync: 30 * time.Millisecond}, RoleImport)
	session := openSession(t, fx.c, "user")

	fx.f.due, fx.f.release = true, make(chan struct{})

	var wg sync.WaitGroup

	codes := make([]int, 3)

	for i := range codes {
		wg.Add(1)

		go func() {
			defer wg.Done()

			codes[i], _ = postReshard(t, fx, session)
		}()
	}

	wg.Wait()

	for _, c := range codes {
		if c != http.StatusAccepted {
			t.Fatalf("concurrent POSTs: %v", codes)
		}
	}

	if n := fx.f.runs.Load(); n != 1 {
		t.Fatalf("%d runs for one session", n)
	}

	if st := getReshard(t, fx, session); st.State != reshardRunning {
		t.Fatalf("get while running: %+v", st)
	}

	batch, _ := fx.c.ImportChangesWithResponse(context.Background(), "user", "x", &adminapi.ImportChangesParams{XImportSession: session}, adminapi.ImportChanges{})
	if batch.StatusCode() != http.StatusConflict {
		t.Errorf("import batch during the reshard: %d", batch.StatusCode())
	}

	close(fx.f.release)

	waitReshard(t, fx, session, reshardDone)

	cl, _ := fx.c.CloseImportWithResponse(context.Background(), "user", &adminapi.CloseImportParams{XImportSession: session})
	if cl.StatusCode() != http.StatusNoContent {
		t.Fatalf("close %d", cl.StatusCode())
	}

	fx.s.rs.mu.Lock()
	left := len(fx.s.rs.runs)
	fx.s.rs.mu.Unlock()

	if left != 0 {
		t.Errorf("%d map entries after the session closed", left)
	}
}

// waitReshard polls GET until the state.
func waitReshard(t *testing.T, fx *layoutFixture, session, state string) *adminapi.ImportReshard {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if st := getReshard(t, fx, session); st.State == state {
			return st
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("reshard not %s", state)

	return nil
}

// TestImportReshardForeign (R4.2): another session's run with a fresh
// heartbeat answers 409; with an old heartbeat it is cleared and a new
// run starts.
func TestImportReshardForeign(t *testing.T) {
	fx := serveLayout(t, Options{}, RoleImport)
	session := openSession(t, fx.c, "user")

	fx.f.due, fx.f.next, fx.f.nextAt = true, "othersession00000000/abc", time.Now()

	if code, _ := postReshard(t, fx, session); code != http.StatusConflict {
		t.Fatalf("fresh foreign run: %d", code)
	}

	fx.f.mu.Lock()
	fx.f.nextAt = time.Now().Add(-2 * jobs.ImportReshardFresh)
	fx.f.mu.Unlock()

	code, st := postReshard(t, fx, session)
	if code != http.StatusOK || st.State != reshardDone {
		t.Fatalf("stale foreign run: %d %+v", code, st)
	}

	if _, _, cleared := fx.f.snapshot(); len(cleared) != 1 || cleared[0] != "othersession00000000/abc" {
		t.Errorf("cleared %v", cleared)
	}
}

// TestImportReshardStateFromStore (R4.2, R5.3): without a run in this
// server's map GET reads the layout record: the session's run with a
// fresh heartbeat is running, with an old one failed.
func TestImportReshardStateFromStore(t *testing.T) {
	fx := serveLayout(t, Options{}, RoleImport)
	session := openSession(t, fx.c, "user")

	fx.f.next, fx.f.nextAt = session+"/n1", time.Now()

	if st := getReshard(t, fx, session); st.State != reshardRunning {
		t.Fatalf("fresh: %+v", st)
	}

	fx.f.mu.Lock()
	fx.f.nextAt = time.Now().Add(-jobs.ImportReshardFresh - time.Second)
	fx.f.mu.Unlock()

	if st := getReshard(t, fx, session); st.State != reshardFailed || st.Error == nil {
		t.Fatalf("stale: %+v", st)
	}
}

// TestImportReshardMapNotHeldOverIO (review M5): while a POST reads the
// store (here the key counts), the process map is free: a GET of the
// session answers running at once, and the map's mutex is not held.
func TestImportReshardMapNotHeldOverIO(t *testing.T) {
	fx := serveLayout(t, Options{}, RoleImport)
	session := openSession(t, fx.c, "user")

	fx.f.dueEntered, fx.f.dueGate = make(chan struct{}, 1), make(chan struct{})

	done := make(chan int, 1)

	go func() {
		code, _ := postReshard(t, fx, session)
		done <- code
	}()

	select {
	case <-fx.f.dueEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the POST did not read the key counts")
	}

	if !fx.s.rs.mu.TryLock() {
		t.Fatal("the process map is locked while the store is read")
	}

	fx.s.rs.mu.Unlock()

	if st := getReshard(t, fx, session); st.State != reshardRunning {
		t.Errorf("get during the check: %+v", st)
	}

	close(fx.f.dueGate)

	if code := <-done; code != http.StatusOK {
		t.Errorf("post %d", code)
	}

	fx.s.rs.mu.Lock()
	left := len(fx.s.rs.runs)
	fx.s.rs.mu.Unlock()

	if left != 0 {
		t.Errorf("%d map entries after a check without need", left)
	}
}

// TestImportReshardShutdown (R4.2): Close ends a background run on its
// error path, which clears its conversion, and returns after it.
func TestImportReshardShutdown(t *testing.T) {
	fx := serveLayout(t, Options{ImportReshardSync: 10 * time.Millisecond}, RoleImport)
	session := openSession(t, fx.c, "user")

	fx.f.due, fx.f.release = true, make(chan struct{})

	if code, _ := postReshard(t, fx, session); code != http.StatusAccepted {
		t.Fatalf("post %d", code)
	}

	fx.s.Close()

	if _, next, cleared := fx.f.snapshot(); next != "" || len(cleared) != 1 {
		t.Errorf("after shutdown: next %q cleared %v", next, cleared)
	}
}

// TestImportReshardHeartbeat (R5.3, F9): the run renews the import lock
// and its heartbeat across several lock TTLs.
func TestImportReshardHeartbeat(t *testing.T) {
	beat := reshardBeat
	reshardBeat = 20 * time.Millisecond

	t.Cleanup(func() { reshardBeat = beat })

	fx := serveLayout(t, Options{ImportReshardSync: 10 * time.Millisecond, ImportLockTTL: 100 * time.Millisecond}, RoleImport)
	session := openSession(t, fx.c, "user")

	fx.f.due, fx.f.release = true, make(chan struct{})

	if code, _ := postReshard(t, fx, session); code != http.StatusAccepted {
		t.Fatalf("post %d", code)
	}

	time.Sleep(400 * time.Millisecond) // four lock TTLs

	fx.f.mu.Lock()
	at := fx.f.nextAt
	fx.f.mu.Unlock()

	if time.Since(at) > 100*time.Millisecond {
		t.Errorf("heartbeat %v old", time.Since(at))
	}

	close(fx.f.release)

	if st := waitReshard(t, fx, session, reshardDone); st.Error != nil {
		t.Errorf("done with %v", *st.Error)
	}
}

// TestOpenImportClearsForeign (F2, R4.3): a new session that takes the
// lock clears the conversion an aborted import left and imports without
// waiting for its freeze.
func TestOpenImportClearsForeign(t *testing.T) {
	fx := serveLayout(t, Options{}, RoleImport)

	fx.f.next, fx.f.nextAt = "abortedsession000000/x", time.Now().Add(-time.Hour)

	openSession(t, fx.c, "user")

	if _, next, cleared := fx.f.snapshot(); next != "" || len(cleared) != 1 {
		t.Errorf("next %q cleared %v", next, cleared)
	}
}

// TestImportHintApplied (N6): the hint of OpenImport reaches the views
// of the session's batches.
func TestImportHintApplied(t *testing.T) {
	fx := serveLayout(t, Options{}, RoleImport)

	n := 5000

	resp, err := fx.c.OpenImportWithResponse(context.Background(), "user", adminapi.ImportSessionRequest{Messages: &n})
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("open %v", err)
	}

	hinted := &hintUser{}
	fx.s.applyHint("user", resp.JSON200.Session, hinted)

	if hinted.h.Messages != 5000 {
		t.Errorf("hint %+v", hinted.h)
	}

	b, _ := json.Marshal(adminapi.ImportSessionRequest{Messages: &n})
	if !strings.Contains(string(b), `"messages":5000`) {
		t.Errorf("request %s", b)
	}
}

type hintUser struct {
	model.User
	h model.ImportHint
}

func (u *hintUser) SetImportHint(h model.ImportHint) { u.h = h }
