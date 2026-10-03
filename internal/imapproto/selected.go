package imapproto

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"shardpost/internal/membudget"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// --- unilateral updates ----------------------------------------------------

// poll announces changes made by other sessions or pods. It reads the
// header (one omap read); only if HIGHESTMODSEQ moved does it fetch the
// change log since the session's modseq and the records of the touched
// UIDs. A full reload happens only when the log no longer covers the gap.
//
// Two subtleties keep the session consistent: a pending change (entries
// on a higher shard not yet written) stops the scan at that modseq so the
// UIDs are picked up on the next poll; and when expunges must not be
// announced (FETCH/STORE/SEARCH in progress) the session's modseq is not
// advanced past them, so the next NOOP announces them.
func (c *conn) poll(allowExpunge bool) error {
	if c.mbox == nil {
		return nil
	}

	if gone, err := c.selectedRecord(); gone || err != nil {
		return err
	}

	info, err := c.mu.Info(c.mbox.Name)
	if err != nil {
		return nil // transient; next command retries
	}

	if info.HighestModSeq == c.info.HighestModSeq {
		return nil
	}

	changes, ok, err := c.mu.Changes(c.mbox.ID, c.info.HighestModSeq)
	if err != nil || !ok {
		return c.pollReload(allowExpunge)
	}

	touched, gone, reached := scanChanges(changes, allowExpunge, c.info.HighestModSeq)
	if reached == c.info.HighestModSeq {
		return nil
	}

	var uids []uint32
	for uid := range touched {
		uids = append(uids, uid)
	}

	fresh, err := c.mu.GetMessages(c.mbox.ID, uids)
	if err != nil {
		return c.pollReload(allowExpunge)
	}
	// In a public folder a change may be a mark of this account set by
	// another of its sessions: that is not in the shared record, so the
	// marks of the changed messages are re-read before the fresh
	// records are overlaid.
	if err := c.mergePrivateSeen(uids); err != nil {
		return c.pollReload(allowExpunge)
	}

	c.pollAnnounce(info, fresh, gone, reached)

	return nil
}

// pollAnnounce announces what an incremental poll read. A UID announced
// by a committed change but not readable was either expunged by a
// change beyond "reached" (announced there) or lost by a crashed writer
// (pending expired): skip it, never stall on it.
func (c *conn) pollAnnounce(info *model.MailboxInfo, fresh []model.Meta, gone map[uint32]bool, reached uint64) {
	// 1. expunges
	if len(gone) > 0 {
		c.pollExpunges(gone)
	}
	// 2. flag changes on known messages, 3. new messages. New keywords
	// are announced first: a FETCH FLAGS must not name unknown ones.
	c.announceKeywords(c.info.Keywords, info.Keywords)
	oldLen := len(c.msgs)
	c.pollMerge(fresh)

	if len(c.msgs) != oldLen {
		c.w.untagged("%d EXISTS", len(c.msgs))
		c.announceRecent()
		c.claimRecent(info)
	}

	c.info = info
	c.info.HighestModSeq = reached

	metrics.Commands.WithLabelValues(protoIMAP, "poll", "incremental").Inc()

	c.notifyAppended(oldLen)
}

// scanChanges walks the change log from modseq from on and returns the
// UIDs to (re)read, the expunged UIDs and the highest modseq fully
// applied. It stops at a pending change and, unless allowExpunge, at an
// expunge.
func scanChanges(changes []model.Change, allowExpunge bool, from uint64) (touched, gone map[uint32]bool, reached uint64) {
	touched = map[uint32]bool{}
	gone = map[uint32]bool{}
	reached = from // highest modseq fully applied

	for _, ch := range changes {
		if ch.Pending {
			break
		}

		if ch.Expunged && !allowExpunge {
			break
		}

		for _, uid := range ch.UIDs {
			if ch.Expunged {
				gone[uid] = true
				delete(touched, uid)
			} else if !gone[uid] {
				touched[uid] = true
			}
		}

		reached = ch.ModSeq
	}

	return touched, gone, reached
}

