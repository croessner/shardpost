// Package migrate copies an account from an IMAP/ManageSieve source into
// ShardPost through the admin import API, keeping UIDVALIDITY, UIDs,
// flags, keywords, dates and MODSEQs, so clients only resynchronise
// incrementally after the switch (roadmap 8e). Every run is a delta
// against what the target already holds: the first run copies
// everything, later runs add new messages, apply flag changes and
// expunges, and a run under a director hold makes the target exact.
package migrate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// Options configure a run.
type Options struct {
	// Account is the canonical ShardPost account.
	Account string
	// SourceUser is the login name at the source ("" = Account).
	SourceUser string
	// MasterUser logs in as SourceUser with the master password
	// (UserFormat); "" logs in as SourceUser with Password.
	MasterUser string
	// UserFormat builds the master login ("{user}", "{master}");
	// default "{user}*{master}".
	UserFormat string
	Password   string

	IMAP Endpoint
	// Sieve, if set, copies the Sieve scripts over ManageSieve.
	Sieve *Endpoint

	// Session resumes an import session ("" = a new one).
	Session string
	// Prune deletes target mailboxes the source no longer has.
	Prune bool
	// ACL copies the explicit ACL entries of every mailbox.
	ACL bool
	// Reimport replaces a target mailbox whose UIDVALIDITY no longer
	// matches the source (the source recreated it) instead of failing;
	// its messages are removed and imported again.
	Reimport bool
	// POP3UIDLFormat, if set, stores a Dovecot-compatible POP3 UIDL for
	// INBOX messages (see parseUIDLFormat).
	POP3UIDLFormat string

	// BatchMessages and BatchBytes bound one import request.
	BatchMessages int
	BatchBytes    int64

	Log *slog.Logger
}

// Report is the result of a run.
type Report struct {
	Account   string          `json:"account"`
	Session   string          `json:"session"`
	Mailboxes []MailboxReport `json:"mailboxes"`
	Pruned    []string        `json:"pruned,omitempty"`
	Sieve     string          `json:"sieve,omitempty"`
	Warnings  []string        `json:"warnings,omitempty"`
	Failed    int             `json:"failed"`
	Duration  string          `json:"duration"`
}

// MailboxReport is the result for one mailbox.
type MailboxReport struct {
	Name          string `json:"name"`
	ID            string `json:"id,omitempty"`
	UIDValidity   uint32 `json:"uidvalidity"`
	UIDNext       uint32 `json:"uid_next"`
	HighestModSeq uint64 `json:"highest_modseq"`
	Messages      int    `json:"messages"`
	Imported      int    `json:"imported"`
	Existing      int    `json:"existing"`
	Shared        int    `json:"shared"`
	Bytes         int64  `json:"bytes"`
	FlagChanges   int    `json:"flag_changes"`
	Expunged      int    `json:"expunged"`
	Skipped       int    `json:"skipped,omitempty"` // messages the target refused as too large
	Verified      bool   `json:"verified"`
	Error         string `json:"error,omitempty"`
}

// ErrPartial means the run finished but some parts failed (see Report).
var ErrPartial = errors.New("migration partially failed")

// changeChunk bounds one changes request.
const changeChunk = 5000

// Run migrates the account once (a full copy or a delta).
func Run(ctx context.Context, o Options, tgt Target) (*Report, error) {
	start := time.Now()

	if err := o.defaults(); err != nil {
		return nil, err
	}

	uidl, err := parseUIDLFormat(o.POP3UIDLFormat)
	if err != nil {
		return nil, err
	}

	session, err := tgt.Open(ctx, o.Account, o.Session)
	if err != nil {
		return nil, fmt.Errorf("open import session: %w", err)
	}

	rep := &Report{Account: o.Account, Session: session}

	defer func() {
		if err := tgt.Close(context.WithoutCancel(ctx)); err != nil {
			o.Log.Warn("import session not released (it expires)", "err", err)
		}
	}()

	src, err := dialIMAP(ctx, o.IMAP)
	if err != nil {
		return rep, err
	}
	defer src.Close()

	r := &run{o: o, src: src, tgt: tgt, rep: rep, uidl: uidl}
	if err := r.all(ctx); err != nil {
		return rep, err
	}

	rep.Duration = time.Since(start).Round(time.Second).String()

	if rep.Failed > 0 {
		return rep, ErrPartial
	}

	return rep, nil
}

