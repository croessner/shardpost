// Command shardpost-load is a load generator for the mail server: it delivers
// messages via LMTP and drives IMAP sessions (LOGIN, SELECT, FETCH, SEARCH,
// STORE, IDLE-less polling) for many users concurrently and prints latency
// percentiles. It is deliberately dependency-free so it can run from the
// tools pod.
//
//	shardpost-load -imap shardpost:143 -lmtp shardpost:24 -users 50 -sessions 4 -duration 60s \
//	         -user-pattern 'load%03d' -password test -deliver-rate 20
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type stats struct {
	mu   sync.Mutex
	lat  map[string][]time.Duration
	errs map[string]int64
}

func (s *stats) add(op string, d time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		s.errs[op]++
		return
	}

	s.lat[op] = append(s.lat[op], d)
}

func (s *stats) report(elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ops := make([]string, 0, len(s.lat))
	for op := range s.lat {
		ops = append(ops, op)
	}

	for op := range s.errs {
		if _, ok := s.lat[op]; !ok {
			ops = append(ops, op)
		}
	}

	sort.Strings(ops)
	fmt.Printf("%-10s %8s %8s %9s %9s %9s %9s %7s\n", "op", "count", "ops/s", "p50", "p90", "p99", "max", "errors")

	for _, op := range ops {
		l := s.lat[op]
		sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })

		pct := func(p float64) time.Duration {
			if len(l) == 0 {
				return 0
			}

			i := int(float64(len(l)-1) * p)

			return l[i]
		}
		fmt.Printf("%-10s %8d %8.1f %9s %9s %9s %9s %7d\n", op, len(l), float64(len(l))/elapsed.Seconds(),
			pct(0.5).Round(time.Millisecond), pct(0.9).Round(time.Millisecond), pct(0.99).Round(time.Millisecond), pct(1).Round(time.Millisecond), s.errs[op])
	}
}

// imapClient is a minimal tagged IMAP client.
type imapClient struct {
	nc  net.Conn
	br  *bufio.Reader
	tag int
}

func dialIMAP(addr string) (*imapClient, error) {
	nc, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}

	c := &imapClient{nc: nc, br: bufio.NewReaderSize(nc, 64<<10)}
	if _, err := c.br.ReadString('\n'); err != nil {
		_ = nc.Close()
		return nil, err
	}

	return c, nil
}

// cmd sends a command and reads until the tagged reply, skipping literals.
func (c *imapClient) cmd(format string, args ...any) (string, error) {
	c.tag++
	tag := fmt.Sprintf("t%d", c.tag)

	_ = c.nc.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(c.nc, tag+" "+format+"\r\n", args...); err != nil {
		return "", err
	}

	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return "", err
		}

		if strings.HasPrefix(line, "+ ") {
			continue
		}

		if i := strings.LastIndex(line, "{"); i >= 0 && strings.HasSuffix(strings.TrimRight(line, "\r\n"), "}") {
			var n int
			if _, err := fmt.Sscanf(line[i:], "{%d}", &n); err == nil {
				buf := make([]byte, n)
				if _, err := readFull(c.br, buf); err != nil {
					return "", err
				}

				continue
			}
		}

		if strings.HasPrefix(line, tag+" ") {
			if strings.HasPrefix(line, tag+" OK") {
				return line, nil
			}

			return line, fmt.Errorf("%s", strings.TrimSpace(line))
		}
	}
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])

		n += m
		if err != nil {
			return n, err
		}
	}

	return n, nil
}

