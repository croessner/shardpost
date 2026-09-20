package imapproto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2/imapserver"

	"shardpost/internal/membudget"
	"shardpost/internal/model"
)

const delim = '/'

// --- LIST / LSUB -----------------------------------------------------------

type listOptions struct {
	selectSubscribed, selectRecursive, selectSpecialUse bool
	returnSubscribed, returnChildren, returnSpecialUse  bool
	returnMyRights                                      bool
	returnStatus                                        []string
}

func (c *conn) cmdList(tag string, lsub bool) error {
	var opts listOptions

	t, err := c.r.next()
	if err != nil {
		return err
	}

	if t.kind == tokList && !lsub { // selection options
		if !c.parseListSelection(tag, t, &opts) {
			return nil
		}

		if t, err = c.r.next(); err != nil {
			return err
		}
	}

	if !t.isString() {
		return fmt.Errorf("%w: expected reference", errSyntax)
	}

	ref := t.str

	patterns, err := c.readOneOrList("pattern")
	if err != nil {
		return err
	}

	if !c.r.eol() { // RETURN (...)
		if ok, err := c.parseListReturn(tag, &opts); err != nil || !ok {
			return err
		}
	}

	return c.listRun(tag, lsub, opts, ref, patterns)
}

// listRun answers a parsed LIST/LSUB command.
func (c *conn) listRun(tag string, lsub bool, opts listOptions, ref string, patterns []string) error {
	if lsub {
		opts.selectSubscribed = true
	}

	if opts.selectRecursive && !opts.selectSubscribed {
		// RFC 5258 3.1: RECURSIVEMATCH needs another selection option
		c.bad(tag, "RECURSIVEMATCH requires SUBSCRIBED")
		return nil
	}

	if len(patterns) == 1 && patterns[0] == "" {
		c.w.untagged(`%s (\Noselect) "/" ""`, listVerb(lsub))
		c.ok(tag, "", "LIST completed")

		return nil
	}

	// The other namespaces are collected first: an account whose own
	// mailbox is named like the root of one of them gets a single LIST
	// line, and that line has to say \HasChildren.
	public, publicNodes, err := c.collectPublic()
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	shared, sharedNodes, err := c.collectSharedRefs()
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if err := c.listOwn(lsub, opts, ref, patterns, namespaceRoots(ref, patterns, public, shared)); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.listForeign(public, publicNodes, lsub, opts, ref, patterns)
	c.listForeign(shared, sharedNodes, lsub, opts, ref, patterns)

	c.ok(tag, "", "LIST completed")

	return nil
}

// listVerb is the response name of LIST or LSUB.
func listVerb(lsub bool) string {
	if lsub {
		return kwLsub
	}

	return kwList
}

// parseListSelection parses the LIST selection options of t into opts; it
// answers BAD and returns false for an unknown option.
func (c *conn) parseListSelection(tag string, t token, opts *listOptions) bool {
	for _, o := range t.list {
		switch strings.ToUpper(o.str) {
		case kwSubscribed:
			opts.selectSubscribed = true
		case "RECURSIVEMATCH":
			opts.selectRecursive = true
		case kwSpecialUse:
			opts.selectSpecialUse = true
		case "REMOTE":
		default:
			c.bad(tag, "Unknown LIST selection option")
			return false
		}
	}

	return true
}

// readOneOrList reads a string or a parenthesised list of strings (LIST
// patterns, METADATA entries); what names it in the syntax error.
func (c *conn) readOneOrList(what string) ([]string, error) {
	pt, err := c.r.next()
	if err != nil {
		return nil, err
	}

	var out []string

	switch pt.kind {
	case tokList:
		for _, p := range pt.list {
			out = append(out, p.str)
		}
	case tokAtom, tokString:
		out = []string{pt.str}
	default:
		return nil, fmt.Errorf("%w: expected %s", errSyntax, what)
	}

	return out, nil
}

// parseListReturn parses "RETURN (options)" into opts. ok is false when an
// unknown option was answered with BAD.
func (c *conn) parseListReturn(tag string, opts *listOptions) (bool, error) {
	kw, err := c.r.atom()
	if err != nil || !strings.EqualFold(kw, "RETURN") {
		return false, fmt.Errorf("%w: expected RETURN", errSyntax)
	}

	rt, err := c.r.next()
	if err != nil {
		return false, err
	}

	return c.applyListReturn(tag, rt.list, opts)
}

// applyListReturn applies the RETURN options of LIST to opts; it answers
// BAD and returns false for an unknown option.
func (c *conn) applyListReturn(tag string, list []token, opts *listOptions) (bool, error) {
	for i := 0; i < len(list); i++ {
		switch strings.ToUpper(list[i].str) {
		case kwSubscribed:
			opts.returnSubscribed = true
		case "CHILDREN":
			opts.returnChildren = true
		case kwSpecialUse:
			opts.returnSpecialUse = true
		case kwMyRights:
			opts.returnMyRights = true
		case kwStatus:
			i++
			if i >= len(list) || list[i].kind != tokList {
				return false, fmt.Errorf("%w: STATUS needs a list", errSyntax)
			}

			for _, it := range list[i].list {
				opts.returnStatus = append(opts.returnStatus, strings.ToUpper(it.str))
			}
		default:
			c.bad(tag, "Unknown LIST return option")
			return false, nil
		}
	}

	return true, nil
}

// matchAny reports whether name matches one of the LIST patterns.
func matchAny(name, ref string, patterns []string) bool {
	for _, p := range patterns {
		if imapserver.MatchList(name, delim, ref, p) {
			return true
		}
	}

	return false
}

// listTree is the account's own mailbox hierarchy as LIST sees it.
type listTree struct {
	boxes   []model.Mailbox
	byName  map[string]*model.Mailbox
	names   map[string]bool // true = real mailbox, false = implicit parent
	missing map[string]bool // subscriptions without a mailbox
	// foreign are names whose children live in another namespace: an own
	// mailbox called like the root of the shared or public namespace is
	// one LIST line, and that line must say \HasChildren — and stand in
	// for the subscriptions below it.
	foreign  map[string]foreignRoot
	ref      string
	patterns []string
}

// foreignRoot says what another namespace holds below a name of the
// account's own listing.
type foreignRoot struct {
	subscribed bool // a subscribed entry lies below it
	unmatched  bool // one of those is not listed by the pattern itself
}

// hasChildren reports whether n has any descendant, in this namespace
// or in another one.
func (lt *listTree) hasChildren(n string) bool {
	if _, ok := lt.foreign[n]; ok {
		return true
	}

	for cn := range lt.names {
		if strings.HasPrefix(cn, n+string(delim)) {
			return true
		}
	}

	return false
}

// subscribedChild reports whether n has a subscribed descendant, here
// or in another namespace.
func (lt *listTree) subscribedChild(n string) bool {
	if lt.foreign[n].subscribed {
		return true
	}

	for _, b := range lt.boxes {
		if b.Subscribed && strings.HasPrefix(b.Name, n+string(delim)) {
			return true
		}
	}

	return false
}

// unmatchedChild: a subscribed descendant that no pattern lists
// itself (RFC 5258 3.5: CHILDINFO only stands in for such children)
func (lt *listTree) unmatchedChild(n string) bool {
	if lt.foreign[n].unmatched {
		return true
	}

	for _, b := range lt.boxes {
		if !b.Subscribed || !strings.HasPrefix(b.Name, n+string(delim)) {
			continue
		}

		if !matchAny(b.Name, lt.ref, lt.patterns) {
			return true
		}
	}

	return false
}

// warnShadowed logs the account's own mailboxes that lie below a
// namespace prefix. They are listed, but a client cannot open them:
// their name resolves to the other namespace. This happens when a
// prefix is introduced or changed while such a mailbox exists; the
// admin API can rename it out of the way.
func (c *conn) warnShadowed(boxes []model.Mailbox) {
	if c.shadowWarned {
		return
	}

	for i := range boxes {
		if !shadowed(boxes[i].Name, c.sharedPrefix(), c.publicPrefix()) {
			continue
		}

		c.shadowWarned = true

		c.log.Warn("own mailbox lies below a namespace prefix and cannot be opened by a client",
			"mailbox", boxes[i].Name, "mailbox_id", boxes[i].ID,
			"shared_prefix", c.sharedPrefix(), "public_prefix", c.publicPrefix())

		return
	}

	c.shadowWarned = true
}

