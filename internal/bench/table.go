package bench

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// printer writes formatted lines and keeps the first error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

// table writes the rows of a tabwriter through p.
func (p *printer) table(align uint, rows func(t *printer)) {
	tw := tabwriter.NewWriter(p.w, 2, 4, 2, ' ', align)
	t := &printer{w: tw}

	rows(t)

	if err := tw.Flush(); t.err == nil {
		t.err = err
	}

	if p.err == nil {
		p.err = t.err
	}
}

// WritePopulateTable writes a population report for people.
func WritePopulateTable(w io.Writer, r *PopulateReport) error {
	p := &printer{w: w}

	p.table(0, func(t *printer) {
		t.f("accounts\t%s%06d .. %s%06d (%d, %d already complete, %d failed)\n",
			r.Prefix, r.First, r.Prefix, r.First+r.Accounts-1, r.Accounts, r.AccountsComplete, r.AccountsFailed)
		t.f("messages per account\t%d into %v, sizes %s, seed %d\n", r.MessagesPerAccount, r.Mailboxes, r.Sizes, r.Seed)
		t.f("deliveries\t%d (%d failed), %s in %.1f s, %.1f/s\n", r.Deliveries, r.Failures, human(r.Bytes), r.Seconds, r.PerSecond)
		t.latency("delivery latency\t", r.Latency)
		t.f("CAS retries\t%.0f (%.3f per delivery) by kind %v, by class %v\n",
			r.CASRetries.Total, r.CASRetries.PerDelivery, r.CASRetries.ByKind, r.CASRetries.ByClass)
	})

	return p.err
}

func (p *printer) latency(what string, l LatencySummary) {
	p.f("%sp50 %.2f ms  p90 %.2f ms  p99 %.2f ms  max %.2f ms  mean %.2f ms  (n=%d)\n",
		what, l.P50MS, l.P90MS, l.P99MS, l.MaxMS, l.MeanMS, l.Count)
}

// WriteMeasureTable writes a measurement report for people.
func WriteMeasureTable(w io.Writer, r *MeasureReport) error {
	p := &printer{w: w}
	inv := r.Inventory

	p.f("Inventory of %d accounts (%s), messages per account %.1f (min %d, max %d); fts_shards %d\n",
		inv.Accounts, r.Prefix, inv.MessagesPerAccount.Mean, inv.MessagesPerAccount.Min, inv.MessagesPerAccount.Max, r.FTSShards)
	p.table(tabwriter.AlignRight, func(t *printer) { inventoryRows(t, r) })
	p.f("(per message: the account mean divided by the message count; the full-text shards do not grow with it)\n")
	p.storeWide(r)
	p.extrapolation(r.Extrapolated)

	if r.Search != nil {
		p.search(r.Search)
	}

	if r.Delivery != nil {
		p.probe(r.Delivery)
	}

	if r.Contention != nil {
		p.probe(r.Contention)
	}

	for _, o := range []*OpenReport{r.UserOpen, r.SessionOpen} {
		if o != nil {
			p.open(o)
		}
	}

	if r.Expunge != nil {
		p.round(r.Expunge)
	}

	for _, n := range r.Notes {
		p.f("note: %s\n", n)
	}

	return p.err
}

func inventoryRows(t *printer, r *MeasureReport) {
	inv := r.Inventory

	t.f("pool\tclass\tobjects/acct\tmin\tmax\tbytes/acct\tomap keys/acct\tomap bytes/acct\t\n")

	for _, pool := range sortedKeys(inv.PerAccount) {
		for _, class := range sortedKeys(inv.PerAccount[pool]) {
			c := inv.PerAccount[pool][class]
			t.f("%s\t%s\t%.1f\t%d\t%d\t%s\t%.0f\t%s\t\n", pool, class, c.Objects.Mean, c.Objects.Min, c.Objects.Max,
				human(int64(c.Bytes.Mean)), c.OmapKeys.Mean, human(int64(c.OmapBytes.Mean)))
		}
	}

	for _, pool := range sortedKeys(inv.PerMessage) {
		u := inv.PerMessage[pool]
		t.f("%s\tper message\t%d\t\t\t%s\t%d\t%s\t\n", pool, u.Objects, human(u.Bytes), u.OmapKeys, human(u.OmapBytes))
	}

	t.f("data raw estimate\tEC %d+%d\t\t\t\t%s for %s\toverhead\t%.2fx\t\n",
		r.DataRaw.DataChunks, r.DataRaw.CodingChunks, human(r.DataRaw.Raw), human(r.DataRaw.Logical), r.DataRaw.Overhead)
}