// all logs in and migrates every mailbox, then prunes and copies Sieve.
func (r *run) all(ctx context.Context) error {
	if err := r.src.login(r.o.login(), r.o.Password); err != nil {
		return err
	}

	if !r.src.caps["CONDSTORE"] {
		r.rep.Warnings = append(r.rep.Warnings, "source has no CONDSTORE: modseqs are assigned by the target")
	}

	mailboxes, err := r.src.listMailboxes()
	if err != nil {
		return fmt.Errorf("list source mailboxes: %w", err)
	}

	for _, p := range r.src.skippedNamespaces {
		r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("personal namespace %q is not imported", p))
	}

	keep, err := r.mailboxes(ctx, mailboxes)
	if err != nil {
		return err
	}

	if r.o.Prune && r.rep.Failed == 0 {
		r.prune(ctx, keep)
	}

	if r.o.Sieve != nil {
		r.sieve(ctx)
	}

	return nil
}

// mailboxes migrates the selectable mailboxes and returns the target ids
// that correspond to source mailboxes.
func (r *run) mailboxes(ctx context.Context, mailboxes []srcMailbox) (map[string]bool, error) {
	keep := map[string]bool{}

	for _, mb := range mailboxes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		mr := MailboxReport{Name: mb.Name}

		switch {
		case !mb.Selectable:
			continue
		case mb.Err != nil:
			mr.Error = mb.Err.Error()
		default:
			mr = r.mailbox(ctx, mb)
			keep[mr.ID] = mr.ID != ""
		}

		if mr.Error != "" {
			r.rep.Failed++
		}

		r.rep.Mailboxes = append(r.rep.Mailboxes, mr)
	}

	return keep, nil
}

// defaults fills in and checks the options.
func (o *Options) defaults() error {
	if o.Account == "" || o.IMAP.Addr == "" {
		return errors.New("account and source address are required")
	}

	o.SourceUser = cmp.Or(o.SourceUser, o.Account)
	o.UserFormat = cmp.Or(o.UserFormat, "{user}*{master}")
	o.BatchMessages = cmp.Or(max(o.BatchMessages, 0), 200)
	o.BatchBytes = cmp.Or(max(o.BatchBytes, 0), 32<<20)
	o.IMAP.Timeout = cmp.Or(max(o.IMAP.Timeout, 0), 2*time.Minute)

	if o.Sieve != nil {
		o.Sieve.Timeout = cmp.Or(max(o.Sieve.Timeout, 0), time.Minute)
	}

	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	for _, mode := range []string{o.IMAP.TLS, sieveTLS(o.Sieve)} {
		if mode != TLSImplicit && mode != TLSStartTLS && mode != TLSNone {
			return fmt.Errorf("tls mode %q (implicit, starttls or none)", mode)
		}
	}

	return nil
}

func sieveTLS(e *Endpoint) string {
	if e == nil {
		return TLSNone
	}

	return e.TLS
}

// login is the source login name.
func (o *Options) login() string {
	if o.MasterUser == "" {
		return o.SourceUser
	}

	return strings.NewReplacer("{user}", o.SourceUser, "{master}", o.MasterUser).Replace(o.UserFormat)
}

// run is the state of one migration run.
type run struct {
	o    Options
	src  *imapClient
	tgt  Target
	rep  *Report
	uidl uidlFormat
}

// mailbox migrates one mailbox; errors end up in the report.
func (r *run) mailbox(ctx context.Context, mb srcMailbox) MailboxReport {
	mr := MailboxReport{Name: mb.Name}
	log := r.o.Log.With("mailbox", mb.Name)

	if err := r.syncMailbox(ctx, mb, &mr, log); err != nil {
		mr.Error = err.Error()
		log.Error("mailbox failed", "err", err)

		return mr
	}

	log.Info("mailbox done", "messages", mr.Messages, "imported", mr.Imported, "existing", mr.Existing, "shared", mr.Shared,
		"flag_changes", mr.FlagChanges, "expunged", mr.Expunged, "uidvalidity", mr.UIDValidity, "uid_next", mr.UIDNext,
		"highest_modseq", mr.HighestModSeq)

	return mr
}

