package imapproto

import (
	"errors"
	"fmt"
	"strings"

	"shardpost/internal/model"
)

// Access control (RFC 4314) and the namespace of shared mailboxes.
//
// A mailbox name of the form "<shared prefix><owner>/<name>" addresses a
// mailbox in another user's namespace. Every command resolves its
// mailbox argument to a target (the owning user object, the name inside
// that namespace) and checks the rights the session's account holds on
// it: all rights on its own mailboxes, otherwise the union of the ACL
// entries for the account and for "anyone". A mailbox the account has no
// rights on does not exist for it (NONEXISTENT rather than NOPERM).

// DefaultSharedPrefix is the client-visible prefix of the mailboxes
// other accounts share (see Options.SharedPrefix); below it a mailbox
// is addressed as "<prefix><account>/<mailbox>". Dovecot calls its own
// "shared/"; servers migrated from Exchange or from an older Dovecot
// often use "Other Users/", which is why it is a setting.
const DefaultSharedPrefix = "Shared/"

// DefaultPublicPrefix is the client-visible prefix of the public
// namespace (see Options.PublicPrefix).
const DefaultPublicPrefix = "Public/"

// target is a resolved mailbox argument.
type target struct {
	u      model.User
	owner  string // "" for the session's own namespace and for public folders
	public bool   // a folder of the public namespace
	prefix string // the namespace prefix it was addressed with
	name   string // name inside the namespace
	full   string // name as the client sees it
}

// shared is true when rights come from the ACL rather than from
// ownership: another account's mailbox, or a public folder, which nobody
// owns.
func (t *target) shared() bool { return t.owner != "" || t.public }

// display converts a name inside t's namespace back to the client view.
func (t *target) display(name string) string {
	switch {
	case t.public:
		return t.prefix + name
	case t.owner == "":
		return name
	default:
		return t.prefix + t.owner + "/" + name
	}
}

// target resolves a client-visible mailbox name.
func (c *conn) target(full string) (*target, error) {
	if prefix := c.publicPrefix(); prefix != "" && strings.HasPrefix(full, prefix) {
		name := strings.TrimPrefix(full, prefix)
		if name == "" {
			return nil, model.ErrNoMailbox
		}

		u, err := c.publicUser()
		if err != nil {
			return nil, err
		}

		return &target{u: u, public: true, prefix: prefix, name: name, full: full}, nil
	}

	shared := c.sharedPrefix()
	if shared == "" || !strings.HasPrefix(full, shared) {
		return &target{u: c.user, name: full, full: full}, nil
	}

	rest := strings.TrimPrefix(full, shared)
	owner, name, ok := strings.Cut(rest, "/")

	owner = strings.ToLower(owner)
	if !ok || owner == "" || name == "" {
		return nil, model.ErrNoMailbox
	}

	if owner == c.name {
		return &target{u: c.user, name: name, full: name}, nil
	}

	u, err := c.other(owner)
	if err != nil {
		return nil, err
	}

	return &target{u: u, owner: owner, prefix: shared, name: name, full: full}, nil
}

// publicPrefix is the client-visible prefix of the public namespace,
// "" when public folders are switched off (server.public_prefix: "").
func (c *conn) publicPrefix() string { return c.s.o.PublicPrefix }

// sharedPrefix is the client-visible prefix of the mailboxes other
// accounts share, "" when sharing is switched off
// (server.shared_prefix: ""). The mailboxes stay shared in the store;
// no client can reach them while the namespace is off.
func (c *conn) sharedPrefix() string { return c.s.o.SharedPrefix }

// namespaceField renders one NAMESPACE field: the prefix and the
// hierarchy delimiter, or NIL when that namespace is switched off.
func namespaceField(prefix string) string {
	if prefix == "" {
		return "NIL"
	}

	// RFC 3501 wants a string here, never a bare atom.
	return fmt.Sprintf(`((%s "/"))`, quote(prefix))
}

// publicUser opens the public namespace, once per session. Its view is
// kept with the other namespaces and closed at logout.
func (c *conn) publicUser() (model.User, error) { return c.other(model.PublicNamespace) }