// shadowed reports whether a name lies below one of the namespace
// prefixes (empty prefixes are switched off).
func shadowed(name string, prefixes ...string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}

	return false
}

// newListTree loads the account's mailboxes, subscriptions without a
// mailbox (when subscriptions matter) and their implicit parents.
func (c *conn) newListTree(opts listOptions, ref string, patterns []string, foreign map[string]foreignRoot) (*listTree, error) {
	boxes, err := c.user.ListMailboxes()
	if err != nil {
		return nil, err
	}
	// Subscriptions to names without a mailbox (RFC 5258 \NonExistent).
	missing := map[string]bool{}

	if opts.selectSubscribed || opts.returnSubscribed {
		if names, err := c.user.ListSubscribedMissing(); err == nil {
			for _, n := range names {
				missing[n] = true
				boxes = append(boxes, model.Mailbox{Name: n, MailboxRec: model.MailboxRec{Subscribed: true}})
			}
		}
	}

	c.warnShadowed(boxes)

	lt := &listTree{boxes: boxes, byName: map[string]*model.Mailbox{}, names: map[string]bool{}, missing: missing,
		foreign: foreign, ref: ref, patterns: patterns}

	for i := range boxes {
		lt.byName[boxes[i].Name] = &boxes[i]
		lt.names[boxes[i].Name] = true

		n := boxes[i].Name
		for {
			j := strings.LastIndex(n, string(delim))
			if j < 0 {
				break
			}

			n = n[:j]
			if !lt.names[n] {
				lt.names[n] = false
			}
		}
	}

	return lt, nil
}

// listOwn lists the matching mailboxes of the account's own namespace.
func (c *conn) listOwn(lsub bool, opts listOptions, ref string, patterns []string, foreign map[string]foreignRoot) error {
	lt, err := c.newListTree(opts, ref, patterns, foreign)
	if err != nil {
		return err
	}

	sorted := make([]string, 0, len(lt.names))
	for n := range lt.names {
		sorted = append(sorted, n)
	}

	sort.Strings(sorted)

	for _, name := range sorted {
		if !matchAny(name, ref, patterns) {
			continue
		}

		c.listOwnEntry(lt, name, lsub, opts)
	}

	return nil
}

// listOwnEntry writes the LIST/LSUB response for one matching name of the
// own namespace, if the selection options admit it.
func (c *conn) listOwnEntry(lt *listTree, name string, lsub bool, opts listOptions) {
	mb := lt.byName[name]

	childInfo, viaChild, list := lt.childInfoOf(mb, name, lsub, opts)
	if !list {
		if lsub && opts.selectSubscribed {
			c.lsubParent(lt, name)
		}

		return
	}

	if opts.selectSpecialUse && (mb == nil || len(mb.SpecialUse) == 0) {
		return
	}

	attrs := lt.attrs(mb, name, viaChild, lsub, opts)
	c.w.untagged(`%s (%s) "/" %s%s`, listVerb(lsub), strings.Join(attrs, " "), astringOut(name), childInfo)
	c.listOwnExtras(mb, name, opts)
}

// childInfoOf decides whether a name of the own namespace is listed at
// all, and with which CHILDINFO. A name that does not match the
// selection itself is listed when it stands in for a subscribed child
// the pattern does not list; a name that does match says so too
// (RFC 5258 3.5, example 8A1).
func (lt *listTree) childInfoOf(mb *model.Mailbox, name string, lsub bool, opts listOptions) (childInfo string, viaChild, list bool) {
	standsIn := opts.selectRecursive && lt.subscribedChild(name) && lt.unmatchedChild(name)

	if opts.selectSubscribed && (mb == nil || !mb.Subscribed) {
		if lsub || !standsIn {
			return "", false, false
		}

		return childInfoSubscribed, true, true
	}

	if standsIn {
		return childInfoSubscribed, false, true
	}

	return "", false, true
}

// lsubParent lists a name that is not subscribed itself for LSUB.
// RFC 3501 6.3.9: a parent of subscribed children is listed with
// \Noselect when the pattern ends in %.
func (c *conn) lsubParent(lt *listTree, name string) {
	if lt.subscribedChild(name) && strings.HasSuffix(lt.patterns[0], "%") {
		c.w.untagged(`LSUB (%s) "/" %s`, `\Noselect`, astringOut(name))
	}
}

// attrs returns the name attributes of a listed own mailbox (mb nil for
// an implicit parent; viaChild when listed for subscribed children only).
func (lt *listTree) attrs(mb *model.Mailbox, name string, viaChild, lsub bool, opts listOptions) []string {
	var attrs []string

	if mb == nil {
		attrs = append(attrs, `\Noselect`, `\HasChildren`)
	} else if viaChild {
		// listed for its subscribed children only: no attributes
		// unless CHILDREN was requested (RFC 5258 example 9)
		if opts.returnChildren {
			attrs = append(attrs, `\HasChildren`)
		}
	} else {
		attrs = lt.boxAttrs(mb, name, lsub, opts)
	}

	return attrs
}

// boxAttrs returns the name attributes of an existing listed own mailbox.
func (lt *listTree) boxAttrs(mb *model.Mailbox, name string, lsub bool, opts listOptions) []string {
	var attrs []string

	if lt.missing[name] {
		attrs = append(attrs, `\NonExistent`)
	}

	if mb.Subscribed && (opts.returnSubscribed || opts.selectSubscribed || lsub) {
		attrs = append(attrs, `\Subscribed`)
	}

	if !lt.missing[name] {
		if lt.hasChildren(name) {
			attrs = append(attrs, `\HasChildren`)
		} else {
			attrs = append(attrs, `\HasNoChildren`)
		}
	}

	if opts.returnSpecialUse || opts.selectSpecialUse {
		attrs = append(attrs, mb.SpecialUse...)
	}

	return attrs
}

// listOwnExtras writes the STATUS and MYRIGHTS responses requested with
// RETURN for a listed own mailbox.
func (c *conn) listOwnExtras(mb *model.Mailbox, name string, opts listOptions) {
	if len(opts.returnStatus) > 0 && mb != nil {
		if line, err := c.statusLine(&target{u: c.user, name: name, full: name}, opts.returnStatus); err == nil {
			c.w.untagged("%s", line)
		}
	}

	if opts.returnMyRights && mb != nil {
		c.w.untagged("MYRIGHTS %s %s", astringOut(name), model.AllRights)
	}
}

// sharedEntry is one mailbox of another user listed in the shared
// namespace.
type sharedEntry struct {
	full   string
	sub    bool
	leaf   bool
	rights string
}

// sharedTree is the hierarchy of shared mailboxes as LIST sees it.
type sharedTree struct {
	entries  []sharedEntry
	full     map[string]sharedEntry
	ref      string
	patterns []string
}

// hasChild reports whether n has a listed descendant.
func (st *sharedTree) hasChild(n string) bool {
	for _, e := range st.entries {
		if strings.HasPrefix(e.full, n+"/") {
			return true
		}
	}

	return false
}

// subChild reports whether n has a subscribed descendant.
func (st *sharedTree) subChild(n string) bool {
	for _, e := range st.entries {
		if e.sub && strings.HasPrefix(e.full, n+"/") {
			return true
		}
	}

	return false
}

// subChildUnmatched reports whether n has a subscribed descendant that no
// pattern lists itself.
func (st *sharedTree) subChildUnmatched(n string) bool {
	for _, e := range st.entries {
		if !e.sub || !strings.HasPrefix(e.full, n+"/") {
			continue
		}

		if !matchAny(e.full, st.ref, st.patterns) {
			return true
		}
	}

	return false
}

// collectSharedRefs reads the shared namespace: the mailboxes of other
// users this account holds a pointer for, verified against the owner's
// ACL (right l). Pointers without rights are pruned.
func (c *conn) collectSharedRefs() ([]sharedEntry, map[string]bool, error) {
	if c.sharedPrefix() == "" {
		return nil, nil, nil // no shared namespace: nothing to list, nothing to read
	}

	refs, err := c.user.ListShared()
	if err != nil {
		return nil, nil, err
	}

	if len(refs) == 0 {
		return nil, nil, nil
	}

	entries, nodes := c.collectShared(refs)

	return entries, nodes, nil
}