// pollExpunges announces (EXPUNGE or VANISHED) and drops the expunged
// messages of the snapshot.
func (c *conn) pollExpunges(gone map[uint32]bool) {
	var (
		kept    []model.Meta
		vanUIDs []uint32
	)

	for i := len(c.msgs) - 1; i >= 0; i-- {
		if gone[c.msgs[i].UID] {
			if c.qresync {
				vanUIDs = append(vanUIDs, c.msgs[i].UID)
			} else {
				c.w.untagged("%d EXPUNGE", i+1)
			}
		}
	}

	for _, m := range c.msgs {
		if !gone[m.UID] {
			kept = append(kept, m)
		}
	}

	c.msgs = kept
	if len(vanUIDs) > 0 {
		c.w.untagged("VANISHED %s", formatSet(vanUIDs))
	}
}

// pollMerge applies the re-read messages to the snapshot: flag changes of
// known messages are announced, new ones appended in UID order.
func (c *conn) pollMerge(fresh []model.Meta) {
	c.privateOverlay(fresh)

	freshByUID := map[uint32]model.Meta{}
	for _, m := range fresh {
		freshByUID[m.UID] = m
	}

	oldLen := len(c.msgs)

	known := map[uint32]bool{}
	for i := range c.msgs {
		known[c.msgs[i].UID] = true
		if m, ok := freshByUID[c.msgs[i].UID]; ok && m.ModSeq != c.msgs[i].ModSeq {
			c.msgs[i] = m
			c.w.untagged("%d FETCH (UID %d FLAGS (%s)%s)", i+1, m.UID, c.flagsOut(&m), c.modseqItem(m))
		}
	}

	for _, m := range fresh {
		if !known[m.UID] {
			c.msgs = append(c.msgs, m)
		}
	}

	sortByUID(c.msgs[oldLen:])
}

// notifyAppended passes the sequence numbers of the messages appended to
// the snapshot beyond oldLen to NOTIFY.
func (c *conn) notifyAppended(oldLen int) {
	if len(c.msgs) == oldLen {
		return
	}

	var seqs []uint32
	for i := oldLen; i < len(c.msgs); i++ {
		seqs = append(seqs, uint32(i+1))
	}

	c.notifyNewMessages(seqs)
}

func sortByUID(l []model.Meta) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j].UID < l[j-1].UID; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

// pollReload is the full-snapshot fallback. When expunges must not be
// announced, the session's modseq stays where it was so the next allowed
// command announces them.
func (c *conn) pollReload(allowExpunge bool) error {
	old := c.msgs
	oldModSeq := c.info.HighestModSeq

	oldKeywords := c.info.Keywords
	if err := c.reload(); err != nil {
		return nil
	}

	metrics.Commands.WithLabelValues(protoIMAP, "poll", "reload").Inc()

	oldByUID := map[uint32]model.Meta{}
	for _, m := range old {
		oldByUID[m.UID] = m
	}

	cur := map[uint32]bool{}
	for _, m := range c.msgs {
		cur[m.UID] = true
	}

	if !allowExpunge {
		c.reloadDeferExpunges(old, oldByUID, cur, oldModSeq)
	} else {
		c.reloadExpunges(old, cur)
	}

	c.announceKeywords(oldKeywords, c.info.Keywords)

	newSeqs := c.reloadFlagChanges(oldByUID)
	defer c.notifyNewMessages(newSeqs)

	if len(c.msgs) != len(old) {
		c.w.untagged("%d EXISTS", len(c.msgs))
		c.announceRecent()
		c.claimRecent(c.info)
	}

	return nil
}

// reloadDeferExpunges keeps the expunged messages of the old snapshot in
// place (updated where they still exist, new ones appended) and, if any
// is gone, keeps the session's modseq at oldModSeq.
func (c *conn) reloadDeferExpunges(old []model.Meta, oldByUID map[uint32]model.Meta, cur map[uint32]bool, oldModSeq uint64) {
	merged := append([]model.Meta{}, old...)
	for i := range merged {
		if m, ok := metaByUID(c.msgs, merged[i].UID); ok {
			merged[i] = m
		}
	}

	deferred := false

	for _, m := range old {
		if !cur[m.UID] {
			deferred = true
			break
		}
	}

	for _, m := range c.msgs {
		if _, ok := oldByUID[m.UID]; !ok {
			merged = append(merged, m)
		}
	}

	c.msgs = merged
	if deferred {
		c.info.HighestModSeq = oldModSeq
	}
}

// reloadExpunges announces the messages of the old snapshot that are gone.
func (c *conn) reloadExpunges(old []model.Meta, cur map[uint32]bool) {
	var vanished []uint32

	for i := len(old) - 1; i >= 0; i-- {
		if !cur[old[i].UID] {
			if c.qresync {
				vanished = append(vanished, old[i].UID)
			} else {
				c.w.untagged("%d EXPUNGE", i+1)
			}
		}
	}

	if len(vanished) > 0 {
		c.w.untagged("VANISHED %s", formatSet(vanished))
	}
}

