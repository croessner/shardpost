package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"shardpost/internal/bench"
)

// fillAccount delivers cfg.count messages of the bench corpus to one
// account over LMTP, at most cfg.fillRate per second over cfg.parallel
// connections, and returns the exit code: 0 when every message was
// delivered, 1 when some failed, 2 for wrong options. A message that
// fails is reported with its number; run again with -first to continue.
func fillAccount(cfg loadConfig, out io.Writer) int {
	gen, err := fillGenerator(cfg)
	if err != nil {
		_, _ = fmt.Fprintln(out, "shardpost-load:", err)
		return 2
	}

	f := &filler{cfg: cfg, gen: gen, out: out, st: &stats{lat: map[string][]time.Duration{}, errs: map[string]int64{}}}

	var wg sync.WaitGroup

	next := make(chan int)

	for range cfg.parallel {
		wg.Go(func() {
			for n := range next {
				f.deliver(n)
			}
		})
	}

	start := time.Now()
	tick := time.NewTicker(time.Duration(float64(time.Second) / cfg.fillRate))

	for n := cfg.first; n < cfg.first+cfg.count; n++ {
		if n > cfg.first {
			<-tick.C
		}

		next <- n
	}

	tick.Stop()
	close(next)
	wg.Wait()

	elapsed := time.Since(start)
	_, _ = fmt.Fprintf(out, "shardpost-load: corpus %s, seed %d, account %s, messages %d to %d: %d delivered (%d bytes), %d failed, %s\n\n",
		gen.Corpus, cfg.seed, cfg.rcpt, cfg.first, cfg.first+cfg.count-1, f.delivered.Load(), f.bytes.Load(), f.failed.Load(),
		elapsed.Round(time.Second))
	f.st.report(elapsed)

	if f.failed.Load() > 0 {
		return 1
	}

	return 0
}

// fillGenerator checks the options of -corpus and returns the generator.
func fillGenerator(cfg loadConfig) (bench.Generator, error) {
	corpus, err := bench.ParseCorpus(cfg.corpus)
	if err != nil {
		return bench.Generator{}, err
	}

	sizes, err := bench.ParseSizes(cfg.sizes)
	if err != nil {
		return bench.Generator{}, err
	}

	if cfg.rcpt == "" || cfg.count <= 0 || cfg.first < 0 || cfg.fillRate <= 0 || cfg.parallel <= 0 {
		return bench.Generator{}, errors.New("-corpus needs -rcpt, -count above 0, -first not below 0, -rate and -parallel above 0")
	}

	return bench.Generator{Seed: cfg.seed, Sizes: sizes, Corpus: corpus}, nil
}

// filler delivers the messages of one fill.
type filler struct {
	cfg loadConfig
	gen bench.Generator
	out io.Writer
	st  *stats

	delivered, failed, bytes atomic.Int64
}

// deliver delivers message n and counts it.
func (f *filler) deliver(n int) {
	raw, _ := f.gen.Message(f.cfg.rcpt, n)

	t := time.Now()
	err := lmtpDeliver(f.cfg.lmtpAddr, f.cfg.rcpt, raw)
	f.st.add("lmtp", time.Since(t), err)

	if err != nil {
		f.failed.Add(1)

		_, _ = fmt.Fprintf(f.out, "message %d: %v\n", n, err)

		return
	}

	f.delivered.Add(1)
	f.bytes.Add(int64(len(raw)))
}
