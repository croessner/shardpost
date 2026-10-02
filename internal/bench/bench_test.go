package bench

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"shardpost/internal/delivery"
	"shardpost/internal/memstore"
)

func TestGuard(t *testing.T) {
	ok := Guard{ProdIndex: "shardpost-index", ProdData: "shardpost-data", ProdSystem: "shardpost-system", Reserved: []string{"shardpost-default-data"},
		BenchIndex: "shardpost-load-index", BenchData: "shardpost-load-data", BenchSystem: "shardpost-load-system", Prefix: "bench-"}
	if err := ok.Check(); err != nil {
		t.Fatalf("valid guard refused: %v", err)
	}

	without := ok
	without.BenchSystem, without.ProdSystem = "", ""

	if err := without.Check(); err != nil {
		t.Fatalf("a bench without system pool refused: %v", err)
	}

	cases := map[string]func(g *Guard){
		"reserved default":         func(g *Guard) { g.BenchData = "shardpost-default-data" },
		"no bench index pool":      func(g *Guard) { g.BenchIndex = "" },
		"no bench data pool":       func(g *Guard) { g.BenchData = "" },
		"index is production":      func(g *Guard) { g.BenchIndex = "shardpost-index" },
		"data is production":       func(g *Guard) { g.BenchData = "shardpost-data" },
		"index is production data": func(g *Guard) { g.BenchIndex = "shardpost-data" },
		"data is production index": func(g *Guard) { g.BenchData = "shardpost-index" },
		"one pool for both":        func(g *Guard) { g.BenchData = g.BenchIndex },
		"system is production":     func(g *Guard) { g.BenchSystem = "shardpost-system" },
		"index is prod system":     func(g *Guard) { g.BenchIndex = "shardpost-system" },
		"data is prod system":      func(g *Guard) { g.BenchData = "shardpost-system" },
		"system is prod index":     func(g *Guard) { g.BenchSystem = "shardpost-index" },
		"system is bench index":    func(g *Guard) { g.BenchSystem = g.BenchIndex },
		"system is bench data":     func(g *Guard) { g.BenchSystem = g.BenchData },
		"prefix without bench-":    func(g *Guard) { g.Prefix = "load" },
		"empty prefix":             func(g *Guard) { g.Prefix = "" },
		"prefix with slash":        func(g *Guard) { g.Prefix = "bench-a/b" },
		"reserved prefix":          func(g *Guard) { g.Prefix = "~bench-" },
	}

	for name, mutate := range cases {
		g := ok
		mutate(&g)

		if err := g.Check(); !errors.Is(err, ErrGuard) {
			t.Errorf("%s: got %v, want a guard refusal", name, err)
		}
	}
}

func TestAccountNames(t *testing.T) {
	if got := AccountName("bench-", 42); got != "bench-000042" {
		t.Fatalf("AccountName = %q", got)
	}

	for ns, want := range map[string]bool{
		"bench-000042": true, "bench-1": true, "bench-": false, "bench-r4-000001": false,
		"croessner": false, "~accounts": false, "bench-00x": false,
	} {
		if got := IsAccount(ns, "bench-"); got != want {
			t.Errorf("IsAccount(%q) = %v, want %v", ns, got, want)
		}
	}

	if IsAccount("load000001", "load") {
		t.Error("an account of a prefix without bench- was taken for a bench account")
	}

	if err := CheckAccount("croessner", "bench-"); !errors.Is(err, ErrGuard) {
		t.Errorf("CheckAccount accepted a non-bench account: %v", err)
	}
}

func TestSizeDistribution(t *testing.T) {
	d, err := ParseSizes(DefaultSizes)
	if err != nil {
		t.Fatal(err)
	}

	if len(d.Buckets) != 3 || d.Buckets[2].Max != 1<<20 || d.Buckets[0].Min != 1<<10 {
		t.Fatalf("parsed %+v", d.Buckets)
	}

	shares := bucketShares(t, d, 20000)

	for i, want := range []float64{0.6, 0.3, 0.1} {
		if got := shares[i]; got < want-0.02 || got > want+0.02 {
			t.Errorf("bucket %d share %.3f, want about %.2f", i, got, want)
		}
	}
}