// reloadFlagChanges announces the flag changes against the old snapshot
// and returns the sequence numbers of the new messages.
func (c *conn) reloadFlagChanges(oldByUID map[uint32]model.Meta) []uint32 {
	var newSeqs []uint32

	for i, m := range c.msgs {
		if o, ok := oldByUID[m.UID]; ok && o.ModSeq != m.ModSeq {
			c.w.untagged("%d FETCH (UID %d FLAGS (%s)%s)", i+1, m.UID, c.flagsOut(&m), c.modseqItem(m))
		} else if !ok {
			newSeqs = append(newSeqs, uint32(i+1))
		}
	}

	return newSeqs
}

// claimRecent moves the mailbox's recent watermark past the messages
// this session has just seen: like SELECT, the first session to see a
// message takes its \Recent (STATUS then reports RECENT 0 for it).
func (c *conn) claimRecent(info *model.MailboxInfo) {
	if c.examine { // EXAMINE never consumes \Recent (RFC 3501 6.3.2)
		return
	}

	if info.RecentFrom < info.UIDNext {
		_ = c.mu.MarkRecent(c.mbox.ID, info.UIDNext)
	}
}

func (c *conn) modseqItem(m model.Meta) string {
	if c.condstore {
		return fmt.Sprintf(" MODSEQ (%d)", m.ModSeq)
	}

	return ""
}

func metaByUID(l []model.Meta, uid uint32) (model.Meta, bool) {
	for _, m := range l {
		if m.UID == uid {
			return m, true
		}
	}

	return model.Meta{}, false
}

// --- CLOSE / UNSELECT / EXPUNGE ----------------------------------------------

func (c *conn) cmdClose(tag string, expunge bool) error {
	if expunge && !c.readOnly && model.HasRight(c.mrights, 'e') {
		var victims []model.Meta

		for _, m := range c.msgs {
			if model.HasFlag(m.Flags, flagDeleted) {
				victims = append(victims, m)
			}
		}

		if len(victims) > 0 {
			if err := c.mu.Expunge(c.mbox.Name, victims); err != nil {
				c.storeErr(tag, err)
				return nil
			}
		}
	}

	c.unselect()
	c.ok(tag, "", "completed")

	return nil
}

func (c *conn) cmdExpunge(tag string, uid bool) error {
	if !model.HasRight(c.mrights, 'e') {
		c.no(tag, "NOPERM", "Permission denied")
		return nil
	}

	if c.readOnly {
		c.no(tag, "NOPERM", "Mailbox is read-only")
		return nil
	}

	var set *numSet

	if uid {
		s, err := c.r.atom()
		if err != nil {
			return err
		}

		if set, err = parseSet(s); err != nil {
			return err
		}

		set = c.concrete(set)
	}

	var (
		victims []model.Meta
		seqs    []int
	)

	for i, m := range c.msgs {
		if !model.HasFlag(m.Flags, flagDeleted) {
			continue
		}

		if set != nil && !set.contains(m.UID, c.lastUID()) {
			continue
		}

		victims = append(victims, m)
		seqs = append(seqs, i+1)
	}

	if len(victims) > 0 {
		if err := c.mu.Expunge(c.mbox.Name, victims); err != nil {
			c.storeErr(tag, err)
			return nil
		}

		c.announceExpunge(victims, seqs)
	}

	c.ok(tag, "", "EXPUNGE completed")

	return nil
}

// announceExpunge reports removed messages (reverse order so earlier
// sequence numbers stay valid) and drops them from the snapshot.
func (c *conn) announceExpunge(victims []model.Meta, seqs []int) {
	if c.qresync {
		uids := make([]uint32, len(victims))
		for i, v := range victims {
			uids[i] = v.UID
		}

		c.w.untagged("VANISHED %s", formatSet(uids))
	} else {
		for i := len(seqs) - 1; i >= 0; i-- {
			c.w.untagged("%d EXPUNGE", seqs[i])
		}
	}

	gone := map[uint32]bool{}
	for _, v := range victims {
		gone[v.UID] = true
	}

	kept := c.msgs[:0]
	for _, m := range c.msgs {
		if !gone[m.UID] {
			kept = append(kept, m)
		}
	}

	c.msgs = kept
	// Catch up with everything else that happened meanwhile (a message
	// delivered during the command, the copies of a MOVE into the
	// selected mailbox) instead of skipping ahead in the change log:
	// our own expunges are no longer in the snapshot and print nothing.
	_ = c.poll(true)
}