func (r *run) syncMailbox(ctx context.Context, mb srcMailbox, mr *MailboxReport, log *slog.Logger) error {
	st, id, srcMsgs, err := r.readSource(ctx, mb, mr)
	if err != nil {
		return err
	}

	have, err := inventory(ctx, r.tgt, id)
	if err != nil {
		return fmt.Errorf("target inventory: %w", err)
	}

	plan := diff(srcMsgs, have)
	if len(plan.conflicts) > 0 {
		if !r.o.Reimport {
			return fmt.Errorf("uid %d: the target holds a message of another size; run with -reimport", plan.conflicts[0].UID)
		}

		// Remove the wrong messages and store the source ones at the
		// same UIDs (the mailbox is not live here yet).
		r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%s: %d messages differ in size, imported again", mb.Name, len(plan.conflicts)))
		plan.reimport()
	}

	if err := r.applyChanges(ctx, id, plan, mr); err != nil {
		return err
	}

	if err := r.copyMessages(ctx, mb, st, id, plan.missing, mr, log); err != nil {
		return err
	}

	final, err := r.tgt.Finalize(ctx, id, st.UIDNext, st.HighestModSeq)
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}

	if final.Uidvalidity != int64(st.UIDValidity) || final.UidNext < int64(st.UIDNext) || final.HighestModseq < int64(st.HighestModSeq) {
		return fmt.Errorf("target state %d/%d/%d does not cover the source %d/%d/%d", final.Uidvalidity, final.UidNext, final.HighestModseq,
			st.UIDValidity, st.UIDNext, st.HighestModSeq)
	}

	if r.o.ACL {
		r.acl(ctx, mb, id)
	}

	return r.verify(ctx, id, srcMsgs, mr)
}

// readSource reads the source state and messages of a mailbox and
// creates (or adopts) the target mailbox; it returns the target id.
func (r *run) readSource(ctx context.Context, mb srcMailbox, mr *MailboxReport) (*srcStatus, string, []srcMessage, error) {
	st, err := r.src.status(mb.Raw)
	if err != nil {
		return nil, "", nil, fmt.Errorf("source status: %w", err)
	}

	mr.UIDValidity, mr.UIDNext, mr.HighestModSeq = st.UIDValidity, st.UIDNext, st.HighestModSeq

	id := strings.ToLower(st.GUID)
	if !model.ValidGUID(id) {
		id = ""
	}

	state, err := r.putMailbox(ctx, adminapi.ImportMailboxSpec{Name: mb.Name, Uidvalidity: int64(st.UIDValidity), Id: optional(id),
		SpecialUse: &mb.SpecialUse, Subscribed: &mb.Subscribed})
	if err != nil {
		return nil, "", nil, fmt.Errorf("target mailbox: %w", err)
	}

	mr.ID = state.Id

	if err := r.src.examine(mb.Raw); err != nil {
		return nil, "", nil, fmt.Errorf("source examine: %w", err)
	}

	// An empty mailbox may answer "UID FETCH 1:*" with BAD.
	srcMsgs, err := r.src.fetchMeta()
	if err != nil && (st.Messages != 0 || !isBAD(err)) {
		return nil, "", nil, fmt.Errorf("source fetch: %w", err)
	}

	mr.Messages = len(srcMsgs)

	return st, state.Id, srcMsgs, nil
}

// putMailbox creates or adopts the target mailbox; with Reimport a
// UIDVALIDITY conflict replaces it.
func (r *run) putMailbox(ctx context.Context, spec adminapi.ImportMailboxSpec) (*adminapi.ImportMailboxState, error) {
	state, err := r.tgt.PutMailbox(ctx, spec)

	var apiErr *APIError
	if !r.o.Reimport || !errors.As(err, &apiErr) || apiErr.Code != string(adminapi.Conflict) {
		return state, err
	}

	r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%s: replaced for a reimport (%s)", spec.Name, apiErr.Msg))
	replace := true
	spec.Replace = &replace

	return r.tgt.PutMailbox(ctx, spec)
}

// plan is what a mailbox needs to match the source.
type plan struct {
	missing   []srcMessage
	flags     []adminapi.ImportFlagChange
	expunge   []int64
	conflicts []srcMessage // same UID, other size
}

// reimport turns the conflicts into expunges plus imports.
func (p *plan) reimport() {
	for _, m := range p.conflicts {
		p.expunge = append(p.expunge, int64(m.UID))
		p.missing = append(p.missing, m)
	}

	sort.Slice(p.expunge, func(i, j int) bool { return p.expunge[i] < p.expunge[j] })
	sort.Slice(p.missing, func(i, j int) bool { return p.missing[i].UID < p.missing[j].UID })
	p.conflicts = nil
}

