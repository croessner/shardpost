package jmap

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"shardpost/internal/model"
)

// The state of a shared account as the user sees it.
//
// The account change log of a namespace counts every change of it, and a
// shared account shows only part of the namespace. A state token built
// from the counter itself would move whenever the owner did anything —
// in a mailbox the user may not see as well — and push would turn that
// into an event: the user would learn when, and roughly how much, the
// owner works in mailboxes that are none of its business.
//
// So the sequence behind the token of a shared account is the visible
// watermark: the sequence of the newest log entry the user can see (one
// hidden() does not filter out). Entries after it are about mailboxes
// the user cannot see, so a /changes from the watermark reports exactly
// what a /changes from the head would, and the token stays where it is
// while the owner works elsewhere. The fingerprint that follows the
// sequence (account.fingerprint) still names the visible set, so a
// change of what the user may see is a new token as before.
//
// The sequence itself is the owner's counter, and two of them would tell
// how many invisible changes lie between two visible ones. So the token
// does not carry it in the clear: it is enciphered with a key of the
// namespace (sealSeq), a permutation of the 64-bit numbers that the user
// can neither read nor order.
//
// Finding the watermark means reading the log back to the last visible
// entry, which may lie any distance behind the head. A cursor records
// how far the log has been read for one visible set of one namespace: the
// watermark, the sequence read up to, and when it was last written. It is
// kept in the owner's namespace, as a private server METADATA entry of
// the server's own accessor, where no user can read it, and it is keyed
// by the visible set, not by the user: users who see the same mailboxes
// with the same read rights share one cursor, since the watermark is a
// function of the log and the set alone. Every reader advances the cursor
// from where it stands, so the log is read once, not once per request.
//
// The cursor starts again when there is none for the set yet, when the
// log no longer reaches back to it, and when more than maxCursorPages
// pages lie between it and the head. It then reads the last page of the
// log and takes its newest visible entry as the watermark — the state
// stays when that is where it was — and the head when the page shows
// nothing, the one case where the state moves without a visible change.

// Where the server keeps its entries in a namespace's METADATA: under an
// accessor no account can be (names starting with "~" are reserved), so
// neither the owner nor anyone else reads them over IMAP.
const (
	serverAccessor     = "~jmap"
	stateKeyEntry      = model.ServerManagedPrefix + "statekey"
	sharedCursorPrefix = model.ServerManagedPrefix + "sharedcursor/"
)

// maxCursorPages bounds how many pages of the log one advance, and one
// /changes, reads.
const maxCursorPages = 16

// cursorSaveStride is how far a cursor has to have moved before it is
// written back: a reader that finds nothing new writes nothing, and one
// that read little leaves the next reader a little more to read.
const cursorSaveStride = 256

// maxCursors bounds the cursors one namespace keeps; the one written
// longest ago goes first. A reader whose cursor went starts again.
const maxCursors = 256

// sharedCursor is how far the log of a shared account has been read for
// one visible set.
type sharedCursor struct {
	// Key names the visible set the cursor was computed for
	// (account.visibilityKey).
	Key string `json:"k"`
	// LV is the visible watermark, Scanned the sequence the log has been
	// read up to.
	LV      uint64 `json:"v"`
	Scanned uint64 `json:"s"`
	// T is when it was written (Unix seconds), for pruning.
	T int64 `json:"t,omitempty"`
}