// --- STORE with UNCHANGEDSINCE ----------------------------------------------

// storeArgs are the parsed arguments of a STORE command.
type storeArgs struct {
	set            *numSet
	unchangedSince uint64
	op             string
	silent         bool
	flags          []string
}

// readStoreArgs parses "<set> <operation> <flags>" with its modifier.
func (c *conn) readStoreArgs() (a storeArgs, err error) {
	setStr, err := c.r.atom()
	if err != nil {
		return a, err
	}

	if a.set, err = parseSet(setStr); err != nil {
		return a, err
	}

	unchangedSince, t, err := c.readStoreModifier()
	if err != nil {
		return a, err
	}

	a.unchangedSince = unchangedSince
	a.op = strings.ToUpper(t.str)
	a.silent = strings.HasSuffix(a.op, ".SILENT")
	a.op = strings.TrimSuffix(a.op, ".SILENT")
	a.flags, err = c.readStoreFlags()

	return a, err
}

func (c *conn) cmdStore(tag string, uid bool) error {
	args, err := c.readStoreArgs()
	if err != nil {
		return err
	}

	set, unchangedSince, op, silent, flags := args.set, args.unchangedSince, args.op, args.silent, args.flags

	if c.readOnly {
		c.no(tag, "NOPERM", "Mailbox is read-only")
		return nil
	}
	// RFC 4314: \Seen needs s, \Deleted needs t, everything else w. A
	// replacing STORE also removes flags, so every flag that would change
	// on any addressed message is checked.
	if c.storeDenied(op, flags, set, uid) {
		c.no(tag, "NOPERM", "Permission denied")
		return nil
	}

	apply := storeOperation(op, c.mrights, flags)
	if apply == nil {
		c.bad(tag, "Unknown STORE operation")
		return nil
	}

	seqs, metas := c.resolve(set, uid)

	uids := make([]uint32, len(metas))
	for i, m := range metas {
		uids[i] = m.UID
	}

	// The private \Seen of a public folder is decided first and written
	// last: a message the shared store refuses under UNCHANGEDSINCE
	// keeps its mark, and a message whose mark is too new is not
	// changed shared either.
	plan, err := c.planPrivateSeen(args, metas)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	res, err := c.mu.SetFlags(c.mbox.Name, without(uids, plan.modified), unchangedSince, c.sharedApply(apply))
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	ps, err := c.writePrivateSeen(plan, without(plan.changed, res.Modified))
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.mergePrivate(res, ps, metas)
	c.privateOverlay(res.Updated)

	c.storeResult(tag, res, seqs, metas, silent, unchangedSince != 0, uid)

	return nil
}

// without returns the UIDs of a without those in b, keeping the order.
func without(a, b []uint32) []uint32 {
	if len(b) == 0 {
		return a
	}

	drop := make(map[uint32]bool, len(b))
	for _, uid := range b {
		drop[uid] = true
	}

	out := make([]uint32, 0, len(a))

	for _, uid := range a {
		if !drop[uid] {
			out = append(out, uid)
		}
	}

	return out
}

// storeResult reports the outcome of a STORE: the FETCH responses, the
// IMAPSIEVE flag events and the tagged response (MODIFIED for messages
// that failed UNCHANGEDSINCE).
func (c *conn) storeResult(tag string, res *model.FlagsResult, seqs []int, metas []model.Meta, silent, conditional, uid bool) {
	byUID := c.storeKeywords(res.Updated)

	modified := map[uint32]bool{}
	for _, u := range res.Modified {
		modified[u] = true
	}

	c.storeFetches(seqs, metas, byUID, modified, silent, conditional, uid)

	c.sieveEvents("FLAG", c.selectedTarget(), res.Updated, metas)

	if len(res.Modified) > 0 {
		c.ok(tag, "MODIFIED "+formatSet(modifiedIDs(res.Modified, seqs, metas, modified, uid)), "Conditional STORE failed for some messages")
		return
	}

	c.ok(tag, "", "STORE completed")
}