// namespaceRoots returns the first segment of every entry of another
// namespace: the names under which the account's own listing has
// children it does not know about, with what lies below them.
func namespaceRoots(ref string, patterns []string, sets ...[]sharedEntry) map[string]foreignRoot {
	roots := map[string]foreignRoot{}

	for _, set := range sets {
		for _, e := range set {
			i := strings.Index(e.full, string(delim))
			if i <= 0 {
				continue
			}

			r := roots[e.full[:i]]
			if e.sub {
				r.subscribed = true
				// RFC 5258 3.5: CHILDINFO only stands in for children
				// the pattern does not list itself.
				r.unmatched = r.unmatched || !matchAny(e.full, ref, patterns)
			}

			roots[e.full[:i]] = r
		}
	}

	return roots
}

// listForeign writes the LIST lines of another namespace: its entries
// and the implicit parents that only exist as a path.
func (c *conn) listForeign(entries []sharedEntry, nodes map[string]bool, lsub bool, opts listOptions, ref string, patterns []string) {
	if len(entries) == 0 {
		return
	}

	st := &sharedTree{entries: entries, full: map[string]sharedEntry{}, ref: ref, patterns: patterns}
	for _, e := range entries {
		st.full[e.full] = e
	}

	names := make([]string, 0, len(nodes)+len(entries))

	for n := range nodes {
		if _, ok := st.full[n]; !ok {
			names = append(names, n)
		}
	}

	for _, e := range entries {
		names = append(names, e.full)
	}

	sort.Strings(names)

	for _, name := range names {
		if matchAny(name, ref, patterns) {
			c.listSharedEntry(st, name, lsub, opts)
		}
	}
}

// collectPublic reads the public namespace and returns the folders the
// account may see, plus their implicit parents (\Noselect).
func (c *conn) collectPublic() ([]sharedEntry, map[string]bool, error) {
	if c.publicPrefix() == "" {
		return nil, nil, nil // public folders are switched off
	}

	u, err := c.publicUser()
	if err != nil {
		return nil, nil, err
	}

	boxes, err := u.ListMailboxes()
	if err != nil || len(boxes) == 0 {
		return nil, nil, err
	}

	subs, err := c.user.PublicSubscriptions()
	if err != nil {
		return nil, nil, err
	}

	var entries []sharedEntry

	nodes := map[string]bool{}
	prefix := c.publicPrefix()
	// As in the shared namespace: the root is a node of its own unless
	// the account has a mailbox of that name (see collectShared).
	root := strings.TrimSuffix(prefix, "/")
	ownRoot := c.ownMailbox(root)

	c.dropStalePublicSubs(boxes, subs)

	for i := range boxes {
		// The rights come with the registry entry; a second read per
		// folder would cost one round trip per folder and LIST.
		rights := model.EffectiveRights(boxes[i].ACL, c.name)
		if !model.HasRight(rights, 'l') {
			continue
		}

		full := prefix + boxes[i].Name
		entries = append(entries, sharedEntry{full: full, sub: subs[boxes[i].ID], leaf: true, rights: rights})

		if !ownRoot {
			nodes[root] = true
		}

		for n := full; ; {
			j := strings.LastIndex(n, "/")
			if j < 0 || n[:j] == root {
				break
			}

			n = n[:j]
			nodes[n] = true
		}
	}

	return entries, nodes, nil
}

// dropStalePublicSubs removes subscriptions to public folders that no
// longer exist, one CAS per leftover and only once.
func (c *conn) dropStalePublicSubs(boxes []model.Mailbox, subs map[string]bool) {
	if len(subs) == 0 {
		return
	}

	live := make(map[string]bool, len(boxes))
	for i := range boxes {
		live[boxes[i].ID] = true
	}

	for id := range subs {
		if !live[id] {
			_ = c.user.SetPublicSubscribed(id, false)
		}
	}
}

// ownMailbox reports whether the account has a mailbox of that exact
// name.
func (c *conn) ownMailbox(name string) bool {
	_, err := c.user.Lookup(name)

	return err == nil
}

// collectShared resolves the account's shared-mailbox pointers to the
// entries it may list (right l) and their implicit parents (\Noselect),
// pruning stale pointers.
func (c *conn) collectShared(refs []model.SharedRef) ([]sharedEntry, map[string]bool) {
	var entries []sharedEntry

	nodes := map[string]bool{} // implicit parents (\Noselect)
	shared := c.sharedPrefix()
	// The root of the namespace is a node of its own — unless the
	// account has a mailbox of that name, which its own listing already
	// shows, with \HasChildren for what lies below it here.
	root := strings.TrimSuffix(shared, "/")
	ownRoot := c.ownMailbox(root)

	for _, r := range refs {
		if model.IsPublic(r.Owner) {
			// Public folders are listed from their namespace; a pointer
			// to them is a leftover and would list the folder twice.
			_ = c.user.RemoveSharedRef(r)

			continue
		}

		t, err := c.target(shared + r.Owner + "/" + r.Mailbox)
		if err != nil {
			continue
		}

		rights, err := c.rightsOf(t)
		if errors.Is(err, model.ErrNoMailbox) {
			// the mailbox is gone: the pointer is stale (unless it was
			// re-created and shared again meanwhile, hence the generation)
			_ = c.user.RemoveSharedRef(r)
			continue
		}

		if err != nil || !model.HasRight(rights, 'l') {
			continue // hidden this time; a temporary error must not delete
		}

		entries = append(entries, sharedEntry{full: t.full, sub: r.Subscribed, leaf: true, rights: rights})

		if !ownRoot {
			nodes[root] = true
		}

		nodes[shared+r.Owner] = true

		n := t.full
		for {
			j := strings.LastIndex(n, "/")
			if j < 0 || n[:j] == shared+r.Owner {
				break
			}

			n = n[:j]
			nodes[n] = true
		}
	}

	return entries, nodes
}

// listSharedEntry writes the LIST/LSUB response for one matching name of
// the shared namespace, if the selection options admit it.
func (c *conn) listSharedEntry(st *sharedTree, name string, lsub bool, opts listOptions) {
	e, isBox := st.full[name]

	childInfo := ""

	switch {
	case opts.selectSubscribed && (!isBox || !e.sub):
		if lsub {
			c.lsubSharedParent(st, name)
			return
		}

		if !opts.selectRecursive || !st.subChild(name) || !st.subChildUnmatched(name) {
			return
		}

		childInfo = childInfoSubscribed
	case opts.selectRecursive && st.subChild(name) && st.subChildUnmatched(name):
		// As above: the entry matches itself and still stands in for a
		// subscribed child the pattern does not list.
		childInfo = childInfoSubscribed
	}

	if opts.selectSpecialUse {
		return
	}

	attrs := st.attrs(e, isBox, name, lsub, opts)
	c.w.untagged(`%s (%s) "/" %s%s`, listVerb(lsub), strings.Join(attrs, " "), astringOut(name), childInfo)
	c.listSharedExtras(e, isBox, name, opts)
}

// lsubSharedParent lists a name of another namespace that is not
// subscribed itself, as lsubParent does for the account's own
// mailboxes (RFC 3501 6.3.9).
func (c *conn) lsubSharedParent(st *sharedTree, name string) {
	if st.subChild(name) && len(st.patterns) > 0 && strings.HasSuffix(st.patterns[0], "%") {
		c.w.untagged(`LSUB (%s) "/" %s`, `\Noselect`, astringOut(name))
	}
}

// attrs returns the name attributes of a listed shared name.
func (st *sharedTree) attrs(e sharedEntry, isBox bool, name string, lsub bool, opts listOptions) []string {
	var attrs []string

	if !isBox {
		return append(attrs, `\Noselect`, `\HasChildren`)
	}

	if e.sub && (opts.returnSubscribed || opts.selectSubscribed || lsub) {
		attrs = append(attrs, `\Subscribed`)
	}

	if st.hasChild(name) {
		attrs = append(attrs, `\HasChildren`)
	} else {
		attrs = append(attrs, `\HasNoChildren`)
	}

	return attrs
}

// listSharedExtras writes the STATUS and MYRIGHTS responses requested
// with RETURN for a listed shared mailbox.
func (c *conn) listSharedExtras(e sharedEntry, isBox bool, name string, opts listOptions) {
	if len(opts.returnStatus) > 0 && isBox && model.HasRight(e.rights, 'r') {
		if t, err := c.target(name); err == nil {
			if line, err := c.statusLine(t, opts.returnStatus); err == nil {
				c.w.untagged("%s", line)
			}
		}
	}

	if opts.returnMyRights && isBox {
		c.w.untagged("MYRIGHTS %s %s", astringOut(name), e.rights)
	}
}