// diff compares the source messages with the target inventory.
func diff(src []srcMessage, have map[uint32]adminapi.ImportInventoryEntry) *plan {
	p := &plan{}
	seen := make(map[uint32]bool, len(src))

	for _, m := range src {
		seen[m.UID] = true

		t, ok := have[m.UID]
		if !ok {
			p.missing = append(p.missing, m)
			continue
		}

		if m.Size != 0 && t.Size != m.Size {
			p.conflicts = append(p.conflicts, m)
			continue
		}

		flags := storableFlags(m.Flags)
		if !sameFlags(flags, t.Flags) || (m.ModSeq != 0 && uint64(t.Modseq) != m.ModSeq) {
			p.flags = append(p.flags, adminapi.ImportFlagChange{Uid: int64(m.UID), Modseq: int64(m.ModSeq), Flags: flags})
		}
	}

	for uid := range have {
		if !seen[uid] {
			p.expunge = append(p.expunge, int64(uid))
		}
	}

	sort.Slice(p.expunge, func(i, j int) bool { return p.expunge[i] < p.expunge[j] })

	return p
}

// applyChanges sends flag changes and expunges in chunks.
func (r *run) applyChanges(ctx context.Context, id string, p *plan, mr *MailboxReport) error {
	for len(p.flags) > 0 || len(p.expunge) > 0 {
		f := p.flags[:min(len(p.flags), changeChunk)]
		x := p.expunge[:min(len(p.expunge), changeChunk)]

		if _, err := r.tgt.Changes(ctx, id, f, x); err != nil {
			return fmt.Errorf("changes: %w", err)
		}

		mr.FlagChanges += len(f)
		mr.Expunged += len(x)
		p.flags, p.expunge = p.flags[len(f):], p.expunge[len(x):]
	}

	return nil
}

// copyMessages fetches the missing messages in batches and imports them.
func (r *run) copyMessages(ctx context.Context, mb srcMailbox, st *srcStatus, id string, missing []srcMessage, mr *MailboxReport, log *slog.Logger) error {
	for len(missing) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}

		n := r.batchLen(missing)
		batch := missing[:n]
		missing = missing[n:]

		parts, err := r.fetchParts(mb, st, batch, log)
		if err != nil {
			return err
		}

		if len(parts) == 0 {
			continue
		}

		res, err := r.tgt.Import(ctx, id, parts)
		if tooLarge(err) && len(parts) == 1 {
			// Larger than the target accepts: reported, not retried forever.
			r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%s: uid %d (%d bytes) is too large for the target, skipped", mb.Name, parts[0].Meta.UID, len(parts[0].Raw)))
			mr.Skipped++

			continue
		}

		if err != nil {
			return fmt.Errorf("import: %w", err)
		}

		mr.Imported += res.Imported
		mr.Existing += res.Existing
		mr.Shared += res.Shared
		mr.Bytes += res.Bytes

		log.Info("batch imported", "messages", len(parts), "last_uid", parts[len(parts)-1].Meta.UID, "remaining", len(missing))
	}

	return nil
}

// tooLarge reports a 413 answer of the target.
func tooLarge(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == string(adminapi.TooLarge)
}

// batchLen is how many of the missing messages go into the next batch
// (at least one, however large).
func (r *run) batchLen(missing []srcMessage) int {
	n, size := 0, int64(0)

	for n < len(missing) && n < r.o.BatchMessages && (n == 0 || size+missing[n].Size <= r.o.BatchBytes) {
		size += missing[n].Size
		n++
	}

	return n
}

// fetchParts reads the bodies of a batch from the source.
func (r *run) fetchParts(mb srcMailbox, st *srcStatus, batch []srcMessage, log *slog.Logger) ([]adminapi.ImportPart, error) {
	uids := make([]uint32, len(batch))
	for i := range batch {
		uids[i] = batch[i].UID
	}

	bodies, err := r.src.fetchBodies(uids)
	if err != nil {
		return nil, fmt.Errorf("source fetch bodies: %w", err)
	}

	parts := make([]adminapi.ImportPart, 0, len(batch))

	for _, m := range batch {
		raw, ok := bodies[m.UID]
		if !ok {
			log.Info("message vanished at the source during the run", "uid", m.UID)
			continue
		}

		parts = append(parts, adminapi.ImportPart{Raw: raw, Meta: r.meta(mb, st, m)})
	}

	return parts, nil
}

// meta is the import identity of a source message.
func (r *run) meta(mb srcMailbox, st *srcStatus, m srcMessage) adminapi.ImportMessageMeta {
	meta := adminapi.ImportMessageMeta{UID: m.UID, ModSeq: m.ModSeq, Flags: storableFlags(m.Flags), InternalDate: m.InternalDate, SaveDate: m.SaveDate}

	if g := strings.ToLower(m.GUID); model.ValidGUID(g) {
		meta.GUID = g
	}

	if mb.Name == inboxName && r.uidl != nil {
		meta.POP3UIDL = r.uidl.format(m.UID, st.UIDValidity, m.GUID)
	}

	return meta
}