// readStoreModifier reads the optional (UNCHANGEDSINCE n) modifier and
// returns it with the following token (the operation).
func (c *conn) readStoreModifier() (uint64, token, error) {
	var unchangedSince uint64

	t, err := c.r.next()
	if err != nil {
		return 0, t, err
	}

	if t.kind == tokList { // (UNCHANGEDSINCE n)
		if len(t.list) != 2 || !strings.EqualFold(t.list[0].str, "UNCHANGEDSINCE") {
			return 0, t, fmt.Errorf("%w: bad STORE modifier", errSyntax)
		}

		if _, err := fmt.Sscan(t.list[1].str, &unchangedSince); err != nil {
			return 0, t, fmt.Errorf("%w: bad UNCHANGEDSINCE", errSyntax)
		}

		c.condstore = true
		if t, err = c.r.next(); err != nil {
			return 0, t, err
		}
	}

	return unchangedSince, t, nil
}

// readStoreFlags reads the flag list (or the bare flags) of STORE,
// lower-cased.
func (c *conn) readStoreFlags() ([]string, error) {
	ft, err := c.r.next()
	if err != nil {
		return nil, err
	}

	var flags []string

	if ft.kind == tokList {
		for _, f := range ft.list {
			flags = append(flags, strings.ToLower(f.str))
		}

		return flags, nil
	}

	if !ft.isString() {
		return nil, fmt.Errorf("%w: expected flag list", errSyntax)
	}

	flags = append(flags, strings.ToLower(ft.str))

	for !c.r.eol() {
		f, err := c.r.atom()
		if err != nil {
			return nil, err
		}

		flags = append(flags, strings.ToLower(f))
	}

	return flags, nil
}

// storeDenied reports whether the account lacks a right the STORE needs.
func (c *conn) storeDenied(op string, flags []string, set *numSet, uid bool) bool {
	if c.mrights == model.AllRights {
		return false
	}

	if op == kwFlags {
		// Replacing: every flag added to or removed from a message
		// needs its right; flags that stay are not touched.
		_, metas := c.resolve(set, uid)
		for _, m := range metas {
			if c.replaceDenied(m.Flags, flags) {
				return true
			}
		}

		return false
	}

	for _, f := range flags {
		if !model.HasRight(c.mrights, flagRight(f)) {
			return true
		}
	}

	return false
}

// replaceDenied reports whether replacing the flags old by flags changes
// a flag the account has no right for.
func (c *conn) replaceDenied(old, flags []string) bool {
	for _, f := range old {
		if !model.HasFlag(flags, f) && !model.HasRight(c.mrights, flagRight(f)) {
			return true
		}
	}

	for _, f := range flags {
		if !model.HasFlag(old, f) && !model.HasRight(c.mrights, flagRight(f)) {
			return true
		}
	}

	return false
}

// storeOperation returns the flag update of a STORE operation; nil for an
// unknown operation.
func storeOperation(op, rights string, flags []string) func([]string) []string {
	switch op {
	case kwFlags:
		return replaceFlags(rights, flags)
	case kwAddFlags:
		return addFlags(flags)
	case kwDelFlags:
		return delFlags(flags)
	}

	return nil
}

// storeKeywords records keywords the updated messages introduced and
// announces them; it returns the updated messages by UID.
func (c *conn) storeKeywords(updated []model.Meta) map[uint32]model.Meta {
	byUID := map[uint32]model.Meta{}
	oldKeywords := c.info.Keywords

	for _, m := range updated {
		byUID[m.UID] = m
		for _, f := range m.Flags {
			if f[0] != '\\' && !model.HasFlag(c.info.Keywords, f) {
				c.info.Keywords = append(append([]string{}, c.info.Keywords...), f)
			}
		}
	}

	c.announceKeywords(oldKeywords, c.info.Keywords)

	return byUID
}

// storeFetches updates the snapshot with the stored messages and sends
// their FETCH responses: not for messages that failed UNCHANGEDSINCE, and
// with .SILENT only for changed messages of a conditional STORE.
func (c *conn) storeFetches(seqs []int, metas []model.Meta, byUID map[uint32]model.Meta, modified map[uint32]bool, silent, conditional, uid bool) {
	for i, seq := range seqs {
		m, changed := byUID[metas[i].UID]
		if changed {
			c.msgs[seq-1] = m
		} else {
			m = metas[i]
		}

		if modified[m.UID] {
			continue
		}

		if !silent || (conditional && changed) {
			// RFC 9051 appendix E 21: an IMAP4rev2 session gets the UID
			// in every FETCH it did not ask for by UID.
			uidItem := ""
			if uid || c.rev2 {
				uidItem = fmt.Sprintf("UID %d ", m.UID)
			}

			c.w.untagged("%d FETCH (%sFLAGS (%s)%s)", seq, uidItem, flagList(m.Flags), c.modseqItem(m))
		}
	}
}

