package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

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
// So the sequence in the token of a shared account is the visible
// watermark: the sequence of the newest log entry the user can see (one
// hidden() does not filter out). Entries after it are about mailboxes
// the user cannot see, so a /changes from the watermark reports exactly
// what a /changes from the head would, and the token stays where it is
// while the owner works elsewhere. The fingerprint that follows the
// sequence (account.fingerprint) still names the visible set, so a
// change of what the user may see is a new token as before.
//
// Finding the watermark means reading the log back to the last visible
// entry, which may lie any distance behind the head. A cursor records
// how far the log has been read for one user and one namespace: the
// watermark, the sequence read up to, and the visible set both were
// computed for. It is kept in the owner's namespace, as a private server
// METADATA entry of the user — where the user cannot read it: the
// sequence read up to is the owner's counter, which is exactly what the
// watermark keeps from the user. Every reader of the account advances
// the cursor from where it stands, so the log is read once, not once per
// request.
//
// The cursor starts again — at the head, the one place where the token
// may move without a visible change — when the visible set changes (the
// token changes then anyway), when the log no longer reaches back to it,
// and when more than maxCursorPages pages lie between it and the head.

// sharedCursorEntry is the METADATA entry the cursor is kept under.
const sharedCursorEntry = model.ServerManagedPrefix + "sharedcursor"

// maxCursorPages bounds how many pages of the log one advance reads
// before it starts again at the head.
const maxCursorPages = 16

// cursorSaveStride is how far a cursor has to have moved before it is
// written back: a reader that finds nothing new writes nothing, and one
// that read little leaves the next reader a little more to read.
const cursorSaveStride = 256

// sharedCursor is how far the log of a shared account has been read for
// one user.
type sharedCursor struct {
	// Key names the visible set the cursor was computed for
	// (account.visibilityKey).
	Key string `json:"k"`
	// LV is the visible watermark, Scanned the sequence the log has been
	// read up to.
	LV      uint64 `json:"v"`
	Scanned uint64 `json:"s"`
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
// type and restarted when the cursor had to start again. reload, when not nil, is
// called before a page that holds a change of a mailbox itself is
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

// restartCursor starts a cursor again at the head of the log.
func (a *account) restartCursor(c *sharedCursor, key string) (map[string]bool, bool, error) {
	head, err := a.user.AccountState()
	if err != nil {
		return nil, false, err
	}

	*c = sharedCursor{Key: key, LV: head, Scanned: head}

	return allMailTypes(), true, nil
}

// loadCursor reads the user's cursor for the account; raw is what the
// entry held, for the swap that writes it back.
func (a *account) loadCursor() (sharedCursor, string, error) {
	md, err := a.user.GetMetadata("", a.login)
	if err != nil {
		return sharedCursor{}, "", err
	}

	raw := md[sharedCursorEntry]

	var c sharedCursor
	if raw != "" && json.Unmarshal([]byte(raw), &c) != nil {
		c = sharedCursor{}
	}

	return c, raw, nil
}

// saveCursor writes the cursor back when it moved enough, and only
// over what it was read from: of two readers that raced, the first one's
// cursor is kept. It reports whether the stored cursor is c now.
func (a *account) saveCursor(c sharedCursor, was sharedCursor, raw string) bool {
	if c.Key == was.Key && c.LV == was.LV && c.Scanned >= was.Scanned && c.Scanned-was.Scanned < cursorSaveStride {
		return true
	}

	b, _ := json.Marshal(c)
	next := string(b)

	swapped, err := a.user.SwapMetadata("", a.login, sharedCursorEntry, raw, &next)

	return err == nil && swapped
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
// start again at the head, they then answer the same token.
func (a *account) storedCursor() (sharedCursor, error) {
	for attempt := 0; ; attempt++ {
		was, raw, err := a.loadCursor()
		if err != nil {
			return sharedCursor{}, err
		}

		c := was
		if _, _, err := advanceCursor(a, &c, nil); err != nil {
			return sharedCursor{}, err
		}

		if a.saveCursor(c, was, raw) || attempt > 0 {
			return c, nil
		}
	}
}

// changesSeq is the sequence of the newState of a /changes page of the
// account. For the personal account it is the last entry read. For a
// shared account it is the last entry the user can see, so that a client
// that is up to date holds the token a /get answers; a page that shows
// nothing is the page's end while there is more to read, and the visible
// watermark once the log has been read to its end.
func (a *account) changesSeq(since uint64, entries []model.AccountChange, full bool) (uint64, error) {
	if len(entries) == 0 {
		return since, nil
	}

	if a.personal {
		return entries[len(entries)-1].Seq, nil
	}

	for i := len(entries) - 1; i >= 0; i-- {
		if !hidden(a, &entries[i]) {
			return entries[i].Seq, nil
		}
	}

	if full {
		return entries[len(entries)-1].Seq, nil
	}

	lv, _, err := a.visibleSeq()

	return lv, err
}
