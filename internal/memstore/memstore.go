// Package memstore is an in-memory implementation of model.User for
// tests of the protocol layers. It mirrors the semantics of cephstore
// (ids, modseq, change and vanished logs, maildrop lock).
package memstore

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

const (
	inboxName   = "INBOX"
	identAnyone = "anyone" // RFC 4314: rights granted to everyone
)

// New returns an empty store.
func New() *Store { return &Store{users: map[string]*User{}} }

// Store is an in-memory model.Store/User for protocol tests. It mirrors
// the semantics of cephstore (ids, modseq, vanished log, refcount-free).
type Store struct {
	mu    sync.Mutex
	users map[string]*User
}

func (s *Store) user(name string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.users[name]
	if !ok {
		u = &User{name: name, store: s, boxes: map[string]*box{}, blobs: map[string][]byte{}, terms: map[string]map[string]bool{}, shared: map[string]*model.SharedRef{}}
		s.users[name] = u
	}

	return u
}

// User returns a view of the named user's namespace, creating it on
// first use. Views share the data; per-view settings (quota
// enforcement) do not leak into other views.
func (s *Store) User(name string) (model.User, error) { return &view{User: s.user(name)}, nil }

// view is one opened user (model.User) with its own settings.
type view struct {
	*User
	noEnforce bool
}

// SetQuotaEnforced implements model.QuotaExemptable for this view.
func (v *view) SetQuotaEnforced(enforced bool) { v.noEnforce = !enforced }

// Deliver stores with this view's quota setting.
func (v *view) Deliver(name string, raw []byte, flags []string, when time.Time) (*model.Meta, error) {
	return v.deliver(name, v.newGUID(), raw, flags, when, !v.noEnforce)
}

// DeliverGUID stores with this view's quota setting.
func (v *view) DeliverGUID(name, guid string, raw []byte, flags []string, when time.Time) (*model.Meta, error) {
	return v.deliver(name, guid, raw, flags, when, !v.noEnforce)
}

type box struct {
	model.MailboxRec
	recentFrom uint32
	uidNext    uint32
	modseq     uint64
	logStart   uint64 // oldest modseq the change log covers (imports move it)
	keywords   []string
	msgs       []model.Meta
	vanished   map[uint64][]uint32
	changes    []model.Change
	watchers   []chan struct{}
	locked     bool
}

// User is one user's namespace: mailboxes, messages, scripts, ACLs and
// metadata, all guarded by mu.
type User struct {
	name    string
	store   *Store
	mu      sync.Mutex
	shared  map[string]*model.SharedRef // key owner/mailbox
	annot   map[string]string           // "<mailboxID|->|<accessor|>|<entry>"
	vacSeen map[string]time.Time
	subMiss map[string]bool // subscriptions without a mailbox
	pubSubs map[string]bool // subscribed public folders, by mailbox id
	boxes   map[string]*box // by name
	blobs   map[string][]byte
	terms   map[string]map[string]bool // term -> guids
	usage   model.Quota
	limit   model.Quota
	guidN   int
	scripts map[string]string
	active  string
	imp     *importLock
	trace   model.TraceState
}

// Close is a no-op; a memstore user holds no resources.
func (u *User) Close() {}