func (p *printer) storeWide(r *MeasureReport) {
	p.f("\nPools (cluster statistics, all namespaces, reported by the OSDs with a delay) and store-wide objects\n")

	for _, pool := range sortedKeys(r.Pools) {
		s := r.Pools[pool]
		p.f("  %s: %d objects, %s, %d reads, %d writes\n", pool, s.Objects, human(int64(s.Bytes)), s.Reads, s.Writes)
	}

	for _, pool := range sortedKeys(r.StoreWide) {
		for _, class := range sortedKeys(r.StoreWide[pool]) {
			u := r.StoreWide[pool][class]
			p.f("  %s store-wide %s: %d objects, %s data, %d omap keys, %s omap\n",
				pool, class, u.Objects, human(u.Bytes), u.OmapKeys, human(u.OmapBytes))
		}
	}
}

func (p *printer) extrapolation(e *Extrapolation) {
	if e == nil {
		return
	}

	p.f("\nExtrapolated to %d accounts: index %.3g objects, %.3g omap keys, %s omap; data %.3g objects, %s (raw estimate %s)\n",
		e.Accounts, e.IndexObjects, e.IndexKeys, human(int64(e.IndexOmap)), e.DataObjects, human(int64(e.DataBytes)), human(int64(e.DataRawBytes)))
}

func (p *printer) probe(d *ProbeReport) {
	p.f("\nDelivery into populated accounts (%d accounts, %d deliveries per round, %d at a time into one account)\n",
		d.Accounts, d.Deliveries, max(d.Parallel, 1))
	p.latency("  latency ", d.Latency)
	p.f("  %.1f deliveries/s\n", d.PerSecond)
	p.f("  CAS retries %.0f (%.3f per delivery) by kind %v, by class %v\n",
		d.CASRetries.Total, d.CASRetries.PerDelivery, d.CASRetries.ByKind, d.CASRetries.ByClass)
	p.ops("per delivery", d.Ops, d.Total)
}

func (p *printer) round(r *RoundReport) {
	p.f("\n%s (%d accounts, %d calls per round)\n", r.What, r.Accounts, r.Count)
	p.latency("  latency ", r.Latency)

	if r.CASRetries.Total > 0 {
		p.f("  CAS retries %.0f (%.3f per call) by kind %v, by class %v\n",
			r.CASRetries.Total, r.CASRetries.PerDelivery, r.CASRetries.ByKind, r.CASRetries.ByClass)
	}

	p.ops("per call", r.Ops, r.Total)
}

func (p *printer) search(r *SearchReport) {
	p.round(&r.RoundReport)
	p.f("  result fingerprint %s", r.Fingerprint)

	if r.Mismatch {
		p.f(" (the traced round found something else)")
	}

	p.f("\n")
	p.table(tabwriter.AlignRight, func(t *printer) {
		t.f("  key\tkind\trare\ttext\tcandidates\tnot narrowed\twords\tfingerprint\t\n")

		for _, q := range r.Queries {
			t.f("  %s\t%s\t%v\t%s\t%d\t%d\t%d\t%s\t\n",
				q.Key, q.Kind, q.Rare, q.Text, q.Candidates, q.NotNarrowed, q.Words, q.Fingerprint[:12])
		}
	})
}

func (p *printer) open(o *OpenReport) {
	p.f("\n%s (%d accounts)\n", o.What, o.Count)
	p.latency("  latency ", o.Latency)

	if o.HeapBytes > 0 {
		p.f("  Go heap per open view %s\n", human(o.HeapBytes))
	}

	p.ops("per open", o.Ops, o.Total)
}

func (p *printer) ops(per string, ops map[string]map[string]OpRate, total OpRate) {
	p.table(tabwriter.AlignRight, func(t *printer) {
		t.f("  pool\tclass\tops %s\treads\twrites\tcalls\t\n", per)

		for _, pool := range sortedKeys(ops) {
			for _, class := range sortedKeys(ops[pool]) {
				o := ops[pool][class]
				t.f("  %s\t%s\t%.2f\t%.2f\t%.2f\t%.2f\t\n", pool, class, o.Ops, o.Reads, o.Writes, o.Calls)
			}
		}

		t.f("  all\ttotal\t%.2f\t%.2f\t%.2f\t%.2f\t\n", total.Ops, total.Reads, total.Writes, total.Calls)
	})
}