// lmtpDeliver sends one message via LMTP.
func lmtpDeliver(addr, rcpt string, body []byte) error {
	nc, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}

	defer func() { _ = nc.Close() }()

	_ = nc.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(nc)

	if err := lmtpExpect(br, "220"); err != nil {
		return err
	}

	// Write errors are deliberately ignored: a broken connection surfaces as
	// a read error on the following reply.
	steps := []struct{ line, code string }{
		{"LHLO shardpost-load\r\n", "250"},
		{"MAIL FROM:<load@example.org>\r\n", "250"},
		{"RCPT TO:<" + rcpt + ">\r\n", "250"},
		{"DATA\r\n", "354"},
	}
	for _, step := range steps {
		_, _ = io.WriteString(nc, step.line)

		if err := lmtpExpect(br, step.code); err != nil {
			return err
		}
	}

	_, _ = nc.Write(body)
	_, _ = io.WriteString(nc, "\r\n.\r\n")

	if err := lmtpExpect(br, "250"); err != nil {
		return err
	}

	_, _ = io.WriteString(nc, "QUIT\r\n")

	return nil
}

// lmtpExpect reads one (possibly multi-line) LMTP reply and checks its code.
func lmtpExpect(br *bufio.Reader, code string) error {
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}

		if len(line) < 4 {
			return fmt.Errorf("short line %q", line)
		}

		if line[3] == '-' {
			continue
		}

		if !strings.HasPrefix(line, code) {
			return fmt.Errorf("expected %s got %s", code, strings.TrimSpace(line))
		}

		return nil
	}
}

var words = strings.Fields("invoice meeting lunch report quarterly budget release deploy ceph rados omap shard cluster latency review ticket customer support renewal contract")

func randomMessage(n int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: sender%d@example.org\r\nTo: user@example.org\r\nSubject: %s %s %d\r\nDate: %s\r\nMessage-ID: <%d.%d@shardpost-load>\r\n\r\n",
		rand.Intn(1000), words[rand.Intn(len(words))], words[rand.Intn(len(words))], n, time.Now().Format(time.RFC1123Z), time.Now().UnixNano(), n)

	for i := 0; i < 20+rand.Intn(200); i++ {
		b.WriteString(words[rand.Intn(len(words))])
		b.WriteByte(' ')

		if i%12 == 11 {
			b.WriteString("\r\n")
		}
	}

	b.WriteString("\r\n")

	return []byte(b.String())
}

// loadConfig holds the command-line parameters.
type loadConfig struct {
	imapAddr string
	lmtpAddr string
	users    int
	sessions int
	duration time.Duration
	pattern  string
	domain   string
	password string
	rate     float64
}

func parseFlags() loadConfig {
	imapAddr := flag.String("imap", "shardpost:143", "IMAP address")
	lmtpAddr := flag.String("lmtp", "shardpost:24", "LMTP address")
	users := flag.Int("users", 10, "number of users")
	sessions := flag.Int("sessions", 2, "concurrent IMAP sessions per user")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	pattern := flag.String("user-pattern", "load%03d", "user name pattern")
	domain := flag.String("domain", "example.org", "recipient domain")
	password := flag.String("password", "test", "password for all users")
	rate := flag.Float64("deliver-rate", 10, "LMTP deliveries per second (total)")

	flag.Parse()

	return loadConfig{
		imapAddr: *imapAddr,
		lmtpAddr: *lmtpAddr,
		users:    *users,
		sessions: *sessions,
		duration: *duration,
		pattern:  *pattern,
		domain:   *domain,
		password: *password,
		rate:     *rate,
	}
}

// deliverLoop delivers messages at a fixed rate, round-robin over users,
// until stop is closed.
func deliverLoop(cfg loadConfig, st *stats, stop <-chan struct{}, delivered *atomic.Int64) {
	ticker := time.NewTicker(time.Duration(float64(time.Second) / cfg.rate))
	defer ticker.Stop()

	n := 0

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			n++
			user := fmt.Sprintf(cfg.pattern, n%cfg.users)
			body := randomMessage(n)

			go func() {
				t := time.Now()
				err := lmtpDeliver(cfg.lmtpAddr, user+"@"+cfg.domain, body)
				st.add("lmtp", time.Since(t), err)

				if err == nil {
					delivered.Add(1)
				}
			}()
		}
	}
}