// verify compares the target inventory with the source snapshot: the
// same UIDs with the same sizes and flags.
func (r *run) verify(ctx context.Context, id string, src []srcMessage, mr *MailboxReport) error {
	have, err := inventory(ctx, r.tgt, id)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	missing := 0

	for _, m := range src {
		t, ok := have[m.UID]
		switch {
		case !ok:
			missing++ // expunged at the source while it was read
		case !sameFlags(storableFlags(m.Flags), t.Flags) || (m.Size != 0 && t.Size != m.Size):
			return fmt.Errorf("verify: uid %d differs from the source", m.UID)
		}
	}

	if missing -= mr.Skipped; missing < 0 {
		missing = 0
	}

	if extra := len(have) - (len(src) - missing - mr.Skipped); extra != 0 || missing > 0 {
		r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%s: %d source messages vanished during the run, %d target messages not in the snapshot; run again", mr.Name, missing, extra))
		return nil
	}

	mr.Verified = mr.Skipped == 0

	return nil
}

// acl copies the explicit ACL entries (the owner's own entry is implicit).
func (r *run) acl(ctx context.Context, mb srcMailbox, id string) {
	entries, err := r.src.getACL(mb.Raw)
	if err != nil {
		r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%s: source ACL not readable: %v", mb.Name, err))
		return
	}

	for id := range entries {
		switch {
		case id == r.o.SourceUser || id == r.o.Account || id == "owner":
			delete(entries, id) // the owner's rights are implicit
		case id == "" || id == "authenticated" || strings.ContainsAny(id[:1], "$-#"):
			r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%s: ACL identifier %q has no counterpart here, skipped", mb.Name, id))
			delete(entries, id)
		}
	}

	if err := r.tgt.PutACL(ctx, id, entries); err != nil {
		r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%s: ACL not imported: %v", mb.Name, err))
	}
}

// prune deletes target mailboxes that are not in the source.
func (r *run) prune(ctx context.Context, keep map[string]bool) {
	all, err := r.tgt.Mailboxes(ctx)
	if err != nil {
		r.rep.Warnings = append(r.rep.Warnings, "prune: "+err.Error())
		return
	}

	for _, mb := range all {
		if keep[mb.Id] || mb.Name == inboxName {
			continue
		}

		if err := r.tgt.DeleteMailbox(ctx, mb.Id); err != nil {
			r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("prune %s: %v", mb.Name, err))
			continue
		}

		r.rep.Pruned = append(r.rep.Pruned, mb.Name)
	}
}

// sieve copies the scripts and the active one.
func (r *run) sieve(ctx context.Context) {
	fail := func(err error) {
		r.rep.Sieve = "failed: " + err.Error()
		r.rep.Failed++
	}

	c, err := dialSieve(ctx, *r.o.Sieve)
	if err != nil {
		fail(err)
		return
	}
	defer c.Close()

	if err := c.login(r.o.login(), r.o.Password); err != nil {
		fail(err)
		return
	}

	scripts, err := c.scripts()
	if err != nil {
		fail(err)
		return
	}

	active := ""
	out := make([]adminapi.ImportSieveScript, 0, len(scripts))

	for _, s := range scripts {
		out = append(out, adminapi.ImportSieveScript{Name: s.Name, Script: s.Text})

		if s.Active {
			active = s.Name
		}
	}

	if err := r.tgt.PutSieve(ctx, out, active); err != nil {
		fail(err)
		return
	}

	r.rep.Sieve = fmt.Sprintf("%d scripts, active %q", len(out), active)
}

// inventory reads the whole target inventory of a mailbox.
func inventory(ctx context.Context, tgt Target, id string) (map[uint32]adminapi.ImportInventoryEntry, error) {
	out := map[uint32]adminapi.ImportInventoryEntry{}
	after := int64(0)

	for {
		page, err := tgt.Inventory(ctx, id, after)
		if err != nil {
			return nil, err
		}

		for _, e := range page.Messages {
			out[uint32(e.Uid)] = e
		}

		if page.NextAfter == nil || len(page.Messages) == 0 {
			return out, nil
		}

		after = *page.NextAfter
	}
}

// storableFlags drops what the target cannot store (\Recent, invalid
// atoms).
func storableFlags(flags []string) []string {
	out := []string{}

	for _, f := range flags {
		if model.ValidFlag(f) {
			out = append(out, f)
		}
	}

	return out
}

// sameFlags compares flag sets case-insensitively.
func sameFlags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for _, f := range a {
		if !model.HasFlag(b, f) {
			return false
		}
	}

	return true
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}