// --- CREATE / DELETE / RENAME / SUBSCRIBE ----------------------------------

func (c *conn) cmdCreate(tag string) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	use, err := c.readCreateUse()
	if err != nil {
		return err
	}

	t, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if t.shared() && !c.mayCreateShared(tag, t) {
		return nil
	}

	if err := t.u.CreateMailbox(t.name, use); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.ok(tag, "", "CREATE completed")

	return nil
}

// readCreateUse reads the optional CREATE parameter (USE (\Drafts ...))
// and returns the canonical special-use attributes.
func (c *conn) readCreateUse() ([]string, error) {
	var use []string

	if !c.r.eol() { // (USE (\Drafts ...))
		t, err := c.r.next()
		if err != nil {
			return nil, err
		}

		if t.kind == tokList && len(t.list) == 2 && strings.EqualFold(t.list[0].str, "USE") {
			for _, u := range t.list[1].list {
				use = append(use, canonicalAttr(u.str))
			}
		}
	}

	return use, nil
}

// mayCreateShared checks right k on the parent of a mailbox to create in
// another user's namespace and answers the client when it is missing.
func (c *conn) mayCreateShared(tag string, t *target) bool {
	rights, err := c.parentRights(t, t.name)
	if err != nil {
		c.storeErr(tag, err)
		return false
	}

	if !model.HasRight(rights, 'k') {
		if model.HasRight(rights, 'l') {
			c.no(tag, "NOPERM", "Permission denied")
		} else {
			c.no(tag, "NONEXISTENT", "No such mailbox")
		}

		return false
	}

	return true
}

func canonicalAttr(a string) string {
	c, _ := model.CanonicalSpecialUse(a)
	return c
}

func (c *conn) cmdDelete(tag string) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	if strings.EqualFold(name, "INBOX") {
		c.no(tag, "CANNOT", "INBOX cannot be deleted")
		return nil
	}

	t, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if !c.needRight(tag, t, 'x') {
		return nil
	}

	if c.mbox != nil && c.mu == t.u && strings.EqualFold(c.mbox.Name, t.name) {
		c.unselect()
	}

	if err := t.u.DeleteMailbox(t.name); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.ok(tag, "", "DELETE completed")

	return nil
}

func (c *conn) cmdRename(tag string) error {
	oldName, err := c.r.astring()
	if err != nil {
		return err
	}

	newName, err := c.r.astring()
	if err != nil {
		return err
	}

	ot, err := c.target(oldName)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	nt, err := c.target(newName)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if ot.u != nt.u {
		c.no(tag, "CANNOT", "Cannot rename across users")
		return nil
	}

	if ot.shared() {
		if !c.needRight(tag, ot, 'x') {
			return nil
		}

		rights, err := c.parentRights(nt, nt.name)
		if err != nil {
			c.storeErr(tag, err)
			return nil
		}

		if !model.HasRight(rights, 'k') {
			c.no(tag, "NOPERM", "Permission denied")
			return nil
		}
	}

	if err := ot.u.RenameMailbox(ot.name, nt.name); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.ok(tag, "", "RENAME completed")

	return nil
}

func (c *conn) cmdSubscribe(tag string, on bool) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	t, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	switch {
	case t.public:
		if !c.needRight(tag, t, 'l') {
			return nil
		}

		var mb *model.Mailbox

		if mb, err = c.publicMailbox(t); err == nil {
			// By id: a folder that is renamed stays subscribed.
			err = c.user.SetPublicSubscribed(mb.ID, on)
		}
	case t.shared():
		if !c.needRight(tag, t, 'l') {
			return nil
		}

		err = c.user.SetSharedSubscribed(t.owner, t.name, on)
	default:
		err = c.user.SetSubscribed(t.name, on)
	}

	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.ok(tag, "", "completed")

	return nil
}

// --- STATUS ----------------------------------------------------------------

func (c *conn) cmdStatus(tag string) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	t, err := c.r.next()
	if err != nil {
		return err
	}

	if t.kind != tokList {
		return fmt.Errorf("%w: STATUS needs a list", errSyntax)
	}

	var items []string
	for _, it := range t.list {
		items = append(items, strings.ToUpper(it.str))
	}

	st, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if !c.needRight(tag, st, 'r') {
		return nil
	}

	line, err := c.statusLine(st, items)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.w.untagged("%s", line)
	c.ok(tag, "", "STATUS completed")

	return nil
}

func (c *conn) statusLine(t *target, items []string) (string, error) {
	info, err := t.u.Info(t.name)
	if err != nil {
		return "", err
	}

	msgs, err := t.u.ListMessages(info.ID)
	if err != nil {
		return "", err
	}

	d := &statusData{info: info, msgs: msgs, maxLiteral: c.s.o.MaxLiteral}
	for _, m := range msgs {
		if !model.HasFlag(m.Flags, flagSeen) {
			d.unseen++
		}

		if model.HasFlag(m.Flags, flagDeleted) {
			d.deleted++
		}

		d.size += m.Size
	}

	if t.public && slices.Contains(items, "UNSEEN") {
		// "Unread" is per account here: the shared record says nothing
		// about this account, so its own marks decide. The marks are
		// read only when the client asks for the count.
		unseen, err := c.publicUnseen(info.ID, info.UIDValidity, msgs)
		if err != nil {
			return "", err
		}

		d.unseen = unseen
	}

	var out []string

	for _, it := range items {
		if f, ok := statusItems[it]; ok {
			out = append(out, f(d))
		}
	}

	return fmt.Sprintf("STATUS %s (%s)", astringOut(t.display(info.Name)), strings.Join(out, " ")), nil
}

// statusData is what the STATUS items are computed from.
type statusData struct {
	info            *model.MailboxInfo
	msgs            []model.Meta
	unseen, deleted int
	size            int64
	maxLiteral      int64
}

// statusItems render the STATUS data items.
var statusItems = map[string]func(d *statusData) string{
	kwMessages: func(d *statusData) string { return fmt.Sprintf("MESSAGES %d", len(d.msgs)) },
	kwRecent: func(d *statusData) string {
		recent := 0

		for _, m := range d.msgs {
			if m.UID >= d.info.RecentFrom {
				recent++
			}
		}

		return fmt.Sprintf("RECENT %d", recent)
	},
	kwUIDNext:       func(d *statusData) string { return fmt.Sprintf("UIDNEXT %d", d.info.UIDNext) },
	kwUIDValidity:   func(d *statusData) string { return fmt.Sprintf("UIDVALIDITY %d", d.info.UIDValidity) },
	kwUnseen:        func(d *statusData) string { return fmt.Sprintf("UNSEEN %d", d.unseen) },
	kwDeleted:       func(d *statusData) string { return fmt.Sprintf("DELETED %d", d.deleted) },
	kwSize:          func(d *statusData) string { return fmt.Sprintf("SIZE %d", d.size) },
	"HIGHESTMODSEQ": func(d *statusData) string { return fmt.Sprintf("HIGHESTMODSEQ %d", d.info.HighestModSeq) },
	"MAILBOXID":     func(d *statusData) string { return fmt.Sprintf("MAILBOXID (%s)", d.info.ID) },
	"APPENDLIMIT":   func(d *statusData) string { return fmt.Sprintf("APPENDLIMIT %d", d.maxLiteral) },
}

// --- APPEND (with MULTIAPPEND) ---------------------------------------------

func (c *conn) cmdAppend(tag string) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}
	// Target and rights are checked first so that a synchronising literal
	// can be refused before the client sends it; whatever is already on
	// the wire (non-synchronising literals) is drained on failure.
	at, ok := c.appendTarget(tag, name)
	if !ok {
		return nil
	}

	args, err := c.readAppendArgs()
	if err != nil {
		return err
	}

	if !c.appendArgsValid(tag, args) {
		return nil
	}

	appendRights, _ := c.rightsOf(at)
	name = at.name
	au := at.u

	stored, uids, ok := c.appendStore(tag, au, name, appendRights, args.parts)
	if !ok {
		return nil
	}

	c.sieveEvents(kwAppend, at, stored, nil)

	validity := uint32(0)
	if info, err := au.Info(name); err == nil {
		validity = info.UIDValidity
	}

	if c.mbox != nil && au == c.mu && name == c.mbox.Name {
		_ = c.poll(false) // show the new messages before the tagged OK
	}

	c.ok(tag, fmt.Sprintf("APPENDUID %d %s", validity, formatSet(uids)), "APPEND completed")

	return nil
}