// modifiedIDs are the UIDs (UID STORE) or sequence numbers of the
// messages that failed UNCHANGEDSINCE.
func modifiedIDs(mod []uint32, seqs []int, metas []model.Meta, modified map[uint32]bool, uid bool) []uint32 {
	if uid {
		return mod
	}

	var ids []uint32

	for i, m := range metas {
		if modified[m.UID] {
			ids = append(ids, uint32(seqs[i]))
		}
	}

	return ids
}

// flagRight is the RFC 4314 right a flag change needs: \Seen needs s,
// \Deleted needs t, everything else w.
func flagRight(f string) byte {
	switch strings.ToLower(f) {
	case flagSeenLower:
		return 's'
	case flagDeletedLower:
		return 't'
	}

	return 'w'
}

// replaceFlags is STORE FLAGS: with full rights the new list replaces
// the old one. Otherwise the rights were checked against the session's
// snapshot, but the store applies the change to the current flags, so
// flags the client may not touch are preserved as they are there
// (RFC 4314 4): a flag set by the owner meanwhile stays.
func replaceFlags(rights string, flags []string) func([]string) []string {
	if rights == model.AllRights {
		return func(_ []string) []string { return flags }
	}

	return func(old []string) []string {
		out := make([]string, 0, len(flags)+len(old))
		for _, f := range flags {
			if model.HasFlag(old, f) || model.HasRight(rights, flagRight(f)) {
				out = append(out, f)
			}
		}

		for _, f := range old {
			if !model.HasFlag(flags, f) && !model.HasRight(rights, flagRight(f)) {
				out = append(out, f)
			}
		}

		return out
	}
}

func addFlags(add []string) func([]string) []string {
	return func(old []string) []string {
		out := append([]string{}, old...)
		for _, f := range add {
			if !model.HasFlag(out, f) {
				out = append(out, f)
			}
		}

		return out
	}
}

func delFlags(del []string) func([]string) []string {
	return func(old []string) []string {
		var out []string

		for _, f := range old {
			if !model.HasFlag(del, f) {
				out = append(out, f)
			}
		}

		return out
	}
}

// --- COPY / MOVE -----------------------------------------------------------

func (c *conn) cmdCopy(tag string, uid bool) error {
	setStr, err := c.r.atom()
	if err != nil {
		return err
	}

	set, err := parseSet(setStr)
	if err != nil {
		return err
	}

	dest, err := c.mailboxArg()
	if err != nil {
		return err
	}

	dt, info, ok := c.copyDest(tag, dest)
	if !ok {
		return nil
	}

	_, metas := c.resolve(set, uid)
	if len(metas) == 0 {
		c.ok(tag, "", "COPY completed")
		return nil
	}

	clearLearning := c.prepareLearning(kwCopy, dt)
	defer clearLearning()

	var copied []model.Meta
	if dt.u == c.mu {
		copied, err = c.mu.Copy(c.flagsForTarget(dt, metas), dt.name)
	} else {
		copied, err = c.copyAcross(dt, metas)
	}

	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	clearLearning()
	c.sieveEvents(kwCopy, dt, copied, nil)
	c.ok(tag, copyUID(info.UIDValidity, metas, copied), "COPY completed")

	return nil
}

// copyDest resolves the destination of COPY/MOVE, which must exist and
// take right i; ok is false when the client was answered.
func (c *conn) copyDest(tag, dest string) (*target, *model.MailboxInfo, bool) {
	dt, err := c.target(dest)
	if err != nil {
		c.storeErr(tag, err)
		return nil, nil, false
	}

	if c.lockedTarget(tag, dt) {
		return nil, nil, false
	}

	info, err := dt.u.Info(dt.name)
	if err != nil {
		if err == model.ErrNoMailbox {
			c.hideOrTryCreate(tag, dt)
			return nil, nil, false
		}

		c.storeErr(tag, err)

		return nil, nil, false
	}

	if !c.needRight(tag, dt, 'i') {
		return nil, nil, false
	}

	return dt, info, true
}

