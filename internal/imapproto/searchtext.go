package imapproto

import (
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/membudget"
	"shardpost/internal/model"
)

// IMAP SEARCH text keys match substrings of the decoded message without
// regard to case (RFC 3501 6.4.4, RFC 9051 6.4.4). The full-text index
// cannot answer that alone; it narrows:
//
//   - prepareText asks the index, for every text key wherever it is in
//     the tree, for a superset of the messages whose header and whose
//     body can contain the value (model.User.SearchCandidates).
//   - searchCandidates combines them over the tree — AND intersects, OR
//     unites, NOT does not narrow — into the messages worth looking at.
//   - Each of those is checked against the tree in a pool of
//     Options.VerifyParallel workers. A key checks the header against
//     the header text the store keeps (model.User.HeaderTexts, read for
//     a batch of messages at once) and reads the message only for the
//     body, and only where the body candidates allow it.
//
// With Options.WordSearch the index answers SUBJECT, BODY, TEXT and
// HEADER Subject alone, by word, wherever they are in the tree.

// verifyBatch is how many messages the header texts are read for at
// once.
const verifyBatch = 256

// Retrying a read the memory budget refused.
const (
	budgetRetries = 5
	budgetBackoff = 50 * time.Millisecond
)

// defaultVerifyParallel is Options.VerifyParallel when it is 0.
const defaultVerifyParallel = 8

// prepareText resolves the text keys of the tree against the index.
func (c *conn) prepareText(cr *criteria) error {
	for i := range cr.text {
		if err := c.prepareCrit(&cr.text[i]); err != nil {
			return err
		}
	}

	for _, n := range cr.not {
		if err := c.prepareText(n); err != nil {
			return err
		}
	}

	for _, or := range cr.or {
		for _, b := range or {
			if err := c.prepareText(b); err != nil {
				return err
			}
		}
	}

	return nil
}

// prepareCrit resolves one text key.
func (c *conn) prepareCrit(t *textCrit) error {
	if len(fts.Tokenize(t.value)) == 0 {
		return nil // "header exists", or nothing to look up
	}

	var err error

	switch {
	case c.s.o.WordSearch && indexAnswers(*t):
		t.exact, err = c.mu.SearchFields(t.fields, t.value)
		t.byIndex = err == nil && t.exact != nil
	case isText(*t):
		if t.hdrCand, err = c.mu.SearchCandidates([]string{fts.FieldHeader}, t.value); err == nil {
			t.bodyCand, err = c.mu.SearchCandidates([]string{fts.FieldBody}, t.value)
		}
	case isBody(*t):
		t.bodyCand, err = c.mu.SearchCandidates(t.fields, t.value)
	default:
		t.hdrCand, err = c.mu.SearchCandidates(t.fields, t.value)
	}

	return err
}

func isText(t textCrit) bool { return t.key == "" && len(t.fields) == 2 }
func isBody(t textCrit) bool {
	return t.key == "" && len(t.fields) == 1 && t.fields[0] == fts.FieldBody
}

// indexAnswers reports whether the index answers a criterion by itself
// in word search: SUBJECT, BODY, TEXT and HEADER Subject. HEADER <other>
// shares the "all headers" field, and FROM and TO/CC/BCC share fields:
// for those the index only narrows the candidates.
func indexAnswers(t textCrit) bool {
	otherHeader := t.key != "" && headerFields(t.key)[0] == fts.FieldHeader

	return !otherHeader && t.fields[0] != fts.FieldTo && t.fields[0] != fts.FieldFrom
}

// critCandidates is the superset of the messages a key can match; nil
// when it is not narrowed.
func critCandidates(t textCrit) map[string]bool {
	switch {
	case t.byIndex:
		return t.exact
	case isText(t):
		return union(t.hdrCand, t.bodyCand)
	case isBody(t):
		return t.bodyCand
	}

	return t.hdrCand
}

// searchCandidates is the superset of the messages the tree can match;
// nil when it is not narrowed.
func searchCandidates(cr *criteria) map[string]bool {
	var sets []map[string]bool

	for _, t := range cr.text {
		if s := critCandidates(t); s != nil {
			sets = append(sets, s)
		}
	}

	for _, or := range cr.or {
		if s := union(searchCandidates(or[0]), searchCandidates(or[1])); s != nil {
			sets = append(sets, s)
		}
	}

	// NOT narrows nothing: the messages a key cannot match are the ones
	// its negation may match.
	return intersect(sets)
}