// bucketShares draws n sizes and returns the share of each default
// bucket.
func bucketShares(t *testing.T, d SizeDist, n int) [3]float64 {
	t.Helper()

	r := rand.New(rand.NewPCG(1, 2))
	counts := [3]int{}

	for range n {
		s := d.Pick(r)

		switch {
		case s < 1<<10 || s > 1<<20:
			t.Fatalf("size %d out of range", s)
		case s <= 8<<10:
			counts[0]++
		case s <= 64<<10:
			counts[1]++
		default:
			counts[2]++
		}
	}

	return [3]float64{float64(counts[0]) / float64(n), float64(counts[1]) / float64(n), float64(counts[2]) / float64(n)}
}

func TestSizeParsing(t *testing.T) {
	d, _ := ParseSizes(DefaultSizes)

	again, err := ParseSizes(d.String())
	if err != nil || again.String() != d.String() {
		t.Errorf("round trip %q -> %q (%v)", d.String(), again.String(), err)
	}

	for _, bad := range []string{"", "60", "60:1-2", "x:1KiB-2KiB", "60:8KiB-1KiB", "60:1KiB-2GiB", "0:1KiB-2KiB"} {
		if _, err := ParseSizes(bad); err == nil {
			t.Errorf("ParseSizes(%q) accepted", bad)
		}
	}
}

func TestMessagesAreDeterministic(t *testing.T) {
	sizes, _ := ParseSizes("1:100KiB-100KiB,1:2KiB-2KiB")
	g := Generator{Seed: 7, Sizes: sizes}

	for n := range 20 {
		a, _ := g.Message("bench-000001", n)
		b, _ := g.Message("bench-000001", n)

		if !bytes.Equal(a, b) {
			t.Fatalf("message %d differs between two calls", n)
		}

		if !bytes.Contains(a, []byte("\r\n\r\n")) || !bytes.HasPrefix(a, []byte("From: ")) {
			t.Fatalf("message %d is no RFC 5322 message: %q", n, a[:min(len(a), 200)])
		}

		large := len(a) > 50<<10

		if large != bytes.Contains(a, []byte("multipart/mixed")) {
			t.Errorf("message %d (%d bytes): attachment iff large", n, len(a))
		}

		if want := map[bool]int{true: 100 << 10, false: 2 << 10}[large]; len(a) < want*9/10 || len(a) > want*11/10+1024 {
			t.Errorf("message %d has %d bytes, want about %d", n, len(a), want)
		}
	}

	other, _ := g.Message("bench-000002", 0)
	first, _ := g.Message("bench-000001", 0)

	if bytes.Equal(other, first) {
		t.Error("two accounts got the same message")
	}
}

func TestLatencyQuantiles(t *testing.T) {
	var l Latency

	for i := 1; i <= 1000; i++ {
		l.Add(time.Duration(i) * time.Millisecond)
	}

	for q, want := range map[float64]time.Duration{0.5: 500 * time.Millisecond, 0.99: 990 * time.Millisecond} {
		got := l.Quantile(q)
		if diff := float64(got-want) / float64(want); diff < -0.001 || diff > 0.021 {
			t.Errorf("q%.2f = %v, want %v within 2 %%", q, got, want)
		}
	}

	s := l.Summary()
	if s.Count != 1000 || s.MaxMS != 1000 || s.MeanMS < 500 || s.MeanMS > 501 {
		t.Errorf("summary %+v", s)
	}

	var empty Latency
	if empty.Quantile(0.5) != 0 || empty.Summary().Count != 0 {
		t.Error("empty histogram")
	}
}

func TestClassify(t *testing.T) {
	for oid, want := range map[string]string{
		"mailboxes": ClassRegistry, "account": ClassAccount, "mbox/0a1b": ClassMbox, "mbox/0a1b/3": ClassMboxShard,
		"msg/ff00/0": ClassMsg, "fts/07": ClassFTS, "ftst/255": ClassFTST, "ftsh/03": ClassFTSH, "ftsd/01": ClassFTSD,
		"ftsmap": ClassFTSMap, "metadata": ClassMetadata, "sieve": ClassSieve, "scrub": ClassScrub,
		"ix/att/04": ClassIx, "ix.accounts/12": ClassStoreIx, "pubseen/x": ClassPubSeen, "whatever": ClassOther,
		"ftx/9c1e00aa00bb00cc/p0": ClassFTXP, "ftx/9c1e00aa00bb00cc/d3": ClassFTXD, "ftx/9c1e00aa00bb00cc/h12": ClassFTXH,
		"ftx/9c1e00aa00bb00cc": ClassOther,
	} {
		if got := Classify(oid); got != want {
			t.Errorf("Classify(%q) = %q, want %q", oid, got, want)
		}
	}
}

