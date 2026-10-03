package main

import (
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
	corpus, err := bench.ParseCorpus(cfg.corpus)
	if err != nil {
		_, _ = fmt.Fprintln(out, "shardpost-load:", err)
		return 2
	}

	sizes, err := bench.ParseSizes(cfg.sizes)
	if err != nil || cfg.rcpt == "" || cfg.count <= 0 || cfg.first < 0 || cfg.fillRate <= 0 || cfg.parallel <= 0 {
		_, _ = fmt.Fprintln(out, "shardpost-load: -corpus needs -rcpt, -count above 0, -rate above 0, -parallel above 0 and valid -sizes", err)
		return 2
	}

	gen := bench.Generator{Seed: cfg.seed, Sizes: sizes, Corpus: corpus}
	st := &stats{lat: map[string][]time.Duration{}, errs: map[string]int64{}}

	var (
		delivered, failed, bytesOut atomic.Int64
		wg                          sync.WaitGroup
	)

	next := make(chan int)

	for range cfg.parallel {
		wg.Go(func() {
			for n := range next {
				raw, _ := gen.Message(cfg.rcpt, n)

				t := time.Now()
				err := lmtpDeliver(cfg.lmtpAddr, cfg.rcpt, raw)
				st.add("lmtp", time.Since(t), err)

				if err != nil {
					failed.Add(1)
					_, _ = fmt.Fprintf(out, "message %d: %v\n", n, err)

					continue
				}

				delivered.Add(1)
				bytesOut.Add(int64(len(raw)))
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
		corpus, cfg.seed, cfg.rcpt, cfg.first, cfg.first+cfg.count-1, delivered.Load(), bytesOut.Load(), failed.Load(), elapsed.Round(time.Second))
	st.report(elapsed)

	if failed.Load() > 0 {
		return 1
	}

	return 0
}