// union is nil when either side is not narrowed.
func union(a, b map[string]bool) map[string]bool {
	if a == nil || b == nil {
		return nil
	}

	out := make(map[string]bool, len(a)+len(b))
	for g := range a {
		out[g] = true
	}

	for g := range b {
		out[g] = true
	}

	return out
}

// intersect is nil without sets.
func intersect(sets []map[string]bool) map[string]bool {
	if len(sets) == 0 {
		return nil
	}

	out := map[string]bool{}

	for g := range sets[0] {
		in := true

		for _, s := range sets[1:] {
			if !s[g] {
				in = false

				break
			}
		}

		if in {
			out[g] = true
		}
	}

	return out
}

// needsHeader reports whether a key of the tree checks header text.
func needsHeader(cr *criteria) bool {
	for _, t := range cr.text {
		if !t.byIndex && !isBody(t) {
			return true
		}
	}

	for _, n := range cr.not {
		if needsHeader(n) {
			return true
		}
	}

	for _, or := range cr.or {
		if needsHeader(or[0]) || needsHeader(or[1]) {
			return true
		}
	}

	return false
}

// hasText reports whether the tree has text keys.
func hasText(cr *criteria) bool {
	if len(cr.text) > 0 {
		return true
	}

	for _, n := range cr.not {
		if hasText(n) {
			return true
		}
	}

	for _, or := range cr.or {
		if hasText(or[0]) || hasText(or[1]) {
			return true
		}
	}

	return false
}

// runSearch evaluates criteria against the snapshot.
func (c *conn) runSearch(cr *criteria) ([]int, []model.Meta, error) {
	if err := c.prepareText(cr); err != nil {
		return nil, nil, err
	}

	cands := searchCandidates(cr)

	var idx []int

	for i := range c.msgs {
		if cands == nil || cands[c.msgs[i].GUID] {
			idx = append(idx, i)
		}
	}

	hit := make([]bool, len(c.msgs))

	if !hasText(cr) {
		for _, i := range idx {
			hit[i] = c.match(i+1, &c.msgs[i], cr, nil)
		}
	} else if err := c.verify(cr, idx, hit); err != nil {
		return nil, nil, err // a message could not be read: no partial result
	}

	var (
		seqs  []int
		metas []model.Meta
	)

	for i, ok := range hit {
		if ok {
			seqs = append(seqs, i+1)
			metas = append(metas, c.msgs[i])
		}
	}

	return seqs, metas, nil
}

// verify checks the messages idx against the tree, a batch at a time,
// with Options.VerifyParallel workers, and sets hit.
func (c *conn) verify(cr *criteria, idx []int, hit []bool) error {
	workers := c.s.o.VerifyParallel
	if workers <= 0 {
		workers = defaultVerifyParallel
	}

	header := needsHeader(cr)

	watch := c.watchClient()
	defer watch.stop()

	for len(idx) > 0 {
		if watch.gone() {
			return errClientGone
		}

		batch := idx[:min(verifyBatch, len(idx))]
		idx = idx[len(batch):]

		var texts map[string]string

		if header {
			guids := make([]string, len(batch))
			for k, i := range batch {
				guids[k] = c.msgs[i].GUID
			}

			var err error
			if texts, err = c.mu.HeaderTexts(guids); err != nil {
				return err
			}
		}

		if err := c.verifyBatch(cr, batch, hit, texts, workers, watch); err != nil {
			return err
		}
	}

	return nil
}

// verifyBatch checks one batch with a pool of workers.
func (c *conn) verifyBatch(cr *criteria, batch []int, hit []bool, texts map[string]string, workers int, watch *clientWatch) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ferr error
	)

	next := make(chan int)

	for w := 0; w < min(workers, len(batch)); w++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range next {
				ev := &textEval{c: c, m: &c.msgs[i]}
				if h, ok := texts[ev.m.GUID]; ok {
					ev.hdr, ev.hdrRead = h, true
				}

				ok := c.match(i+1, ev.m, cr, ev)

				mu.Lock()
				if ev.err != nil && ferr == nil {
					ferr = ev.err
				}

				hit[i] = ok && !ev.gone
				mu.Unlock()
			}
		}()
	}

	for _, i := range batch {
		mu.Lock()
		failed := ferr != nil
		mu.Unlock()

		if failed {
			break
		}

		if watch.gone() {
			mu.Lock()
			ferr = errClientGone
			mu.Unlock()

			break
		}

		next <- i
	}

	close(next)
	wg.Wait()

	return ferr
}