// appendTarget resolves the APPEND destination and checks it exists and
// takes right i. On failure the rest of the command is drained, the
// client answered and ok is false.
func (c *conn) appendTarget(tag, name string) (*target, bool) {
	at, err := c.target(name)
	if err != nil {
		c.drain()
		c.storeErr(tag, err)

		return nil, false
	}

	if _, err := at.u.Lookup(at.name); err != nil {
		c.drain()

		if err == model.ErrNoMailbox {
			c.hideOrTryCreate(tag, at)
			return nil, false
		}

		c.storeErr(tag, err)

		return nil, false
	}

	if rights, err := c.rightsOf(at); err != nil || (at.shared() && rights == "") {
		c.drain()
		c.no(tag, "NONEXISTENT", "No such mailbox")

		return nil, false
	} else if !model.HasRight(rights, 'i') {
		c.drain()
		c.no(tag, "NOPERM", "Permission denied")

		return nil, false
	}

	return at, true
}

// appendPart is one message of an APPEND command.
type appendPart struct {
	flags []string
	when  time.Time
	raw   string
}

// appendArgs are the parsed messages of an APPEND command with the first
// syntax problem and the first CATENATE failure (and its response code).
type appendArgs struct {
	parts     []appendPart
	badSyntax string
	catCode   string
	catErr    error
}

// readAppendArgs reads the messages of APPEND (MULTIAPPEND, CATENATE).
func (c *conn) readAppendArgs() (*appendArgs, error) {
	a := &appendArgs{}

	for !c.r.eol() {
		pt, t, err := c.readAppendHead()
		if err != nil {
			return nil, err
		}

		if t.kind == tokAtom && strings.EqualFold(t.str, "CATENATE") {
			if err := c.readCatenatePart(a, pt); err != nil {
				return nil, err
			}

			continue
		}

		if t.kind != tokString {
			a.badSyntax = "Expected message literal"
			continue
		}

		pt.raw = t.str
		a.parts = append(a.parts, pt)
	}

	return a, nil
}

// readAppendHead reads the optional flag list and date-time of one APPEND
// message and returns them with the token that follows.
func (c *conn) readAppendHead() (appendPart, token, error) {
	var pt appendPart

	t, err := c.r.next()
	if err != nil {
		return pt, t, err
	}

	if t.kind == tokList {
		for _, f := range t.list {
			pt.flags = append(pt.flags, strings.ToLower(f.str))
		}

		if t, err = c.r.next(); err != nil {
			return pt, t, err
		}
	}

	if t.kind == tokString && len(t.str) < 40 && !strings.Contains(t.str, "\n") {
		if d, err := time.Parse("_2-Jan-2006 15:04:05 -0700", t.str); err == nil {
			pt.when = d

			if t, err = c.r.next(); err != nil {
				return pt, t, err
			}
		}
	}

	return pt, t, nil
}

// readCatenatePart reads "CATENATE (TEXT literal | URL string ...)"
// (RFC 4469) and adds the assembled message to a.
func (c *conn) readCatenatePart(a *appendArgs, pt appendPart) error {
	lt, err := c.r.next()
	if err != nil {
		return err
	}

	if lt.kind != tokList {
		a.badSyntax = "CATENATE needs a list"
		return nil
	}

	raw, code, err := c.catenate(lt.list)
	if err != nil && a.catErr == nil {
		a.catCode, a.catErr = code, err
		if code == "" {
			a.badSyntax = err.Error()
		}
	}

	pt.raw = raw
	a.parts = append(a.parts, pt)

	return nil
}

// appendArgsValid answers syntax errors and CATENATE failures; false when
// the command must stop.
func (c *conn) appendArgsValid(tag string, a *appendArgs) bool {
	if a.badSyntax != "" {
		c.bad(tag, a.badSyntax)
		return false
	}

	if len(a.parts) == 0 {
		c.bad(tag, "Expected message literal")
		return false
	}

	if a.catErr != nil {
		c.no(tag, a.catCode, a.catErr.Error())
		return false
	}

	return true
}

// appendStore stores the messages into mailbox name of au.
// MULTIAPPEND is all-or-nothing: a failure removes what this command
// already stored, and a delivery intent (recorded before each store)
// lets the scrubber do the same after a crash. The intent must be
// gone before the tagged OK. ok is false when the client was answered.
func (c *conn) appendStore(tag string, au model.User, name, appendRights string, parts []appendPart) (stored []model.Meta, uids []uint32, ok bool) {
	intent := ""

	if len(parts) > 1 {
		id, err := au.BeginDelivery()
		if err != nil {
			c.storeErr(tag, err)
			return nil, nil, false
		}

		intent = id
	}

	fail := func(err error) {
		if len(stored) > 0 {
			if rerr := au.Expunge(name, stored); rerr != nil {
				c.log.Error("multiappend rollback failed, intent kept for the scrubber", "err", rerr)
				c.storeErr(tag, err)

				return
			}
		}

		_ = au.EndDelivery(intent)

		c.storeErr(tag, err)
	}

	for _, pt := range parts {
		guid := model.NewGUID()
		if err := au.RecordDelivery(intent, name, guid); err != nil {
			fail(err)
			return nil, nil, false
		}

		meta, err := au.DeliverGUID(name, guid, []byte(binaryToBase64(pt.raw)), allowedFlags(appendRights, pt.flags), pt.when)
		if err != nil {
			fail(err)
			return nil, nil, false
		}

		stored = append(stored, *meta)
		uids = append(uids, meta.UID)
	}

	if err := au.EndDelivery(intent); err != nil {
		c.log.Error("delivery intent removal failed, rolling back", "err", err)
		fail(err)

		return nil, nil, false
	}

	return stored, uids, true
}

// --- SELECT / EXAMINE with CONDSTORE and QRESYNC ---------------------------

func (c *conn) cmdSelect(tag string, examine bool) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	condstore, qr, ok, err := c.parseSelectParams(tag)
	if err != nil || !ok {
		return err
	}

	if condstore {
		c.condstore = true
	}

	if c.mbox != nil {
		c.unselect()
		c.w.untagged("OK [CLOSED] Previous mailbox is now closed")
	}

	rights, ok := c.openSelected(tag, name, examine)
	if !ok {
		return nil
	}

	c.state = stateSelected
	c.notifyReselect()
	c.initRecent(examine)
	c.selectUntagged(rights)

	if qr != nil {
		if err := c.qresyncReplay(qr); err != nil {
			return err
		}
	}

	code := "READ-WRITE"
	if c.readOnly {
		code = "READ-ONLY"
	}

	verb := kwSelect
	if examine {
		verb = kwExamine
	}

	c.ok(tag, code, verb+" completed")

	return nil
}

// parseSelectParams parses the SELECT/EXAMINE parameters (CONDSTORE,
// QRESYNC). ok is false when the command was answered already.
func (c *conn) parseSelectParams(tag string) (condstore bool, qr *qresyncParams, ok bool, err error) {
	if c.r.eol() {
		return false, nil, true, nil
	}

	t, err := c.r.next()
	if err != nil {
		return false, nil, false, err
	}

	for i := 0; i < len(t.list); i++ {
		switch strings.ToUpper(t.list[i].str) {
		case kwCondstore:
			condstore = true
		case kwQresync:
			if !c.qresync {
				c.bad(tag, "QRESYNC not enabled")
				return false, nil, false, nil
			}

			i++
			if i >= len(t.list) || t.list[i].kind != tokList {
				return false, nil, false, fmt.Errorf("%w: QRESYNC needs parameters", errSyntax)
			}

			if qr, err = parseQresync(t.list[i].list); err != nil {
				return false, nil, false, err
			}
		}
	}

	return condstore, qr, true, nil
}

// openSelected makes mailbox name the selected one and loads its
// snapshot; it returns the account's rights on it. ok is false when the
// client was answered with an error.
func (c *conn) openSelected(tag, name string, examine bool) (string, bool) {
	st, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return "", false
	}

	if !c.needRight(tag, st, 'r') {
		return "", false
	}

	rights, _ := c.rightsOf(st)

	mb, err := st.u.Lookup(st.name)
	if err != nil {
		c.storeErr(tag, err)
		return "", false
	}

	c.mu, c.mowner, c.mrights = st.u, st.owner, rights
	c.mpublic, c.mprefix = st.public, st.prefix
	// Without any modifying right the mailbox is read-only for this account.
	c.mbox, c.readOnly = mb, examine || (!model.HasRight(rights, 'w') && !model.HasRight(rights, 's') && !model.HasRight(rights, 't') && !model.HasRight(rights, 'e'))

	c.examine = examine
	if err := c.reload(); err != nil {
		c.mbox = nil
		c.storeErr(tag, err)

		return "", false
	}

	c.prunePrivateSeen()

	return rights, true
}