// flagsForTarget applies the target's flag rights to the copied flags
// (RFC 4314 4) when the copy stays within one namespace but the client
// is not its owner: the store copies the flags it is given.
func (c *conn) flagsForTarget(dt *target, metas []model.Meta) []model.Meta {
	rights, err := c.rightsOf(dt)
	if err != nil || rights == model.AllRights {
		return metas
	}

	out := make([]model.Meta, len(metas))
	for i, m := range metas {
		out[i] = m
		out[i].Flags = allowedFlags(rights, m.Flags)
	}

	return out
}

// copyAcross copies messages into another user's namespace: blobs are
// per namespace, so the data is read and stored again (the flags and
// internal date are kept, \Recent semantics do not apply).
func (c *conn) copyAcross(dt *target, metas []model.Meta) ([]model.Meta, error) {
	rights, err := c.rightsOf(dt)
	if err != nil {
		return nil, err
	}
	// All-or-nothing like COPY within a namespace: a delivery intent
	// covers a crash, and a failure removes what was already stored.
	intent := ""
	if len(metas) > 1 {
		if intent, err = dt.u.BeginDelivery(); err != nil {
			return nil, err
		}
	}

	out := make([]model.Meta, 0, len(metas))

	fail := func(err error) ([]model.Meta, error) {
		if len(out) > 0 {
			if rerr := dt.u.Expunge(dt.name, out); rerr != nil {
				c.log.Error("cross-namespace copy rollback failed, intent kept", "err", rerr)
				return nil, err
			}
		}

		_ = dt.u.EndDelivery(intent)

		return nil, err
	}
	for _, m := range metas {
		rel, err := membudget.Acquire(m.Size)
		if err != nil {
			return fail(err)
		}

		raw, err := c.mu.Read(&m)
		if err != nil {
			rel()
			return fail(err)
		}

		guid := model.NewGUID()
		if err := dt.u.RecordDelivery(intent, dt.name, guid); err != nil {
			rel()
			return fail(err)
		}

		nm, err := dt.u.DeliverGUID(dt.name, guid, raw, allowedFlags(rights, m.Flags), m.InternalDate)

		rel()

		if err != nil {
			return fail(err)
		}

		out = append(out, *nm)
	}

	if err := dt.u.EndDelivery(intent); err != nil {
		return fail(err)
	}

	return out, nil
}

func copyUID(validity uint32, src, dst []model.Meta) string {
	s := make([]uint32, len(src))

	d := make([]uint32, len(dst))
	for i := range src {
		s[i], d[i] = src[i].UID, dst[i].UID
	}

	return fmt.Sprintf("COPYUID %d %s %s", validity, formatSet(s), formatSet(d))
}

func (c *conn) cmdMove(tag string, uid bool) error {
	setStr, err := c.r.atom()
	if err != nil {
		return err
	}

	set, err := parseSet(setStr)
	if err != nil {
		return err
	}

	dest, err := c.mailboxArg()
	if err != nil {
		return err
	}

	if c.readOnly {
		c.no(tag, "NOPERM", "Mailbox is read-only")
		return nil
	}

	if !model.HasRight(c.mrights, 't') || !model.HasRight(c.mrights, 'e') {
		c.no(tag, "NOPERM", "Permission denied")
		return nil
	}

	dt, info, ok := c.copyDest(tag, dest)
	if !ok {
		return nil
	}

	seqs, metas := c.resolve(set, uid)
	if len(metas) == 0 {
		c.ok(tag, "", "MOVE completed")
		return nil
	}

	clearLearning := c.prepareLearning(kwMove, dt)
	defer clearLearning()

	moved, err := c.moveMessages(dt, info, metas)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.w.untagged("OK [%s] Moved", copyUID(info.UIDValidity, metas, moved))
	c.announceExpunge(metas, seqs)
	clearLearning()
	c.sieveEvents(kwMove, dt, moved, nil)
	c.ok(tag, "", "MOVE completed")

	return nil
}