// errClientGone ends a search whose client went away.
var errClientGone = errors.New("search: the client is gone")

// searchFailed answers a SEARCH, SORT or THREAD whose search failed. A
// search whose client went away is not answered and not logged as a
// storage error: the error goes back to the command loop, which ends
// the session.
func (c *conn) searchFailed(tag string, err error) error {
	if errors.Is(err, errClientGone) {
		return err
	}

	c.storeErr(tag, err)

	return nil
}

// clientWatch notices, while a search checks messages, that its client
// went away (it closed the connection, or the server closed it), so that
// the search stops instead of reading messages for nobody. It peeks at
// the connection without consuming what a client sends — a pipelined
// command stays for the command loop — under read deadlines of its own,
// re-armed every watchInterval, so that it keeps watching however long
// the search takes.
type clientWatch struct {
	c        *conn
	lost     atomic.Bool
	mu       sync.Mutex // orders re-arming the deadline against stop
	stopped  bool
	wg       sync.WaitGroup
	watching bool
}

// watchInterval is the read deadline of one peek of the watch.
const watchInterval = time.Second

// watchClient starts the watch. A compressed session is not peeked at:
// the read deadline that ends a peek would break the inflate stream;
// there only the server closing the connection is noticed.
func (c *conn) watchClient() *clientWatch {
	w := &clientWatch{c: c}
	if c.compressed {
		return w
	}

	w.watching = true
	w.wg.Add(1)

	go w.run()

	return w
}

// run peeks until the client sends something (the watch is done), the
// connection fails (the client is gone) or stop.
func (w *clientWatch) run() {
	defer w.wg.Done()

	for {
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()

			return
		}

		_ = w.c.nc.SetReadDeadline(time.Now().Add(watchInterval))
		w.mu.Unlock()

		_, err := w.c.r.br.Peek(1)
		if err == nil {
			return
		}

		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			w.lost.Store(true)

			return
		}
	}
}

// gone reports whether the search should stop.
func (w *clientWatch) gone() bool {
	return w.lost.Load() || w.c.closing.Load()
}

// stop ends the watch: an expired read deadline returns the peek at
// once. The deadline is cleared afterwards; the command loop sets its
// own before it reads again.
func (w *clientWatch) stop() {
	if !w.watching {
		return
	}

	w.mu.Lock()
	w.stopped = true
	_ = w.c.nc.SetReadDeadline(time.Now())
	w.mu.Unlock()

	w.wg.Wait()

	_ = w.c.nc.SetReadDeadline(time.Time{})
}

func (c *conn) match(seq int, m *model.Meta, cr *criteria, ev *textEval) bool {
	if !c.matchPlain(seq, m, cr) {
		return false
	}

	for _, t := range cr.text {
		if !ev.crit(t) {
			return false
		}
	}

	for _, n := range cr.not {
		if c.match(seq, m, n, ev) {
			return false
		}
	}

	for _, or := range cr.or {
		if !c.match(seq, m, or[0], ev) && !c.match(seq, m, or[1], ev) {
			return false
		}
	}

	return true
}

// textEval checks the text keys against one message, reading what it
// needs once: the header text (from the store, or the header of the
// message) and the body text (the message, under the memory budget).
type textEval struct {
	c       *conn
	m       *model.Meta
	hdr     string // fts.HeaderText
	body    string // lower-cased fts.BodyText
	hdrRead bool
	read    bool
	gone    bool  // the message went away: no match
	err     error // a read failed: the command fails
}

// crit checks one text key.
func (ev *textEval) crit(t textCrit) bool {
	if ev.gone || ev.err != nil {
		return false
	}

	g := ev.m.GUID
	if t.byIndex {
		return t.exact[g]
	}

	v := strings.ToLower(t.value)

	// a part the index rules out is not read
	hdr := (t.hdrCand == nil || t.hdrCand[g]) && !isBody(t) && ev.headerHas(t, v)

	return hdr || (isText(t) || isBody(t)) && (t.bodyCand == nil || t.bodyCand[g]) && strings.Contains(ev.bodyText(), v)
}