// initRecent sets the session's \Recent watermark: everything from the
// stored watermark on is recent for this session; SELECT (not EXAMINE)
// then moves the watermark past the current messages so later sessions
// do not see them as recent.
func (c *conn) initRecent(examine bool) {
	c.recentFrom = c.info.RecentFrom
	if c.recentFrom == 0 {
		c.recentFrom = 1
	}

	if !examine {
		if err := c.mu.MarkRecent(c.mbox.ID, c.info.UIDNext); err != nil {
			c.log.Warn("recent watermark", "err", err)
		}
	}
}

// selectUntagged writes the untagged responses of SELECT/EXAMINE.
func (c *conn) selectUntagged(rights string) {
	c.w.untagged("%d EXISTS", len(c.msgs))
	c.w.untagged("%d RECENT", c.recentCount())

	for i, m := range c.msgs {
		if !model.HasFlag(m.Flags, flagSeen) {
			c.w.untagged("OK [UNSEEN %d] First unseen", i+1)
			break
		}
	}

	c.w.untagged("OK [UIDVALIDITY %d] UIDs valid", c.info.UIDValidity)
	c.w.untagged("OK [UIDNEXT %d] Predicted next UID", c.info.UIDNext)
	flags := append(systemFlags(), c.info.Keywords...)
	c.w.untagged("FLAGS (%s)", flagList(flags))
	c.permanentFlags(rights, flags)
	c.w.untagged("OK [HIGHESTMODSEQ %d] Highest", c.info.HighestModSeq)
	c.w.untagged("OK [MAILBOXID (%s)] Ok", c.info.ID)
}

// permanentFlags writes PERMANENTFLAGS: none when read-only, all flags
// with rights w, otherwise only what the rights permit.
func (c *conn) permanentFlags(rights string, flags []string) {
	if c.readOnly {
		c.w.untagged("OK [PERMANENTFLAGS ()] Read-only")
	} else if rights == model.AllRights || model.HasRight(rights, 'w') {
		c.w.untagged(`OK [PERMANENTFLAGS (%s \*)] Flags permitted`, flagList(flags))
	} else {
		// Only what the rights permit: \Seen with s, \Deleted with t.
		var perm []string
		if model.HasRight(rights, 's') {
			perm = append(perm, flagSeen)
		}

		if model.HasRight(rights, 't') {
			perm = append(perm, flagDeleted)
		}

		c.w.untagged("OK [PERMANENTFLAGS (%s)] Flags permitted", strings.Join(perm, " "))
	}
}

type qresyncParams struct {
	uidValidity uint32
	modseq      uint64
	knownUIDs   []uint32 // nil = all
}

func parseQresync(l []token) (*qresyncParams, error) {
	if len(l) < 2 {
		return nil, fmt.Errorf("%w: QRESYNC needs uidvalidity and modseq", errSyntax)
	}

	v, err := strconv.ParseUint(l[0].str, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("%w: bad uidvalidity", errSyntax)
	}

	ms, err := strconv.ParseUint(l[1].str, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: bad modseq", errSyntax)
	}

	q := &qresyncParams{uidValidity: uint32(v), modseq: ms}

	if len(l) >= 3 && l[2].kind == tokAtom {
		set, err := parseSet(l[2].str)
		if err != nil {
			return nil, err
		}

		q.knownUIDs = set.expand(0) // "*" is not meaningful for known UIDs
	}

	return q, nil
}

// qresyncReplay sends VANISHED (EARLIER) and FETCH FLAGS for everything
// that changed since the client's modseq.
func (c *conn) qresyncReplay(q *qresyncParams) error {
	if q.uidValidity != c.info.UIDValidity {
		return nil // client must resync from scratch; it sees the new UIDVALIDITY
	}

	if q.modseq+1 < c.info.LogStart {
		return c.qresyncReplayBeyondLog(q)
	}

	vanished, err := c.mu.Vanished(c.mbox.ID, q.modseq)
	if err != nil {
		return err
	}

	if q.knownUIDs != nil {
		vanished = filterKnown(vanished, q.knownUIDs)
	}

	if len(vanished) > 0 {
		c.w.untagged("VANISHED (EARLIER) %s", formatSet(vanished))
	}

	return c.qresyncFlags(q.modseq)
}

// filterKnown returns the UIDs of vanished that are in known.
func filterKnown(vanished, knownUIDs []uint32) []uint32 {
	known := map[uint32]bool{}
	for _, u := range knownUIDs {
		known[u] = true
	}

	var filtered []uint32

	for _, u := range vanished {
		if known[u] {
			filtered = append(filtered, u)
		}
	}

	return filtered
}

// qresyncReplayBeyondLog is qresyncReplay when the vanished log no longer
// covers the client's state. With a known-UID set the answer is exact
// (known minus present). Without one, every UID below UIDNEXT that no
// longer exists is reported: a superset that may include UIDs the client
// never saw, which is harmless (the client has nothing to remove for
// those) and avoids a full UID comparison on the client.
func (c *conn) qresyncReplayBeyondLog(q *qresyncParams) error {
	if q.knownUIDs == nil {
		// The gaps are emitted as ranges straight from the (sorted)
		// snapshot; nothing proportional to UIDNEXT is allocated.
		if set := formatGaps(c.msgs, c.info.UIDNext); set != "" {
			c.w.untagged("VANISHED (EARLIER) %s", set)
		}

		return c.qresyncFlags(q.modseq)
	}

	present := map[uint32]bool{}
	for _, m := range c.msgs {
		present[m.UID] = true
	}

	var vanished []uint32

	for _, u := range q.knownUIDs {
		if !present[u] {
			vanished = append(vanished, u)
		}
	}

	if len(vanished) > 0 {
		c.w.untagged("VANISHED (EARLIER) %s", formatSet(vanished))
	}

	return c.qresyncFlags(q.modseq)
}

func (c *conn) qresyncFlags(since uint64) error {
	for i, m := range c.msgs {
		if m.ModSeq > since {
			c.w.untagged("%d FETCH (UID %d FLAGS (%s) MODSEQ (%d))", i+1, m.UID, flagList(m.Flags), m.ModSeq)
		}
	}

	return nil
}

// reload loads a full snapshot. A change that is still pending (entries
// on a higher shard not yet written) is invisible to the snapshot, so the
// session's modseq is set back to just before it; the incremental poll
// then picks the change up once it is committed.
//
// Order matters: the pending records are inspected before the snapshot
// is listed. A change committed before that inspection has its entries
// written before it, hence in the snapshot; a change still pending at
// that point lowers the floor. Inspecting afterwards could miss a change
// that was written and committed between the two reads.
func (c *conn) reload() error {
	info, err := c.mu.Info(c.mbox.Name)
	if err != nil {
		return err
	}

	floor, ok, err := c.mu.PendingFloor(c.mbox.ID, info.HighestModSeq)
	if err != nil {
		return err
	}

	msgs, err := c.mu.ListMessages(c.mbox.ID)
	if err != nil {
		return err
	}

	if ok && floor < info.HighestModSeq {
		info.HighestModSeq = floor
	}

	// A public folder keeps \Seen per account; the shared record says
	// nothing about this session's account. The marks are read before
	// the session state is replaced: a failure here must leave the
	// session on its old snapshot, or its sequence numbers would no
	// longer be what the client was told.
	marks, err := c.privateSeen()
	if err != nil {
		return err
	}

	c.info, c.msgs, c.pseen = info, msgs, marks

	c.privateOverlay(c.msgs)

	return nil
}

func (c *conn) unselect() {
	c.mbox, c.info, c.msgs, c.searchRes = nil, nil, nil, nil
	c.mu, c.mowner, c.mrights = nil, "", ""
	c.mpublic, c.mprefix = false, ""
	c.notifyReselect()

	if c.state == stateSelected {
		c.state = stateAuth
	}
}

// --- QUOTA (RFC 9208) -----------------------------------------------------