// moveMessages moves metas from the selected mailbox to dt and returns
// the new copies.
func (c *conn) moveMessages(dt *target, info *model.MailboxInfo, metas []model.Meta) ([]model.Meta, error) {
	var (
		moved []model.Meta
		err   error
	)

	sameBox := dt.u == c.mu && info.ID == c.mbox.ID
	if sameBox {
		// Into the selected mailbox itself: new copies, then the
		// originals go (RFC 6851 allows it; imaptest expects success).
		if moved, err = c.mu.Copy(metas, dt.name); err == nil {
			err = c.mu.Expunge(c.mbox.Name, metas)
		}
	} else if dt.u == c.mu {
		moved, err = c.mu.Move(c.mbox.Name, c.flagsForTarget(dt, metas), dt.name)
	} else {
		// across namespaces: copy, then expunge the originals
		if moved, err = c.copyAcross(dt, metas); err == nil {
			err = c.mu.Expunge(c.mbox.Name, metas)
		}
	}

	return moved, err
}

// --- sequence sets ---------------------------------------------------------

func (c *conn) lastUID() uint32 {
	if len(c.msgs) == 0 {
		return 0
	}

	return c.msgs[len(c.msgs)-1].UID
}

// resolve turns a sequence or UID set into snapshot entries.
func (c *conn) resolve(set *numSet, uid bool) (seqs []int, metas []model.Meta) {
	if set.searchRes {
		set = fromUIDs(c.searchRes)
		uid = true
	}

	for i, m := range c.msgs {
		var ok bool
		if uid {
			ok = set.contains(m.UID, c.lastUID())
		} else {
			ok = set.contains(uint32(i+1), uint32(len(c.msgs)))
		}

		if ok {
			seqs = append(seqs, i+1)
			metas = append(metas, m)
		}
	}

	return
}

// announceKeywords sends FLAGS and PERMANENTFLAGS again when the mailbox
// gained keywords (RFC 3501 7.2.6: the server SHOULD do so).
func (c *conn) announceKeywords(oldKeywords, keywords []string) {
	if len(keywords) == len(oldKeywords) {
		return
	}

	flags := append(systemFlags(), keywords...)
	c.w.untagged("FLAGS (%s)", flagList(flags))

	if !c.readOnly {
		c.w.untagged(`OK [PERMANENTFLAGS (%s \*)] Flags permitted`, flagList(flags))
	}
}

// errSelectedGone ends a command (and IDLE) after the session was closed
// because its selected mailbox is being deleted or was deleted; the BYE
// was written already, so the connection loop just ends.
var errSelectedGone = errors.New("selected mailbox gone")

// selectedGone closes the session when err is authoritative: the
// selected mailbox is locked by a mailbox_delete (model.ErrMailboxLocked)
// or no longer registered (model.ErrNoMailbox, confirmed by a registry
// read). Any other error leaves the session alone.
func (c *conn) selectedGone(err error) bool {
	var text, reason string

	switch {
	case errors.Is(err, model.ErrMailboxLocked):
		text, reason = "Selected mailbox is being deleted", metrics.ReasonMailboxLocked
	case errors.Is(err, model.ErrNoMailbox):
		text, reason = "Selected mailbox was deleted", metrics.ReasonMailboxDeleted
	default:
		return false
	}

	metrics.IMAPSessionsClosed.WithLabelValues(reason).Inc()
	c.log.Info("session closed, selected mailbox gone", "event", "session_closed", "mailbox", c.mbox.Name, "reason", reason)
	c.w.untagged("BYE [UNAVAILABLE] %s", text)
	c.state = stateLogout

	return true
}

// selectedRecord looks at the record of the selected mailbox (the lookup
// cache, no extra read) before a poll: a mailbox lock closes the
// session at once; a mailbox that is gone does so only after a fresh
// registry read confirmed it (RFC 2180 3.3). Every other error is
// transient: gone is false and the poll goes on.
func (c *conn) selectedRecord() (bool, error) {
	mb, err := c.mu.Lookup(c.mbox.Name)

	switch {
	case err == nil && mb.Lock.Held("", time.Now()):
		err = model.ErrMailboxLocked
	case errors.Is(err, model.ErrNoMailbox):
		err = model.SelectedMailboxState(c.mu, c.mbox.Name)
	default:
		return false, nil
	}

	if c.selectedGone(err) {
		return true, errSelectedGone
	}

	return false, nil
}

// lockedTarget answers NO [UNAVAILABLE] when a mailbox_delete locks the
// target (its record from the lookup cache); ok is false then.
func (c *conn) lockedTarget(tag string, st *target) bool {
	mb, err := st.u.Lookup(st.name)
	if err != nil || !mb.Lock.Held("", time.Now()) {
		return false
	}

	c.storeErr(tag, model.ErrMailboxLocked)

	return true
}