// headerHas checks the header part of a key: the field of HEADER
// <name> and the address keys, the fields of SUBJECT, FROM, TO, CC and
// BCC, the whole header for TEXT.
func (ev *textEval) headerHas(t textCrit, v string) bool {
	switch {
	case t.key != "":
		return ev.headerKey(strings.ToLower(t.key), v)
	case isText(t):
		return strings.Contains(ev.header(), v)
	}

	return headerFieldContains(ev.header(), t.fields[0], v) || envAddrContains(ev.m, t.fields[0], v)
}

// headerKey checks HEADER <name> <value> and the address keys: the
// value of a field of that name, or the parsed addresses.
func (ev *textEval) headerKey(name, v string) bool {
	key := name + ":"

	for _, line := range strings.Split(ev.header(), "\r\n") {
		if strings.HasPrefix(line, key) && strings.Contains(line[len(key):], v) {
			return true
		}
	}

	return envAddrContains(ev.m, name, v)
}

// header returns the header text: from the store, else from the header
// of the message.
func (ev *textEval) header() string {
	if !ev.hdrRead {
		ev.hdrRead = true

		if raw, rel, ok := ev.readHeader(); ok {
			ev.hdr = fts.HeaderText(raw)

			rel()
		}
	}

	return ev.hdr
}

// bodyText returns the lower-cased body text as the index sees it. The
// message is held under the memory budget only while its text is taken
// out of it, so that a worker never waits for budget while holding some.
func (ev *textEval) bodyText() string {
	if !ev.read {
		ev.read = true

		if raw, rel, ok := ev.readMessage(); ok {
			ev.body = strings.ToLower(fts.BodyText(raw))

			rel()
		}
	}

	return ev.body
}

// headerProbe is how much of a message is read first for its header,
// headerMax where that gives up and reads the whole message.
const (
	headerProbe = 16 << 10
	headerMax   = 1 << 20
)

// readHeader reads the header of the message: its first bytes where
// the store can read a range, else the message.
func (ev *textEval) readHeader() ([]byte, func(), bool) {
	blobs, ok := ev.c.mu.(model.Blobs)
	if !ok || ev.m.Size <= headerProbe {
		return ev.readMessage()
	}

	for want := int64(headerProbe); want <= headerMax; want *= 4 {
		raw, rel, ok := ev.budgeted(min(want, ev.m.Size), func() ([]byte, error) {
			return blobs.ReadBlob(ev.m.GUID, ev.m.Size, 0, min(want, ev.m.Size))
		})
		if !ok {
			return nil, nil, false
		}

		if headerComplete(raw) || int64(len(raw)) >= ev.m.Size {
			return raw, rel, true
		}

		rel()
	}

	return ev.readMessage()
}

// headerComplete reports whether raw holds the empty line that ends
// the header.
func headerComplete(raw []byte) bool {
	s := string(raw)

	return strings.Contains(s, "\r\n\r\n") || strings.Contains(s, "\n\n")
}

// readMessage reads the whole message.
func (ev *textEval) readMessage() ([]byte, func(), bool) {
	return ev.budgeted(ev.m.Size, func() ([]byte, error) { return ev.c.mu.Read(ev.m) })
}

// budgeted reads under the memory budget and returns the release of
// the reservation. A budget that stays exhausted is retried with
// backoff before the command fails; a message that is gone is no match.
func (ev *textEval) budgeted(n int64, read func() ([]byte, error)) ([]byte, func(), bool) {
	rel, err := membudget.Acquire(n)
	for try, wait := 0, budgetBackoff; errors.Is(err, membudget.ErrBusy) && try < budgetRetries; try, wait = try+1, wait*2 {
		time.Sleep(wait)

		rel, err = membudget.Acquire(n)
	}

	if err != nil {
		ev.err = err

		return nil, nil, false
	}

	raw, err := read()
	if err != nil {
		rel()

		if errors.Is(err, model.ErrNoBlob) {
			ev.c.log.Info("search: message is gone", "event", "search_message_gone", "guid", ev.m.GUID, "uid", ev.m.UID)
			ev.gone = true

			return nil, nil, false
		}

		ev.err = err

		return nil, nil, false
	}

	return raw, rel, true
}