// visibilityKey names what decides which log entries the user sees: the
// visible mailboxes, and which of them it may read. It is not the
// fingerprint of either token — a subscription or a right other than r
// changes neither what the log shows nor the watermark.
func (a *account) visibilityKey() string {
	ids := make([]string, 0, len(a.boxes))
	for id := range a.boxes {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	h := sha256.New()

	for _, id := range ids {
		r := "-"
		if model.HasRight(a.boxes[id].rights, 'r') {
			r = "r"
		}

		_, _ = h.Write([]byte(id + ":" + r + "\n"))
	}

	return hex.EncodeToString(h.Sum(nil)[:8])
}

// allMailTypes are the data types a cursor that starts again reports:
// it cannot tell what changed.
func allMailTypes() map[string]bool {
	out := map[string]bool{}
	for _, t := range pushTypes {
		out[t] = true
	}

	return out
}

// advanceCursor reads the log of a shared account from where the cursor
// stands to its end and moves the watermark to the last entry the user
// can see. It returns the data types those entries changed, or every
// type and restarted when the cursor had to start again. reload, when not
// nil, is called before a page that holds a change of a mailbox itself is
// classified, so that a follower whose account was loaded earlier (push)
// judges the page by what the user sees now.
func advanceCursor(a *account, c *sharedCursor, reload func() error) (types map[string]bool, restarted bool, err error) {
	key := a.visibilityKey()
	if c.Key != key {
		return a.restartCursor(c, key)
	}

	types = map[string]bool{}

	for pages := 0; ; pages++ {
		if pages == maxCursorPages {
			return a.restartCursor(c, key)
		}

		entries, ok, err := a.user.AccountChanges(c.Scanned, pushPage)
		if err != nil {
			return nil, false, err
		}

		if !ok {
			return a.restartCursor(c, key)
		}

		if reload != nil && touchesMailbox(entries) {
			if err := reload(); err != nil {
				return nil, false, err
			}

			if k := a.visibilityKey(); k != key {
				return a.restartCursor(c, k)
			}
		}

		c.classify(a, entries, types)

		if len(entries) < pushPage {
			return types, false, nil
		}
	}
}

// classify moves the cursor over a page of the log: the watermark to
// each entry the user can see, and the types those entries changed into
// types.
func (c *sharedCursor) classify(a *account, entries []model.AccountChange, types map[string]bool) {
	for i := range entries {
		c.Scanned = entries[i].Seq

		if hidden(a, &entries[i]) {
			continue
		}

		c.LV = entries[i].Seq

		for t := range typesOf(entries[i : i+1]) {
			types[t] = true
		}
	}
}

// touchesMailbox reports whether a page holds an entry that may change
// what the user sees: a change of a mailbox itself (a rename, a
// deletion, an ACL) or one that does not say what it changed.
func touchesMailbox(entries []model.AccountChange) bool {
	for i := range entries {
		if entries[i].Kind != model.KindEmail || entries[i].Partial {
			return true
		}
	}

	return false
}

// restartCursor starts a cursor again at the end of the log: the newest
// visible entry of the last page is the watermark, the head when the
// page shows none.
func (a *account) restartCursor(c *sharedCursor, key string) (map[string]bool, bool, error) {
	head, err := a.user.AccountState()
	if err != nil {
		return nil, false, err
	}

	*c = sharedCursor{Key: key, LV: head, Scanned: head}

	from := uint64(0)
	if head > pushPage {
		from = head - pushPage
	}

	entries, ok, err := a.user.AccountChanges(from, pushPage)
	if err != nil {
		return nil, false, err
	}

	if ok && len(entries) > 0 {
		visible := false

		for i := range entries {
			if !hidden(a, &entries[i]) {
				c.LV, visible = entries[i].Seq, true
			}
		}

		c.Scanned = max(head, entries[len(entries)-1].Seq)
		if !visible {
			c.LV = c.Scanned
		}
	}

	return allMailTypes(), true, nil
}

// cursorEntry is the METADATA entry of the cursor of a visible set.
func cursorEntry(key string) string { return sharedCursorPrefix + key }

// loadCursor reads the stored cursor of the account's visible set; raw
// is what the entry held, for the swap that writes it back, and md all
// the server's entries of the namespace, for pruning.
func (a *account) loadCursor() (c sharedCursor, raw string, md map[string]string, err error) {
	md, err = a.user.GetMetadata("", serverAccessor)
	if err != nil {
		return sharedCursor{}, "", nil, err
	}

	raw = md[cursorEntry(a.visibilityKey())]
	if raw != "" && json.Unmarshal([]byte(raw), &c) != nil {
		c = sharedCursor{}
	}

	return c, raw, md, nil
}

// saveCursor writes the cursor back when it moved enough, and only
// over what it was read from: of two readers that raced, the first one's
// cursor is kept. It reports whether the stored cursor is c now. A new
// cursor makes room first when the namespace holds maxCursors.
func (a *account) saveCursor(c, was sharedCursor, raw string, md map[string]string) bool {
	if raw != "" && c.Key == was.Key && c.LV == was.LV && c.Scanned >= was.Scanned && c.Scanned-was.Scanned < cursorSaveStride {
		return true
	}

	if raw == "" {
		a.pruneCursors(md)
	}

	c.T = time.Now().Unix()
	b, _ := json.Marshal(c)
	next := string(b)

	swapped, err := a.user.SwapMetadata("", serverAccessor, cursorEntry(c.Key), raw, &next)

	return err == nil && swapped
}

// pruneCursors removes the cursors written longest ago while the
// namespace holds maxCursors or more.
func (a *account) pruneCursors(md map[string]string) {
	type aged struct {
		entry string
		t     int64
	}

	var all []aged

	for entry, raw := range md {
		if !strings.HasPrefix(entry, sharedCursorPrefix) {
			continue
		}

		var c sharedCursor

		_ = json.Unmarshal([]byte(raw), &c)
		all = append(all, aged{entry, c.T})
	}

	if len(all) < maxCursors {
		return
	}

	sort.Slice(all, func(i, j int) bool { return all[i].t < all[j].t })

	drop := map[string]*string{}
	for _, x := range all[:len(all)-maxCursors+1] {
		drop[x.entry] = nil
	}

	if err := a.user.SetMetadata("", serverAccessor, drop); err != nil {
		a.srv.logOnce("cursorprune:"+a.owner, "jmap: shared cursors not pruned", "shared", a.owner, "err", err)
	}
}

// visibleSeq is where the log of the account stands as the user sees
// it: the sequence its state tokens carry (the head for the personal
// account, the visible watermark for a shared one), and the sequence the
// log was read up to, which a locator is brought up to.
func (a *account) visibleSeq() (lv, scanned uint64, err error) {
	if a.personal {
		seq, err := a.user.AccountState()

		return seq, seq, err
	}

	c, err := a.storedCursor()

	return c.LV, c.Scanned, err
}

// storedCursor advances the stored cursor of a shared account to the end
// of the log and stores it. A reader whose write lost to another one's
// reads the stored cursor again and advances that: when both had to
// start again, they then answer the same token.
func (a *account) storedCursor() (sharedCursor, error) {
	for attempt := 0; ; attempt++ {
		was, raw, md, err := a.loadCursor()
		if err != nil {
			return sharedCursor{}, err
		}

		c := was
		if _, _, err := advanceCursor(a, &c, nil); err != nil {
			return sharedCursor{}, err
		}

		if a.saveCursor(c, was, raw, md) || attempt > 0 {
			return c, nil
		}
	}
}

// changesWindow reads the log entries a /changes from since reports: for
// the personal account the next limit entries. For a shared account the
// next limit entries the user can see, reading past invisible ones —
// maxCursorPages pages at most — so that neither the page nor the new
// state tells how many there were: more is set only when visible entries
// were left for the next call, and next is the last visible entry
// returned, or the watermark when there are no more. A run of invisible
// entries longer than the bound, with no visible one before it, is
// cannotCalculateChanges.
func (a *account) changesWindow(since uint64, limit int) (entries []model.AccountChange, next uint64, more bool, merr *methodError) {
	if a.personal {
		entries, ok, err := a.user.AccountChanges(since, limit)
		if err != nil {
			return nil, 0, false, &methodError{Type: errServerFail, Description: err.Error()}
		}

		if !ok {
			return nil, 0, false, &methodError{Type: errCannotCalculateChanges, Description: descLogTrimmed}
		}

		next = since
		if len(entries) > 0 {
			next = entries[len(entries)-1].Seq
		}

		return entries, next, len(entries) == limit, nil
	}

	lv, scanned, err := a.visibleSeq()
	if err != nil {
		return nil, 0, false, &methodError{Type: errServerFail, Description: err.Error()}
	}

	switch {
	case since > scanned:
		return nil, 0, false, &methodError{Type: errCannotCalculateChanges, Description: descUnknownState}
	case since >= lv:
		// Nothing the user sees lies beyond since.
		return nil, lv, false, nil
	}

	return a.visibleEntries(since, lv, limit)
}

// visibleEntries is changesWindow for a shared account whose watermark
// lies beyond since.
func (a *account) visibleEntries(since, lv uint64, limit int) ([]model.AccountChange, uint64, bool, *methodError) {
	var out []model.AccountChange

	pos := since

	for range maxCursorPages {
		page, ok, err := a.user.AccountChanges(pos, pushPage)
		if err != nil {
			return nil, 0, false, &methodError{Type: errServerFail, Description: err.Error()}
		}

		if !ok {
			return nil, 0, false, &methodError{Type: errCannotCalculateChanges, Description: descLogTrimmed}
		}

		for i := range page {
			if hidden(a, &page[i]) {
				continue
			}

			out = append(out, page[i])

			switch {
			case page[i].Seq >= lv:
				return out, page[i].Seq, false, nil
			case len(out) == limit:
				return out, page[i].Seq, true, nil
			}
		}

		if len(page) < pushPage || page[len(page)-1].Seq >= lv {
			// The rest up to the watermark is invisible.
			return out, lv, false, nil
		}

		pos = page[len(page)-1].Seq
	}

	if len(out) > 0 {
		return out, out[len(out)-1].Seq, true, nil
	}

	return nil, 0, false, &methodError{Type: errCannotCalculateChanges,
		Description: "the changes lie too far back; resynchronise"}
}

// The state key of a namespace: 32 random bytes, created by the first
// server that needs it and kept as a server METADATA entry of the
// namespace; each process caches it.

// stateKeyOf returns the state key of a namespace, creating it if there
// is none yet.
func (s *Server) stateKeyOf(u model.User, ns string) ([]byte, error) {
	if k, ok := s.stateKeys.Load(ns); ok {
		return k.([]byte), nil
	}

	for range 3 {
		md, err := u.GetMetadata("", serverAccessor)
		if err != nil {
			return nil, err
		}

		raw := md[stateKeyEntry]
		if k, err := hex.DecodeString(raw); err == nil && len(k) == 32 {
			s.stateKeys.Store(ns, k)

			return k, nil
		}

		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil, err
		}

		next := hex.EncodeToString(k)

		swapped, err := u.SwapMetadata("", serverAccessor, stateKeyEntry, raw, &next)
		if err != nil {
			return nil, err
		}

		if swapped {
			s.stateKeys.Store(ns, k)

			return k, nil
		}
	}

	return nil, errors.New("the state key could not be written")
}