// objecterSample is in the format of the client library (Ceph 20).
const objecterSample = `2026-10-02T05:02:28.905+0000 7f41263ca040 10 client.4122.objecter _op_submit op 0x55f91418a840
2026-10-02T05:02:28.906+0000 7f41263ca040 10 client.4122.objecter _op_submit oid mbox/0a1b '@1;bench-000001' '@1;bench-000001' [omap-get-vals in=16b] tid 1 osd.0
2026-10-02T05:02:28.907+0000 7f41263ca040 10 client.4122.objecter _op_submit oid mbox/0a1b '@1;bench-000001' '@1;bench-000001' [assert-version v12,omap-set-vals in=14b] tid 2 osd.0
2026-10-02T05:02:28.908+0000 7f41263ca040 10 client.4122.objecter _op_submit oid msg/ff00/0 '@2;bench-000001' '@2;bench-000001' [writefull 0~3 in=3b] tid 3 osd.0
2026-10-02T05:02:28.909+0000 7f41263ca040 10 client.4122.objecter _op_submit oid mailboxes '@1;bench-000001' '@1;bench-000001' [call lock.lock in=40b] tid 4 osd.0
2026-10-02T05:02:28.910+0000 7f41263ca040 10 client.4122.objecter _op_submit oid ix.accounts/12 '@1;~accounts' '@1;~accounts' [omap-set-vals in=99b] tid 5 osd.0
2026-10-02T05:02:28.911+0000 7f41263ca040 10 client.4122.objecter _op_submit oid other '@3' '@3' [stat] tid 6 osd.0
`

func TestParseObjecterLog(t *testing.T) {
	pools := map[int64]string{1: "idx", 2: "data"}

	c, unparsed, err := ParseObjecterLog(strings.NewReader(objecterSample), pools, nil)
	if err != nil || unparsed != 0 {
		t.Fatal(unparsed, err)
	}

	if got := *c["idx"][ClassMbox]; got != (OpCount{Ops: 2, Reads: 1, Writes: 1}) {
		t.Errorf("mbox %+v", got)
	}

	if got := *c["data"][ClassMsg]; got != (OpCount{Ops: 1, Writes: 1}) {
		t.Errorf("msg %+v", got)
	}

	if got := *c["idx"][ClassRegistry]; got != (OpCount{Ops: 1, Calls: 1}) {
		t.Errorf("registry %+v", got)
	}

	if got := c.Total(); got.Ops != 5 {
		t.Errorf("total %+v (pool 3 must be skipped)", got)
	}

	rates, total := Rates(c, 5)
	if total.Ops != 1 || rates["idx"][ClassMbox].Ops != 0.4 {
		t.Errorf("rates %+v %+v", rates, total)
	}
}

func TestParseObjecterLogFilterAndUnknownLines(t *testing.T) {
	pools := map[int64]string{1: "idx", 2: "data"}

	only, _, err := ParseObjecterLog(strings.NewReader(objecterSample), pools, func(ns string) bool { return IsAccount(ns, "bench-") })
	if err != nil || only.Total().Ops != 4 || only["idx"][ClassStoreIx] != nil {
		t.Errorf("namespace filter: %+v %v", only.Total(), err)
	}

	// A line of an unknown format is counted, not taken, and does not
	// stop the rest.
	broken := "x _op_submit oid broken\n" + objecterSample

	all, unparsed, err := ParseObjecterLog(strings.NewReader(broken), pools, nil)
	if err != nil || unparsed != 1 || all.Total().Ops != 5 {
		t.Errorf("unknown line: %d unparsed, %+v, %v", unparsed, all.Total(), err)
	}
}

func sampleAccounts() []AccountUsage {
	return []AccountUsage{
		{Account: "bench-000000", Messages: 10, Pools: map[string]PoolUsage{
			"idx":  {ClassFTS: {Objects: 4, OmapKeys: 100, OmapBytes: 1000}, ClassMbox: {Objects: 2}},
			"data": {ClassMsg: {Objects: 10, Bytes: 20000}}}},
		{Account: "bench-000001", Messages: 30, Pools: map[string]PoolUsage{
			"idx":  {ClassFTS: {Objects: 4, OmapKeys: 300, OmapBytes: 3000}, ClassMbox: {Objects: 2}},
			"data": {ClassMsg: {Objects: 30, Bytes: 60000}}}},
	}
}