// stopped reports whether stop has been closed.
func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// imapSessionLoop runs a client-like loop of SELECT, FETCH, SEARCH, STORE,
// NOOP for one user until stop is closed.
func imapSessionLoop(cfg loadConfig, user string, st *stats, stop <-chan struct{}) {
	for {
		if stopped(stop) {
			return
		}

		c, ok := imapLogin(cfg, user, st)
		if !ok {
			continue
		}

		if !imapRunOps(c, st, stop) {
			return
		}

		_, _ = c.cmd("LOGOUT")
		_ = c.nc.Close()
	}
}

// imapLogin connects, logs in and selects INBOX. On failure it records the
// error, closes the connection and returns false.
func imapLogin(cfg loadConfig, user string, st *stats) (*imapClient, bool) {
	t := time.Now()

	c, err := dialIMAP(cfg.imapAddr)
	if err != nil {
		st.add("connect", time.Since(t), err)
		time.Sleep(time.Second)

		return nil, false
	}

	_, err = c.cmd("LOGIN %s %s", user, cfg.password)
	st.add("login", time.Since(t), err)

	if err != nil {
		_ = c.nc.Close()

		time.Sleep(time.Second)

		return nil, false
	}

	t = time.Now()
	_, err = c.cmd("SELECT INBOX")
	st.add("select", time.Since(t), err)

	if err != nil {
		_ = c.nc.Close()

		return nil, false
	}

	return c, true
}

// imapRunOps issues up to 20 random commands. It returns false when stop was
// closed (the connection is then already closed).
func imapRunOps(c *imapClient, st *stats, stop <-chan struct{}) bool {
	for i := 0; i < 20; i++ {
		if stopped(stop) {
			_ = c.nc.Close()

			return false
		}

		ops := []struct {
			name string
			cmd  string
		}{
			{"select", "SELECT INBOX"},
			{"fetch", "FETCH 1:* (UID FLAGS ENVELOPE RFC822.SIZE)"},
			{"search", "SEARCH SUBJECT " + words[rand.Intn(len(words))]},
			{"store", "STORE 1:* +FLAGS.SILENT (\\Seen)"},
			{"noop", "NOOP"},
			{"body", "FETCH 1 BODY.PEEK[]"},
		}
		op := ops[rand.Intn(len(ops))]
		t := time.Now()
		_, err := c.cmd(op.cmd)
		st.add(op.name, time.Since(t), err)

		if err != nil && strings.Contains(err.Error(), "BYE") {
			break
		}

		time.Sleep(time.Duration(rand.Intn(200)) * time.Millisecond)
	}

	return true
}

func main() {
	cfg := parseFlags()

	st := &stats{lat: map[string][]time.Duration{}, errs: map[string]int64{}}
	stop := make(chan struct{})

	var (
		wg        sync.WaitGroup
		delivered atomic.Int64
	)

	start := time.Now()

	// Deliveries at a fixed rate, round-robin over users.
	wg.Add(1)
	go func() {
		defer wg.Done()

		deliverLoop(cfg, st, stop, &delivered)
	}()

	// IMAP sessions: a client-like loop of SELECT, FETCH, SEARCH, STORE, NOOP.
	for u := 0; u < cfg.users; u++ {
		for s := 0; s < cfg.sessions; s++ {
			wg.Add(1)
			go func(user string) {
				defer wg.Done()

				imapSessionLoop(cfg, user, st, stop)
			}(fmt.Sprintf(cfg.pattern, u))
		}
	}

	time.Sleep(cfg.duration)
	close(stop)
	wg.Wait()

	elapsed := time.Since(start)
	fmt.Printf("shardpost-load: %d users x %d sessions, %s, %d messages delivered\n\n", cfg.users, cfg.sessions, elapsed.Round(time.Second), delivered.Load())
	st.report(elapsed)
	os.Exit(0)
}