// sealedPrefix marks an enciphered sequence in a state token.
const sealedPrefix = "x"

// sealSeq enciphers a log sequence for a state token: a four-round
// Feistel network over the two 32-bit halves with HMAC-SHA256 as its
// round function, a keyed permutation of the 64-bit numbers.
func sealSeq(key []byte, seq uint64) string {
	l, r := uint32(seq>>32), uint32(seq)
	for i := range byte(4) {
		l, r = r, l^feistel(key, i, r)
	}

	var b [8]byte

	binary.BigEndian.PutUint64(b[:], uint64(l)<<32|uint64(r))

	return sealedPrefix + hex.EncodeToString(b[:])
}

// openSeq reverses sealSeq.
func openSeq(key []byte, s string) (uint64, bool) {
	raw, ok := strings.CutPrefix(s, sealedPrefix)
	if !ok || len(raw) != 16 {
		return 0, false
	}

	b, err := hex.DecodeString(raw)
	if err != nil {
		return 0, false
	}

	v := binary.BigEndian.Uint64(b)
	l, r := uint32(v>>32), uint32(v)

	for i := byte(4); i > 0; i-- {
		l, r = r^feistel(key, i-1, l), l
	}

	return uint64(l)<<32 | uint64(r), true
}

// feistel is the round function.
func feistel(key []byte, round byte, half uint32) uint32 {
	m := hmac.New(sha256.New, key)

	var b [5]byte

	b[0] = round
	binary.BigEndian.PutUint32(b[1:], half)
	_, _ = m.Write(b[:])

	return binary.BigEndian.Uint32(m.Sum(nil))
}