func TestSummarize(t *testing.T) {
	s := Summarize(sampleAccounts())
	if s.Accounts != 2 || s.MessagesPerAccount.Mean != 20 {
		t.Fatalf("summary %+v", s)
	}

	if got := s.PerAccount["idx"][ClassTotal].Objects; got.Mean != 6 || got.Min != 6 {
		t.Errorf("idx total %+v", got)
	}

	if got := s.PerAccount["data"][ClassMsg].Bytes; got.Mean != 40000 || got.Min != 20000 || got.Max != 60000 {
		t.Errorf("msg bytes %+v", got)
	}

	if got := s.PerMessage["data"]; got.Objects != 1 || got.Bytes != 2000 {
		t.Errorf("per message %+v", got)
	}
}

func TestECEstimateAndExtrapolation(t *testing.T) {
	ec := ECEstimate{DataChunks: 2, CodingChunks: 1, StripeUnit: 4096, MinAlloc: 4096}
	ec.Add(1000)     // one stripe: 3 chunks of 4 KiB
	ec.Add(10 << 10) // two stripes: 3 chunks of 8 KiB
	ec.Add(64 << 10) // eight stripes: 3 chunks of 32 KiB

	if want := int64(3*4096 + 3*8192 + 3*32768); ec.Raw != want {
		t.Errorf("raw %d, want %d", ec.Raw, want)
	}

	r := &MeasureReport{IndexPool: "idx", DataPool: "data", Inventory: Summarize(sampleAccounts()), DataRaw: ec}
	r.Extrapolate(1000)

	if r.Extrapolated.IndexObjects != 6000 || r.Extrapolated.DataBytes != 40000*1000 {
		t.Errorf("extrapolated %+v", r.Extrapolated)
	}

	var out bytes.Buffer
	if err := WriteMeasureTable(&out, r); err != nil || !strings.Contains(out.String(), "Extrapolated to 1000 accounts") {
		t.Errorf("table: %v\n%s", err, out.String())
	}
}

// TestPopulate fills accounts in memory through the delivery service,
// and a second run adds only what is missing.
func TestPopulate(t *testing.T) {
	store := memstore.New()
	svc := &delivery.Service{Store: store, Log: discard()}
	sizes, _ := ParseSizes("1:1KiB-4KiB")
	o := PopulateOptions{Prefix: "bench-t-", Accounts: 3, Messages: 8, Mailboxes: ParseMailboxes(DefaultMailboxes),
		InboxShare: 0.5, Gen: Generator{Seed: 3, Sizes: sizes}, Parallel: 2}

	rep, err := Populate(context.Background(), o, store, svc)
	if err != nil {
		t.Fatal(err)
	}

	if rep.Deliveries != 24 || rep.Failures != 0 || rep.Latency.Count != 24 {
		t.Fatalf("first run %+v", rep)
	}

	if total := stored(t, store, "bench-t-", 3); total != 24 {
		t.Errorf("%d messages stored, want 24", total)
	}

	o.Messages = 10

	rep, err = Populate(context.Background(), o, store, svc)
	if err != nil || rep.Deliveries != 6 || rep.AccountsComplete != 0 {
		t.Fatalf("second run %+v %v", rep, err)
	}

	rep, err = Populate(context.Background(), o, store, svc)
	if err != nil || rep.Deliveries != 0 || rep.AccountsComplete != 3 {
		t.Fatalf("third run %+v %v", rep, err)
	}
}

// stored counts the messages of the first n accounts and checks that
// each has the six default mailboxes.
func stored(t *testing.T, store *memstore.Store, prefix string, n int) int {
	t.Helper()

	total := 0

	for i := range n {
		u, err := store.User(AccountName(prefix, i))
		if err != nil {
			t.Fatal(err)
		}

		boxes, _ := u.ListMailboxes()
		if len(boxes) != 6 {
			t.Errorf("account %d has %d mailboxes", i, len(boxes))
		}

		for _, b := range boxes {
			info, err := u.Info(b.Name)
			if err != nil {
				t.Fatal(err)
			}

			total += int(info.UIDNext) - 1 // nothing was expunged
		}

		u.Close()
	}

	return total
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestPopulateRefusesOutsideTheBench(t *testing.T) {
	sizes, _ := ParseSizes(DefaultSizes)
	o := PopulateOptions{Prefix: "load", Accounts: 1, Messages: 1, Mailboxes: []string{"INBOX"},
		Gen: Generator{Sizes: sizes}, Parallel: 1}

	if _, err := Populate(context.Background(), o, memstore.New(), nil); !errors.Is(err, ErrGuard) {
		t.Fatalf("got %v, want a guard refusal", err)
	}
}