func (c *conn) quotaLine() (string, error) {
	usage, err := c.user.QuotaUsage()
	if err != nil {
		return "", err
	}

	limit, err := c.user.QuotaLimit()
	if err != nil {
		return "", err
	}

	var res []string
	if limit.Bytes > 0 {
		res = append(res, fmt.Sprintf("STORAGE %d %d", (usage.Bytes+1023)/1024, (limit.Bytes+1023)/1024))
	}

	if limit.Msgs > 0 {
		res = append(res, fmt.Sprintf("MESSAGE %d %d", usage.Msgs, limit.Msgs))
	}

	return fmt.Sprintf(`QUOTA "" (%s)`, strings.Join(res, " ")), nil
}

func (c *conn) cmdGetQuota(tag string) error {
	root, err := c.r.astring()
	if err != nil {
		return err
	}

	if root != "" {
		c.no(tag, "", "No such quota root")
		return nil
	}

	line, err := c.quotaLine()
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.w.untagged("%s", line)
	c.ok(tag, "", "GETQUOTA completed")

	return nil
}

func (c *conn) cmdGetQuotaRoot(tag string) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	if _, err := c.user.Lookup(name); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	line, err := c.quotaLine()
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.w.untagged(`QUOTAROOT %s ""`, astringOut(name))
	c.w.untagged("%s", line)
	c.ok(tag, "", "GETQUOTAROOT completed")

	return nil
}

func (c *conn) cmdSetQuota(tag string) error {
	root, err := c.r.astring()
	if err != nil {
		return err
	}

	t, err := c.r.next()
	if err != nil {
		return err
	}

	if root != "" || t.kind != tokList || len(t.list)%2 != 0 {
		c.bad(tag, "SETQUOTA root (resource limit ...)")
		return nil
	}
	// Quota limits are administrative. Users may only change them when
	// the operator explicitly allows self-service (test-profile default: off).
	if !c.s.o.QuotaSelfService {
		c.no(tag, "NOPERM", "Quota limits are set by the administrator")
		return nil
	}

	limit, err := c.user.QuotaLimit()
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if !c.applyQuotaLimits(tag, t.list, &limit) {
		return nil
	}

	if err := c.user.SetQuotaLimit(limit); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	line, err := c.quotaLine()
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.w.untagged("%s", line)
	c.ok(tag, "", "SETQUOTA completed")

	return nil
}

// applyQuotaLimits sets the resource limits of a SETQUOTA list on limit.
// It answers the client and returns false for a bad limit or an unknown
// resource.
func (c *conn) applyQuotaLimits(tag string, list []token, limit *model.Quota) bool {
	for i := 0; i < len(list); i += 2 {
		n, err := strconv.ParseInt(list[i+1].str, 10, 64)
		if err != nil || n < 0 || n > 1<<40 {
			c.bad(tag, "Bad limit")
			return false
		}

		switch strings.ToUpper(list[i].str) {
		case "STORAGE":
			limit.Bytes = n * 1024
		case "MESSAGE":
			limit.Msgs = n
		default:
			c.no(tag, "", "Unknown resource")
			return false
		}
	}

	return true
}

// formatGaps returns the UIDs in [1, uidNext) that msgs (sorted by UID)
// does not contain, as a sequence set.
func formatGaps(msgs []model.Meta, uidNext uint32) string {
	var b strings.Builder

	emit := func(from, to uint32) {
		if from > to {
			return
		}

		if b.Len() > 0 {
			b.WriteByte(',')
		}

		if from == to {
			fmt.Fprintf(&b, "%d", from)
		} else {
			fmt.Fprintf(&b, "%d:%d", from, to)
		}
	}

	next := uint32(1)
	for _, m := range msgs {
		if m.UID < next {
			continue
		}

		emit(next, m.UID-1)
		next = m.UID + 1
	}

	if uidNext > 0 {
		emit(next, uidNext-1)
	}

	return b.String()
}

// catenate assembles a message from TEXT parts and IMAP URLs (RFC 4469,
// RFC 5092 relative URLs: imap:///<mailbox>;UIDVALIDITY=<n>/;UID=<n>
// [/;SECTION=<section>]). A part the account may not read or that does
// not exist is a BADURL failure; the total is bounded like a literal.
func (c *conn) catenate(items []token) (string, string, error) {
	var b strings.Builder

	for i := 0; i < len(items); i++ {
		kind := strings.ToUpper(items[i].str)
		if i+1 >= len(items) || !items[i+1].isString() {
			return "", "", fmt.Errorf("%w: CATENATE part needs a value", errSyntax)
		}

		val := items[i+1].str
		i++

		switch kind {
		case kwText:
			b.WriteString(val)
		case "URL":
			part, err := c.resolveURL(val)
			if err != nil {
				return "", "BADURL", err
			}
			// Account the part like a literal of this command (memory
			// budget and APPENDLIMIT), released with the command.
			if c.cmdBytes+int64(len(part)) > c.s.o.MaxLiteral {
				return "", respCodeTooBig, fmt.Errorf("catenated message too large")
			}

			rel, err := membudget.Acquire(int64(len(part)))
			if err != nil {
				return "", respCodeTooBig, fmt.Errorf("catenated message too large")
			}

			c.cmdBytes += int64(len(part))
			c.releases = append(c.releases, rel)

			b.Write(part)
		default:
			return "", "", fmt.Errorf("%w: unknown CATENATE part %q", errSyntax, kind)
		}

		if int64(b.Len()) > c.s.o.MaxLiteral {
			return "", respCodeTooBig, fmt.Errorf("catenated message too large")
		}
	}

	return b.String(), "", nil
}

// imapURL is a parsed IMAP URL of a CATENATE part.
type imapURL struct {
	mailbox  string
	validity string // "" when not given
	uid      uint32
	section  string
	partial  string
}

// resolveURL returns the content an IMAP URL of a CATENATE part refers
// to, read with the account's rights.
func (c *conn) resolveURL(u string) ([]byte, error) {
	p, err := parseIMAPURL(u)
	if err != nil {
		return nil, err
	}

	t, info, err := c.urlMailbox(p)
	if err != nil {
		return nil, err
	}

	metas, err := t.u.GetMessages(info.ID, []uint32{p.uid})
	if err != nil || len(metas) == 0 {
		return nil, fmt.Errorf("no such message")
	}

	m := metas[0]

	rel, err := membudget.Acquire(m.Size)
	if err != nil {
		return nil, fmt.Errorf("message too large")
	}
	defer rel()

	raw, err := t.u.Read(&m)
	if err != nil {
		return nil, fmt.Errorf("message unavailable")
	}

	out, err := urlSection(append([]byte(nil), raw...), raw, p.section)
	if err != nil {
		return nil, err
	}

	return urlPartial(out, p.partial)
}

// parseIMAPURL parses the absolute (imap:///...) and relative
// ("/INBOX/;UID=1", RFC 4469 §3) forms; a server part is accepted only
// when it is empty.
func parseIMAPURL(u string) (*imapURL, error) {
	var rest string

	switch {
	case strings.HasPrefix(strings.ToLower(u), "imap:///"):
		rest = u[len("imap:///"):]
	case strings.HasPrefix(u, "/"):
		rest = u[1:]
	default:
		return nil, fmt.Errorf("unsupported URL")
	}

	segs := strings.Split(rest, "/;")
	if len(segs) < 2 {
		return nil, fmt.Errorf("URL lacks UID")
	}

	mbSeg := segs[0]

	mailbox, validityStr := mbSeg, ""
	if i := strings.Index(strings.ToUpper(mbSeg), ";UIDVALIDITY="); i >= 0 {
		mailbox, validityStr = mbSeg[:i], mbSeg[i+len(";UIDVALIDITY="):]
	}

	mailbox, err := urlUnescape(mailbox)
	if err != nil {
		return nil, fmt.Errorf("bad mailbox in URL")
	}

	p := &imapURL{mailbox: mailbox, validity: validityStr}
	if err := p.parseSegments(segs[1:]); err != nil {
		return nil, err
	}

	if p.uid == 0 {
		return nil, fmt.Errorf("URL lacks UID")
	}

	return p, nil
}