// ListMailboxes returns all mailboxes sorted by name.
func (u *User) ListMailboxes() ([]model.Mailbox, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	var out []model.Mailbox
	for n, b := range u.boxes {
		out = append(out, model.Mailbox{Name: n, MailboxRec: b.rec()})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

var _ model.MailboxPager = (*User)(nil)

// ListMailboxesAfter implements model.MailboxPager.
func (u *User) ListMailboxesAfter(after string, limit int) ([]model.Mailbox, []string, error) {
	all, err := u.ListMailboxes()
	if err != nil {
		return nil, nil, err
	}

	i := sort.Search(len(all), func(i int) bool { return all[i].Name > after })
	all = all[i:]
	all = all[:min(limit, len(all))]

	scanned := make([]string, len(all))
	for i := range all {
		scanned[i] = all[i].Name
	}

	return all, scanned, nil
}

func (u *User) lookup(name string) (*box, string, error) {
	if strings.EqualFold(name, inboxName) {
		name = inboxName
	}

	b, ok := u.boxes[name]
	if !ok {
		return nil, name, model.ErrNoMailbox
	}

	return b, name, nil
}

// Lookup returns the mailbox of the given name (INBOX is case-insensitive).
func (u *User) Lookup(name string) (*model.Mailbox, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, n, err := u.lookup(name)
	if err != nil {
		return nil, err
	}

	return &model.Mailbox{Name: n, MailboxRec: b.rec()}, nil
}

// CreateMailbox creates a mailbox with the given special-use attributes;
// ErrMailboxExists if it exists.
func (u *User) CreateMailbox(name string, use []string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if strings.EqualFold(name, inboxName) {
		name = inboxName
	}

	if _, ok := u.boxes[name]; ok {
		return model.ErrMailboxExists
	}

	u.guidN++

	b := &box{MailboxRec: model.MailboxRec{ID: fmt.Sprintf("box%d", u.guidN), UIDValidity: uint32(u.guidN), SpecialUse: use}, uidNext: 1, modseq: 1, vanished: map[uint64][]uint32{}}
	if u.subMiss[name] {
		b.Subscribed = true

		delete(u.subMiss, name)
	}

	u.boxes[name] = b

	return nil
}

// EnsureMailbox creates the mailbox if it does not exist yet.
func (u *User) EnsureMailbox(name string, use []string) error {
	if err := u.CreateMailbox(name, use); err != nil && err != model.ErrMailboxExists {
		return err
	}

	return nil
}

// DeleteMailbox removes the mailbox and its messages; a subscription
// survives as one to a missing name.
func (u *User) DeleteMailbox(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, n, err := u.lookup(name)
	if err != nil {
		return err
	}

	delete(u.boxes, n)

	if b.Subscribed { // RFC 3501 6.3.4: the subscription may survive DELETE
		if u.subMiss == nil {
			u.subMiss = map[string]bool{}
		}

		u.subMiss[n] = true
	}

	acl := copyACL(b.ACL)
	u.mu.Unlock()
	u.repoint(acl, n, "")
	u.mu.Lock() // for the deferred unlock

	return nil
}

func copyACL(acl map[string]string) map[string]string {
	out := make(map[string]string, len(acl))
	for k, v := range acl {
		out[k] = v
	}

	return out
}

// repoint moves (newName != "") or drops the grantees' shared pointers.
// Called without u.mu held (grantees may share back to u).
func (u *User) repoint(acl map[string]string, oldName, newName string) {
	for id := range acl {
		if id == identAnyone || id == u.name {
			continue
		}

		g := u.store.user(id)
		g.mu.Lock()
		delete(g.shared, u.name+"/"+oldName)

		if newName != "" {
			g.shared[u.name+"/"+newName] = &model.SharedRef{Owner: u.name, Mailbox: newName}
		}
		g.mu.Unlock()
	}
}

// RenameMailbox renames a mailbox and all its children.
func (u *User) RenameMailbox(oldName, newName string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if _, ok := u.boxes[newName]; ok {
		return model.ErrMailboxExists
	}

	found := false

	type move struct {
		acl      map[string]string
		from, to string
	}

	var moves []move

	for n, b := range u.boxes {
		if n == oldName || strings.HasPrefix(n, oldName+"/") {
			delete(u.boxes, n)
			u.boxes[newName+n[len(oldName):]] = b
			moves = append(moves, move{copyACL(b.ACL), n, newName + n[len(oldName):]})
			found = true
		}
	}

	if !found {
		return model.ErrNoMailbox
	}
	u.mu.Unlock()

	for _, m := range moves {
		u.repoint(m.acl, m.from, m.to)
	}

	u.mu.Lock() // for the deferred unlock

	return nil
}

// SetSubscribed changes the subscription flag of a mailbox; a missing
// mailbox is subscribed by name.
func (u *User) SetSubscribed(name string, sub bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, n, err := u.lookup(name)
	if err != nil {
		if u.subMiss == nil {
			u.subMiss = map[string]bool{}
		}

		if sub {
			u.subMiss[n] = true
		} else {
			delete(u.subMiss, n)
		}

		return nil
	}

	b.Subscribed = sub

	return nil
}

// ListSubscribedMissing returns the subscribed names without a mailbox.
func (u *User) ListSubscribedMissing() ([]string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	var out []string
	for n := range u.subMiss {
		out = append(out, n)
	}

	sort.Strings(out)

	return out, nil
}

// MarkRecent moves the \Recent watermark of the mailbox up to uidNext.
func (u *User) MarkRecent(id string, uidNext uint32) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if b := u.byID(id); b != nil && b.recentFrom < uidNext {
		b.recentFrom = uidNext
	}

	return nil
}

func (u *User) byID(id string) *box {
	for _, b := range u.boxes {
		if b.ID == id {
			return b
		}
	}

	return nil
}

// Info returns the header state of the mailbox.
func (u *User) Info(name string) (*model.MailboxInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, n, err := u.lookup(name)
	if err != nil {
		return nil, err
	}

	return &model.MailboxInfo{Mailbox: model.Mailbox{Name: n, MailboxRec: b.rec()}, RecentFrom: b.recentFrom, UIDNext: b.uidNext, HighestModSeq: b.modseq, LogStart: b.logStart, Keywords: slices.Clone(b.keywords)}, nil
}

// ListMessages returns a copy of the index entries of the mailbox.
func (u *User) ListMessages(id string) ([]model.Meta, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	if b == nil {
		return nil, model.ErrNoMailbox
	}

	return append([]model.Meta{}, b.msgs...), nil
}

// Vanished returns the UIDs expunged after modseq since, sorted.
func (u *User) Vanished(id string, since uint64) ([]uint32, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	if b == nil {
		return nil, model.ErrNoMailbox
	}

	var out []uint32

	for ms, uids := range b.vanished {
		if ms > since {
			out = append(out, uids...)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })

	return out, nil
}

func (b *box) bump() uint64 {
	b.modseq++
	for _, w := range b.watchers {
		select {
		case w <- struct{}{}:
		default:
		}
	}

	return b.modseq
}

// Deliver stores a message under a fresh GUID in the mailbox.
func (u *User) Deliver(name string, raw []byte, flags []string, when time.Time) (*model.Meta, error) {
	return u.deliver(name, u.newGUID(), raw, flags, when, true)
}

// DeliverGUID stores a message under the given GUID in the mailbox.
func (u *User) DeliverGUID(name, guid string, raw []byte, flags []string, when time.Time) (*model.Meta, error) {
	return u.deliver(name, guid, raw, flags, when, true)
}

// newGUID returns the next GUID of the user.
func (u *User) newGUID() string {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.guidN++

	return fmt.Sprintf("%032x", u.guidN)
}

// deliver stores a message; enforce checks the storage limit first.
func (u *User) deliver(name, guid string, raw []byte, flags []string, when time.Time, enforce bool) (*model.Meta, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, _, err := u.lookup(name)
	if err != nil {
		return nil, err
	}

	if enforce && u.limit.Bytes > 0 && u.usage.Bytes+int64(len(raw)) > u.limit.Bytes {
		return nil, model.ErrOverQuota
	}

	if when.IsZero() {
		when = time.Now()
	}

	u.blobs[guid] = raw
	for _, t := range fts.Terms(raw) {
		if u.terms[t] == nil {
			u.terms[t] = map[string]bool{}
		}

		u.terms[t][guid] = true
	}

	env, refs, preview := mailmeta.Extract(raw)
	m := model.Meta{UID: b.uidNext, GUID: guid, Size: int64(len(raw)), Chunks: 1, Flags: flags, InternalDate: when, SaveDate: time.Now(), ModSeq: b.bump(), Envelope: env, References: refs, Preview: preview}
	b.changes = append(b.changes, model.Change{ModSeq: m.ModSeq, UIDs: []uint32{m.UID}})
	b.uidNext++

	b.msgs = append(b.msgs, m)
	for _, f := range flags {
		if !strings.HasPrefix(f, `\`) && !model.HasFlag(b.keywords, f) {
			b.keywords = append(b.keywords, f)
		}
	}

	u.usage.Bytes += m.Size
	u.usage.Msgs++

	return &m, nil
}

// Read returns the raw message of an index entry.
func (u *User) Read(m *model.Meta) ([]byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.blobs[m.GUID], nil
}

// SetFlags replaces the flags of the given UIDs (apply decides the new
// set); messages with a modseq above unchangedSince (if non-zero) are
// skipped and reported in Modified.
func (u *User) SetFlags(name string, uids []uint32, unchangedSince uint64, apply func([]string) []string) (*model.FlagsResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, _, err := u.lookup(name)
	if err != nil {
		return nil, err
	}

	res := &model.FlagsResult{}
	ms := b.modseq + 1
	changed := false

	for i := range b.msgs {
		m := &b.msgs[i]
		in := containsUID(uids, m.UID)

		if !in {
			continue
		}

		if unchangedSince != 0 && m.ModSeq > unchangedSince {
			res.Modified = append(res.Modified, m.UID)
			continue
		}

		m.Flags = apply(m.Flags)
		m.ModSeq = ms
		changed = true

		res.Updated = append(res.Updated, *m)
		b.addKeywords(m.Flags)
	}

	if changed {
		var uids []uint32
		for _, m := range res.Updated {
			uids = append(uids, m.UID)
		}

		b.changes = append(b.changes, model.Change{ModSeq: b.bump(), UIDs: uids})
	}

	return res, nil
}

// containsUID reports whether uids contains uid.
func containsUID(uids []uint32, uid uint32) bool {
	in := false

	for _, x := range uids {
		if x == uid {
			in = true
		}
	}

	return in
}

// addKeywords adds the user keywords (flags not starting with a
// backslash) of flags to the mailbox keyword list.
func (b *box) addKeywords(flags []string) {
	for _, f := range flags {
		if !strings.HasPrefix(f, `\`) && !model.HasFlag(b.keywords, f) {
			b.keywords = append(b.keywords, f)
		}
	}
}

func (u *User) remove(b *box, metas []model.Meta) {
	gone := map[uint32]bool{}

	var uids []uint32

	for _, m := range metas {
		gone[m.UID] = true
		uids = append(uids, m.UID)
	}

	var kept []model.Meta

	for _, m := range b.msgs {
		if !gone[m.UID] {
			kept = append(kept, m)
		}
	}

	b.msgs = kept
	ms := b.bump()
	b.vanished[ms] = uids
	b.changes = append(b.changes, model.Change{ModSeq: ms, UIDs: uids, Expunged: true})
}

// Expunge removes the given messages from the mailbox.
func (u *User) Expunge(name string, metas []model.Meta) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, _, err := u.lookup(name)
	if err != nil {
		return err
	}

	u.remove(b, metas)

	for _, m := range metas {
		u.usage.Bytes -= m.Size
		u.usage.Msgs--
	}

	return nil
}

// Copy appends the messages to dest under new UIDs.
func (u *User) Copy(metas []model.Meta, dest string) ([]model.Meta, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, _, err := u.lookup(dest)
	if err != nil {
		return nil, err
	}

	var out []model.Meta

	for _, m := range metas {
		m.UID, m.POP3UIDL = b.uidNext, ""
		b.uidNext++
		m.ModSeq = b.bump()
		b.changes = append(b.changes, model.Change{ModSeq: m.ModSeq, UIDs: []uint32{m.UID}})
		b.msgs = append(b.msgs, m)
		out = append(out, m)
	}

	return out, nil
}

// GetMessages returns the index entries of the given UIDs.
func (u *User) GetMessages(id string, uids []uint32) ([]model.Meta, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	if b == nil {
		return nil, model.ErrNoMailbox
	}

	var out []model.Meta

	for _, m := range b.msgs {
		for _, uid := range uids {
			if m.UID == uid {
				out = append(out, m)
			}
		}
	}

	return out, nil
}

// Changes returns the change records after modseq since; the log is
// complete except before an import (logStart).
func (u *User) Changes(id string, since uint64) ([]model.Change, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	if b == nil {
		return nil, false, model.ErrNoMailbox
	}

	if since+1 < b.logStart {
		return nil, false, nil
	}

	var out []model.Change

	for _, ch := range b.changes {
		if ch.ModSeq > since {
			out = append(out, ch)
		}
	}

	return out, true, nil
}

type lease struct{ release func() }

func (l *lease) Lost() bool { return false }
func (l *lease) Release()   { l.release() }

// Lock takes the maildrop lock of the mailbox; ErrLocked if it is held.
func (u *User) Lock(id string, _ time.Duration) (model.Lease, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	if b == nil {
		return nil, model.ErrNoMailbox
	}

	if b.locked {
		return nil, model.ErrLocked
	}

	b.locked = true

	return &lease{release: func() { u.mu.Lock(); b.locked = false; u.mu.Unlock() }}, nil
}

// PendingFloor reports no pending changes (memstore writes are atomic).
func (u *User) PendingFloor(string, uint64) (uint64, bool, error) { return 0, false, nil }

// Move copies the messages to dest and removes them from src.
func (u *User) Move(src string, metas []model.Meta, dest string) ([]model.Meta, error) {
	out, err := u.Copy(metas, dest)
	if err != nil {
		return nil, err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	b, _, err := u.lookup(src)
	if err != nil {
		return nil, err
	}

	u.remove(b, metas)

	return out, nil
}

// SearchFields returns the GUIDs whose fields contain every query token
// (OR between fields, AND between tokens).
func (u *User) SearchFields(fields []string, query string) (map[string]bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	var result map[string]bool

	for _, tok := range fts.Tokenize(query) {
		hits := map[string]bool{}

		for _, f := range fields {
			for term, guids := range u.terms {
				fld, t, _ := strings.Cut(term, ":")
				if fld == f && strings.Contains(t, tok) {
					for g := range guids {
						hits[g] = true
					}
				}
			}
		}

		if result == nil {
			result = hits
		} else {
			for g := range result {
				if !hits[g] {
					delete(result, g)
				}
			}
		}
	}

	if result == nil {
		result = map[string]bool{}
	}

	return result, nil
}

// SearchField is SearchFields for one field.
func (u *User) SearchField(f, q string) (map[string]bool, error) {
	return u.SearchFields([]string{f}, q)
}

// QuotaUsage returns the user's current usage.
func (u *User) QuotaUsage() (model.Quota, error) { return u.usage, nil }

// QuotaLimit returns the user's limit.
func (u *User) QuotaLimit() (model.Quota, error) { return u.limit, nil }

// BeginDelivery returns no intent: delivery intents are not needed in
// memory (nothing survives a crash).
func (u *User) BeginDelivery() (string, error) { return "", nil }

// RecordDelivery is a no-op (see BeginDelivery).
func (u *User) RecordDelivery(string, string, string) error { return nil }

// EndDelivery is a no-op (see BeginDelivery).
func (u *User) EndDelivery(string) error { return nil }

// SetQuotaLimit stores the user's limit.
func (u *User) SetQuotaLimit(q model.Quota) error { u.limit = q; return nil }

// SetStorageLimit stores the user's storage limit.
func (u *User) SetStorageLimit(bytes int64) error { u.limit.Bytes = bytes; return nil }

type watch struct{ ch chan struct{} }

func (w *watch) Events() <-chan struct{} { return w.ch }
func (w *watch) Errors() <-chan error    { return nil }
func (w *watch) Close()                  {}

// Watch returns a watch that fires on every change of the mailbox.
func (u *User) Watch(id string) (model.Watch, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	ch := make(chan struct{}, 1)
	b.watchers = append(b.watchers, ch)

	return &watch{ch: ch}, nil
}

// ListScripts returns the names of the Sieve scripts and the active one.
func (u *User) ListScripts() ([]string, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	var names []string
	for n := range u.scripts {
		names = append(names, n)
	}

	sort.Strings(names)

	return names, u.active, nil
}

// GetScript returns the text of a Sieve script.
func (u *User) GetScript(name string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	s, ok := u.scripts[name]
	if !ok {
		return "", model.ErrNoScript
	}

	return s, nil
}

// ActiveScript returns the text of the active Sieve script.
func (u *User) ActiveScript() (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.scripts[u.active], nil
}

// PutScript stores a Sieve script.
func (u *User) PutScript(name, text string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.scripts == nil {
		u.scripts = map[string]string{}
	}

	u.scripts[name] = text

	return nil
}

// DeleteScript removes a Sieve script that is not active.
func (u *User) DeleteScript(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.active == name {
		return model.ErrScriptActive
	}

	delete(u.scripts, name)

	return nil
}

// SetActiveScript makes the named script active (empty: none).
func (u *User) SetActiveScript(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if name != "" {
		if _, ok := u.scripts[name]; !ok {
			return model.ErrNoScript
		}
	}

	u.active = name

	return nil
}

// RenameScript renames a Sieve script.
func (u *User) RenameScript(oldName, newName string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	s, ok := u.scripts[oldName]
	if !ok {
		return model.ErrNoScript
	}

	if _, taken := u.scripts[newName]; taken {
		return model.ErrScriptExists
	}

	delete(u.scripts, oldName)

	u.scripts[newName] = s
	if u.active == oldName {
		u.active = newName
	}

	return nil
}

// --- ACL / shared folders ------------------------------------------------

// GetACL returns the explicit ACL entries of a mailbox.
func (u *User) GetACL(name string) (map[string]string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b, _, err := u.lookup(name)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for k, v := range b.ACL {
		out[k] = v
	}

	return out, nil
}

// SetACL replaces the rights of identifier on the mailbox; empty rights
// remove the entry. The grantee's shared pointer follows.
func (u *User) SetACL(name, identifier, rights string) error {
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if identifier == "" || identifier == u.name {
		return fmt.Errorf("invalid identifier")
	}

	if rights != "" {
		r, ok := model.NormalizeRights(rights)
		if !ok {
			return fmt.Errorf("invalid rights %q", rights)
		}

		rights = r
	}

	u.mu.Lock()

	b, canon, err := u.lookup(name)
	if err != nil {
		u.mu.Unlock()
		return err
	}

	setACLEntry(b, identifier, rights)
	u.mu.Unlock()

	u.syncPointer(canon, identifier, rights)

	return nil
}

// syncPointer writes or removes the grantee's shared pointer ("anyone"
// has none, and public folders have none at all: they are found through
// their namespace).
func (u *User) syncPointer(canon, identifier, rights string) {
	if identifier == identAnyone || model.IsPublic(u.name) {
		return
	}

	g := u.store.user(identifier)
	g.mu.Lock()
	defer g.mu.Unlock()

	key := u.name + "/" + canon
	if rights == "" {
		delete(g.shared, key)
	} else if _, ok := g.shared[key]; !ok {
		g.shared[key] = &model.SharedRef{Owner: u.name, Mailbox: canon}
	}
}

// rec returns a copy of the registry record: callers read it without
// the lock (cephstore decodes a fresh one every time).
func (b *box) rec() model.MailboxRec {
	r := b.MailboxRec
	r.ACL = maps.Clone(b.ACL)
	r.SpecialUse = slices.Clone(b.SpecialUse)

	return r
}

// setACLEntry sets or removes one entry and counts the generation; it
// reports whether anything changed.
func setACLEntry(b *box, identifier, rights string) bool {
	if b.ACL[identifier] == rights {
		return false
	}

	if rights == "" {
		delete(b.ACL, identifier)
	} else {
		if b.ACL == nil {
			b.ACL = map[string]string{}
		}

		b.ACL[identifier] = rights
	}

	b.ACLGen++

	return true
}

var _ model.ACLModifier = (*User)(nil)

// ModifyACL implements model.ACLModifier: read, check and change happen
// under one lock.
func (u *User) ModifyACL(name, identifier string, expectGen int64, change func(old string) string) (string, uint64, error) {
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if identifier == "" || identifier == u.name {
		return "", 0, fmt.Errorf("invalid identifier")
	}

	u.mu.Lock()

	b, canon, err := u.lookup(name)
	if err != nil {
		u.mu.Unlock()
		return "", 0, err
	}

	if expectGen >= 0 && b.ACLGen != uint64(expectGen) {
		u.mu.Unlock()
		return "", 0, model.ErrVersionConflict
	}

	rights := change(b.ACL[identifier])
	if rights != "" {
		r, ok := model.NormalizeRights(rights)
		if !ok {
			u.mu.Unlock()
			return "", 0, fmt.Errorf("invalid rights %q", rights)
		}

		rights = r
	}

	changed := setACLEntry(b, identifier, rights)
	gen := b.ACLGen
	u.mu.Unlock()

	if changed || rights != "" {
		u.syncPointer(canon, identifier, rights)
	}

	return rights, gen, nil
}

// ListShared returns the pointers to mailboxes other users shared.
func (u *User) ListShared() ([]model.SharedRef, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	out := make([]model.SharedRef, 0, len(u.shared))
	for _, r := range u.shared {
		out = append(out, *r)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Owner+"/"+out[i].Mailbox < out[j].Owner+"/"+out[j].Mailbox })

	return out, nil
}

// PublicSubscriptions implements model.User.
func (u *User) PublicSubscriptions() (map[string]bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	out := make(map[string]bool, len(u.pubSubs))
	for id := range u.pubSubs {
		out[id] = true
	}

	return out, nil
}

// SetPublicSubscribed implements model.User.
func (u *User) SetPublicSubscribed(mailboxID string, subscribed bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.pubSubs == nil {
		u.pubSubs = map[string]bool{}
	}

	if subscribed {
		u.pubSubs[mailboxID] = true
	} else {
		delete(u.pubSubs, mailboxID)
	}

	return nil
}

// SetSharedSubscribed changes the subscription flag of a shared pointer.
func (u *User) SetSharedSubscribed(owner, mailbox string, subscribed bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	r, ok := u.shared[owner+"/"+mailbox]
	if !ok {
		return model.ErrNoMailbox
	}

	r.Subscribed = subscribed

	return nil
}

// RemoveSharedRef removes a shared pointer.
func (u *User) RemoveSharedRef(ref model.SharedRef) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if cur, ok := u.shared[ref.Owner+"/"+ref.Mailbox]; ok && (ref.Gen == 0 || cur.Gen <= ref.Gen) {
		delete(u.shared, ref.Owner+"/"+ref.Mailbox)
	}

	return nil
}

// --- METADATA --------------------------------------------------------------

func (u *User) annotKey(mailbox, accessor string) (string, error) {
	id := "-"

	if mailbox != "" {
		b, _, err := u.lookup(mailbox)
		if err != nil {
			return "", err
		}

		id = b.ID
	}

	return id + "|" + accessor + "|", nil
}

// GetMetadata returns the metadata entries of a mailbox (empty: the
// server) visible to accessor.
func (u *User) GetMetadata(mailbox, accessor string) (map[string]string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	shared, err := u.annotKey(mailbox, "")
	if err != nil {
		return nil, err
	}

	private, _ := u.annotKey(mailbox, accessor)
	out := map[string]string{}

	for k, v := range u.annot {
		if strings.HasPrefix(k, shared) {
			out[strings.TrimPrefix(k, shared)] = v
		} else if strings.HasPrefix(k, private) {
			out[strings.TrimPrefix(k, private)] = v
		}
	}

	return out, nil
}

// SetMetadata sets (or, with a nil value, removes) metadata entries.
func (u *User) SetMetadata(mailbox, accessor string, entries map[string]*string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.annot == nil {
		u.annot = map[string]string{}
	}

	for entry, val := range entries {
		acc := ""
		if strings.HasPrefix(entry, "/private/") {
			acc = accessor
		}

		prefix, err := u.annotKey(mailbox, acc)
		if err != nil {
			return err
		}

		if val == nil {
			delete(u.annot, prefix+entry)
		} else {
			u.annot[prefix+entry] = *val
		}
	}

	return nil
}

// VacationSeen reports whether key was seen within ttl and records it.
func (u *User) VacationSeen(key string, ttl time.Duration) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.vacSeen == nil {
		u.vacSeen = map[string]time.Time{}
	}

	already := false
	if t, ok := u.vacSeen[key]; ok && time.Now().Before(t) {
		already = true
	}

	switch {
	case ttl > 0 && !already:
		u.vacSeen[key] = time.Now().Add(ttl)
	case ttl == 0:
		delete(u.vacSeen, key)
	}

	return already, nil
}