// publicMailbox looks the target's folder up in the public namespace,
// for the places that need its stable id (subscriptions).
func (c *conn) publicMailbox(t *target) (*model.Mailbox, error) { return t.u.Lookup(t.name) }

// other returns the user object of another namespace, opened once per
// session and closed at logout.
func (c *conn) other(owner string) (model.User, error) {
	if u, ok := c.others[owner]; ok {
		return u, nil
	}

	u, err := c.s.o.Store.User(owner)
	if err != nil {
		return nil, err
	}

	if c.others == nil {
		c.others = map[string]model.User{}
	}

	c.others[owner] = u
	model.UseRequest(c.requestContext(), u)

	return u, nil
}

func (c *conn) closeOthers() {
	for _, u := range c.others {
		u.Close()
	}

	c.others = nil
}

// rightsOf returns the rights of the session's account on the target.
// A shared mailbox without any rights for the account reports
// ErrNoMailbox, so its existence is not revealed.
func (c *conn) rightsOf(t *target) (string, error) {
	if !t.shared() {
		return model.AllRights, nil
	}

	acl, err := t.u.GetACL(t.name)
	if err != nil {
		return "", err
	}

	return model.EffectiveRights(acl, c.name), nil
}

// needRight checks one right on the target and answers the client on
// failure. ok is false when the command must stop.
func (c *conn) needRight(tag string, t *target, right byte) bool {
	rights, err := c.rightsOf(t)
	if err != nil {
		c.storeErr(tag, err)
		return false
	}

	if t.shared() && rights == "" {
		c.no(tag, "NONEXISTENT", "No such mailbox")
		return false
	}

	if !model.HasRight(rights, right) {
		c.no(tag, "NOPERM", "Permission denied")
		return false
	}

	return true
}

// parentRights returns the rights on the nearest existing ancestor of
// name inside t's namespace (for CREATE and RENAME targets). Own
// namespace: all rights.
func (c *conn) parentRights(t *target, name string) (string, error) {
	if !t.shared() {
		return model.AllRights, nil
	}

	n := name
	for {
		i := strings.LastIndex(n, "/")
		if i < 0 {
			return "", model.ErrNoMailbox
		}

		n = n[:i]

		acl, err := t.u.GetACL(n)
		if err == nil {
			return model.EffectiveRights(acl, c.name), nil
		}

		if !errors.Is(err, model.ErrNoMailbox) {
			return "", err
		}
	}
}

// --- commands ---------------------------------------------------------------

// SETACL mailbox identifier rights (rights may start with + or -)
// DELETEACL mailbox identifier
func (c *conn) cmdSetACL(tag string, del bool) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	identifier, err := c.r.astring()
	if err != nil {
		return err
	}

	mod := byte(0)
	rights := ""

	if !del {
		var ok bool
		if rights, mod, ok, err = c.readRightsArg(tag); err != nil || !ok {
			return err
		}
	}

	t, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if !c.needRight(tag, t, 'a') {
		return nil
	}

	identifier = strings.ToLower(identifier)
	if c.ownerRightsChange(tag, t, identifier) {
		return nil
	}

	if rights, err = resolveRights(t, identifier, rights, mod); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if err := t.u.SetACL(t.name, identifier, rights); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if del {
		c.ok(tag, "", "DELETEACL completed")
	} else {
		c.ok(tag, "", "SETACL completed")
	}

	return nil
}

// readRightsArg reads the rights argument of SETACL with its optional
// +/- modifier (mod 0 replaces the rights). ok is false after invalid
// rights were answered with BAD.
func (c *conn) readRightsArg(tag string) (rights string, mod byte, ok bool, err error) {
	if rights, err = c.r.astring(); err != nil {
		return "", 0, false, err
	}

	if rights != "" && (rights[0] == '+' || rights[0] == '-') {
		mod, rights = rights[0], rights[1:]
	}

	if _, ok := model.NormalizeRights(rights); !ok {
		c.bad(tag, "Invalid rights")
		return "", 0, false, nil
	}

	return rights, mod, true, nil
}

// ownerRightsChange answers NO and reports true when identifier is the
// owner of the target, whose rights cannot be changed.
func (c *conn) ownerRightsChange(tag string, t *target, identifier string) bool {
	if identifier == c.name && !t.shared() {
		c.no(tag, "CANNOT", "The owner's rights cannot be changed")
		return true
	}

	if identifier == t.owner {
		c.no(tag, "CANNOT", "The owner's rights cannot be changed")
		return true
	}

	return false
}