// parseSegments parses the ;UID=, ;SECTION= and ;PARTIAL= segments.
func (p *imapURL) parseSegments(segs []string) error {
	var err error

	for _, sg := range segs {
		up := strings.ToUpper(sg)
		switch {
		case strings.HasPrefix(up, "UID="):
			n, err := strconv.ParseUint(sg[4:], 10, 32)
			if err != nil || n == 0 {
				return fmt.Errorf("bad UID in URL")
			}

			p.uid = uint32(n)
		case strings.HasPrefix(up, "SECTION="):
			if p.section, err = urlUnescape(sg[8:]); err != nil {
				return fmt.Errorf("bad section in URL")
			}
		case strings.HasPrefix(up, "PARTIAL="):
			p.partial = sg[8:]
		default:
			return fmt.Errorf("unsupported URL part")
		}
	}

	return nil
}

// urlMailbox resolves the URL's mailbox, which the account must be able
// to read, and checks the UIDVALIDITY if the URL carries one.
func (c *conn) urlMailbox(p *imapURL) (*target, *model.MailboxInfo, error) {
	t, err := c.target(p.mailbox)
	if err != nil {
		return nil, nil, fmt.Errorf("no such mailbox")
	}

	rights, err := c.rightsOf(t)
	if err != nil || !model.HasRight(rights, 'r') {
		return nil, nil, fmt.Errorf("no such mailbox")
	}

	info, err := t.u.Info(t.name)
	if err != nil {
		return nil, nil, fmt.Errorf("no such mailbox")
	}

	if p.validity != "" {
		if v, err := strconv.ParseUint(p.validity, 10, 32); err != nil || uint32(v) != info.UIDValidity {
			return nil, nil, fmt.Errorf("UIDVALIDITY mismatch")
		}
	}

	return t, info, nil
}

// urlSection extracts the URL's ;SECTION= from raw; out unchanged when
// the URL has none.
func urlSection(out, raw []byte, section string) ([]byte, error) {
	if section == "" {
		return out, nil
	}

	sec, err := parseSection("[" + section + "]")
	if err != nil {
		return nil, fmt.Errorf("bad section")
	}

	return imapserver.ExtractBodySection(bytes.NewReader(raw), sec), nil
}

// urlPartial applies the URL's ;PARTIAL=<offset>.<length> (RFC 5092).
func urlPartial(out []byte, partial string) ([]byte, error) {
	if partial == "" {
		return out, nil
	}

	offStr, lenStr, ok := strings.Cut(partial, ".")
	off, err1 := strconv.ParseInt(offStr, 10, 64)

	ln, err2 := strconv.ParseInt(lenStr, 10, 64)
	if !ok || err1 != nil || err2 != nil || off < 0 || ln < 0 {
		return nil, fmt.Errorf("bad partial in URL")
	}

	if off > int64(len(out)) {
		off = int64(len(out))
	}

	if ln > int64(len(out))-off { // bounded by subtraction, no overflow
		ln = int64(len(out)) - off
	}

	return out[off : off+ln], nil
}

func urlUnescape(s string) (string, error) {
	return url.PathUnescape(s)
}

// REPLACE seq mailbox [flags] [date] message (RFC 8508): store the new
// message in mailbox, then expunge the original. The two steps are not
// atomic; the new message is stored first, so a failure leaves at most a
// duplicate, never a loss.
func (c *conn) cmdReplace(tag string, uid bool) error {
	setStr, err := c.r.atom()
	if err != nil {
		return err
	}

	set, err := parseSet(setStr)
	if err != nil {
		return err
	}

	name, err := c.r.astring()
	if err != nil {
		return err
	}
	// Early rejections consume the rest of the line like APPEND does, so
	// an already announced literal is never read as new commands.
	if !c.mayReplace(tag) {
		return nil
	}

	seqs, metas := c.resolve(set, uid)
	if len(metas) != 1 {
		c.drain()
		c.bad(tag, "REPLACE needs exactly one message")

		return nil
	}

	at, ok := c.replaceTarget(tag, name)
	if !ok {
		return nil
	}

	rights, _ := c.rightsOf(at)

	pt, t, err := c.readAppendHead()
	if err != nil {
		return err
	}

	raw, ok, err := c.readReplaceMessage(tag, t)
	if err != nil || !ok {
		return err
	}

	return c.replaceMessage(tag, at, allowedFlags(rights, pt.flags), pt.when, raw, seqs, metas)
}

// mayReplace checks that the selected mailbox allows expunging the
// original; on failure the command is drained and answered.
func (c *conn) mayReplace(tag string) bool {
	if c.readOnly {
		c.drain()
		c.no(tag, "NOPERM", "Mailbox is read-only")

		return false
	}

	if !model.HasRight(c.mrights, 't') || !model.HasRight(c.mrights, 'e') {
		c.drain()
		c.no(tag, "NOPERM", "Permission denied")

		return false
	}

	return true
}

// replaceTarget resolves the REPLACE destination and checks it exists
// and takes right i; ok is false when the client was answered.
func (c *conn) replaceTarget(tag, name string) (*target, bool) {
	at, err := c.target(name)
	if err != nil {
		c.drain()
		c.storeErr(tag, err)

		return nil, false
	}

	if _, err := at.u.Lookup(at.name); err != nil {
		c.drain()

		if err == model.ErrNoMailbox {
			c.hideOrTryCreate(tag, at)
			return nil, false
		}

		c.storeErr(tag, err)

		return nil, false
	}

	if rights, err := c.rightsOf(at); err != nil || !model.HasRight(rights, 'i') {
		c.drain()

		if !c.needRight(tag, at, 'i') {
			return nil, false
		}
	}

	return at, true
}

// readReplaceMessage reads the replacement message: a literal or a
// CATENATE list starting at t. ok is false when the client was answered.
func (c *conn) readReplaceMessage(tag string, t token) (raw string, ok bool, err error) {
	if t.kind == tokAtom && strings.EqualFold(t.str, "CATENATE") {
		lt, err := c.r.next()
		if err != nil {
			return "", false, err
		}

		if lt.kind != tokList {
			return "", false, fmt.Errorf("%w: CATENATE needs a list", errSyntax)
		}

		if raw, _, err = c.catenate(lt.list); err != nil {
			c.no(tag, "BADURL", err.Error())
			return "", false, nil
		}

		return raw, true, nil
	}

	if t.kind == tokString {
		return t.str, true, nil
	}

	c.bad(tag, "Expected message literal")

	return "", false, nil
}

// replaceMessage stores the replacement in at, then expunges the
// original (metas, at sequence numbers seqs) from the selected mailbox.
func (c *conn) replaceMessage(tag string, at *target, flags []string, when time.Time, raw string, seqs []int, metas []model.Meta) error {
	meta, err := at.u.Deliver(at.name, []byte(raw), flags, when)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	validity := uint32(0)
	if info, err := at.u.Info(at.name); err == nil {
		validity = info.UIDValidity
	}

	c.w.untagged("OK [APPENDUID %d %d] Replacement stored", validity, meta.UID)

	if at.u == c.mu && at.name == c.mbox.Name {
		// Same mailbox: show the replacement (EXISTS) before the original
		// disappears, so the client's sequence numbers stay consistent.
		_ = c.poll(false)
	}

	if err := c.mu.Expunge(c.mbox.Name, metas); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	c.announceExpunge(metas, seqs)
	c.ok(tag, "", "REPLACE completed")

	return nil
}

// binaryToBase64 re-encodes a single-part message whose body is declared
// Content-Transfer-Encoding: binary (RFC 3516 literal8 APPEND) as base64,
// so that it can be fetched by clients without BINARY support and stored
// without NUL bytes in the text. Multipart messages are left alone.
func binaryToBase64(raw string) string {
	hdrEnd := strings.Index(raw, "\r\n\r\n")
	if hdrEnd < 0 {
		return raw
	}

	hdr, body := raw[:hdrEnd+2], raw[hdrEnd+4:]
	lines := strings.Split(hdr, "\r\n")
	cte := -1

	for i, l := range lines {
		k := strings.ToLower(l)
		if strings.HasPrefix(k, "content-type:") && strings.Contains(k, "multipart/") {
			return raw
		}

		if strings.HasPrefix(k, "content-transfer-encoding:") && strings.TrimSpace(k[len("content-transfer-encoding:"):]) == "binary" {
			cte = i
		}
	}

	if cte < 0 {
		return raw
	}

	lines[cte] = "Content-Transfer-Encoding: base64"
	enc := base64.StdEncoding.EncodeToString([]byte(body))

	var b strings.Builder
	b.WriteString(strings.Join(lines, "\r\n"))
	b.WriteString("\r\n")

	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")

		enc = enc[76:]
	}

	b.WriteString(enc)
	b.WriteString("\r\n")

	return b.String()
}