// resolveRights returns the rights to store for identifier: rights as
// given for mod 0, otherwise the current entry with rights added ('+')
// or removed ('-'), in canonical order.
func resolveRights(t *target, identifier, rights string, mod byte) (string, error) {
	if mod == 0 {
		return rights, nil
	}

	acl, err := t.u.GetACL(t.name)
	if err != nil {
		return "", err
	}

	cur, _ := model.NormalizeRights(acl[identifier])
	add, _ := model.NormalizeRights(rights)

	set := map[rune]bool{}
	for _, r := range cur {
		set[r] = true
	}

	for _, r := range add {
		set[r] = mod == '+'
	}

	var b strings.Builder

	for _, r := range model.AllRights {
		if set[r] {
			b.WriteRune(r)
		}
	}

	return b.String(), nil
}

// GETACL mailbox
func (c *conn) cmdGetACL(tag string) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	t, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if !c.needRight(tag, t, 'a') {
		return nil
	}

	acl, err := t.u.GetACL(t.name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	var parts []string

	owner := t.owner
	if owner == "" {
		owner = c.name
	}

	parts = append(parts, astringOut(owner), model.AllRights)
	for _, id := range sortedKeys(acl) {
		parts = append(parts, astringOut(id), acl[id])
	}

	c.w.untagged("ACL %s %s", astringOut(name), strings.Join(parts, " "))
	c.ok(tag, "", "GETACL completed")

	return nil
}

// LISTRIGHTS mailbox identifier
func (c *conn) cmdListRights(tag string) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	identifier, err := c.r.astring()
	if err != nil {
		return err
	}

	t, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if !c.needRight(tag, t, 'a') {
		return nil
	}

	owner := t.owner
	if owner == "" {
		owner = c.name
	}

	if strings.EqualFold(identifier, owner) {
		c.w.untagged("LISTRIGHTS %s %s %s", astringOut(name), astringOut(identifier), model.AllRights)
	} else {
		// nothing is always granted; every right can be granted singly
		c.w.untagged(`LISTRIGHTS %s %s "" l r s w i p k x t e a`, astringOut(name), astringOut(identifier))
	}

	c.ok(tag, "", "LISTRIGHTS completed")

	return nil
}

// MYRIGHTS mailbox
func (c *conn) cmdMyRights(tag string) error {
	name, err := c.r.astring()
	if err != nil {
		return err
	}

	t, err := c.target(name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if _, err := t.u.Lookup(t.name); err != nil {
		c.storeErr(tag, err)
		return nil
	}

	rights, err := c.rightsOf(t)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if t.shared() && rights == "" {
		c.no(tag, "NONEXISTENT", "No such mailbox")
		return nil
	}

	c.w.untagged("MYRIGHTS %s %s", astringOut(name), rights)
	c.ok(tag, "", "MYRIGHTS completed")

	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}

	return out
}

var _ = fmt.Sprintf

// allowedFlags drops the flags the rights do not permit to set (RFC 4314
// section 4: \Seen needs s, \Deleted needs t, everything else w). Used for
// APPEND and COPY into mailboxes with restricted rights.
func allowedFlags(rights string, flags []string) []string {
	if rights == model.AllRights {
		return flags
	}

	out := flags[:0:0]
	for _, f := range flags {
		need := byte('w')

		switch strings.ToLower(f) {
		case flagSeenLower:
			need = 's'
		case flagDeletedLower:
			need = 't'
		}

		if model.HasRight(rights, need) {
			out = append(out, f)
		}
	}

	return out
}

// hideOrTryCreate answers a missing or inaccessible destination: for the
// account's own namespace TRYCREATE, for a shared one NONEXISTENT in
// both cases, so the existence of a foreign mailbox is not revealed.
func (c *conn) hideOrTryCreate(tag string, t *target) {
	if t.shared() {
		c.no(tag, "NONEXISTENT", "No such mailbox")
		return
	}

	c.no(tag, "TRYCREATE", "No such mailbox")
}
