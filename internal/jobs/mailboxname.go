package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"shardpost/internal/model"
	"shardpost/internal/mutf7"
)

// --- mailbox_name_convert --------------------------------------------------

// Before IMAP sessions decoded modified UTF-7, a client that did not
// enable UTF-8 created its mailboxes under their modified UTF-7 names,
// and the store kept those octets: "Entw&APw-rfe" instead of "Entwürfe".
// Such a client now sees the name encoded once more ("Entw&-APw-rfe").
// mailbox_name_convert renames these mailboxes to their UTF-8 names.

// Actions of mailbox_name_convert.
const (
	nameRename       = "rename"
	nameSubscription = "subscription"
	nameConflict     = "conflict"
	nameInvalidUTF8  = "invalid_utf8"
)

// nameChange is one finding of mailbox_name_convert. MailboxID is empty
// for a subscription without a mailbox and for an invalid name of one.
type nameChange struct {
	Action    string `json:"action"`
	MailboxID string `json:"mailbox_id,omitempty"`
	From      string `json:"from"`
	To        string `json:"to,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// nameResult is what mailbox_name_convert did.
type nameResult struct {
	Applied []nameChange `json:"applied"`
}

type nameSummary struct {
	Changes []nameChange `json:"changes"`
	// Renames, Subscriptions, Conflicts and Invalid count the changes
	// by action.
	Renames       int `json:"renames"`
	Subscriptions int `json:"subscriptions"`
	Conflicts     int `json:"conflicts"`
	Invalid       int `json:"invalid_utf8"`
}

// mailboxNameConvert renames mailboxes and subscriptions stored under a
// modified UTF-7 name to the UTF-8 name, under the account lock: open
// sessions would otherwise keep a mailbox selected under its old name.
type mailboxNameConvert struct{ kindBase }

// utf8Name converts a stored name whose levels are modified UTF-7 to
// UTF-8. Only a level that is 7-bit, contains "&" and is exactly the
// canonical form of something else changes; ok is false when nothing
// does. A level that is UTF-8 already, or not valid modified UTF-7
// ("R&D"), stays as it is.
func utf8Name(name string) (string, bool) {
	levels := strings.Split(name, model.Delim)
	changed := false

	for i, l := range levels {
		if !strings.Contains(l, "&") || !isASCII(l) {
			continue
		}

		dec, err := mutf7.DecodeCanonical(l)
		if err != nil || dec == l {
			continue
		}

		levels[i], changed = dec, true
	}

	return strings.Join(levels, model.Delim), changed
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}

	return true
}

// nameChanges computes the renames, subscription rewrites, conflicts and
// invalid names of the account.
func nameChanges(u model.User) (nameSummary, error) {
	s := nameSummary{Changes: []nameChange{}}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return s, err
	}

	sort.Slice(boxes, func(a, b int) bool { return boxes[a].Name < boxes[b].Name })

	exists := map[string]bool{}
	for _, mb := range boxes {
		exists[mb.Name] = true
	}

	// Mailboxes whose rename conflicts: their children are not renamed
	// either, they would land below the mailbox that is in the way.
	var blocked []string

	for _, mb := range boxes {
		if !utf8.ValidString(mb.Name) {
			s.add(nameChange{Action: nameInvalidUTF8, MailboxID: mb.ID, From: mb.Name})
			continue
		}

		to, ok := utf8Name(mb.Name)
		if !ok {
			continue
		}

		switch {
		case below(mb.Name, blocked):
			s.add(nameChange{Action: nameConflict, MailboxID: mb.ID, From: mb.Name, To: to, Reason: "a parent mailbox cannot be renamed"})
		case exists[to]:
			blocked = append(blocked, mb.Name)
			s.add(nameChange{Action: nameConflict, MailboxID: mb.ID, From: mb.Name, To: to, Reason: "a mailbox of that name exists"})
		default:
			s.add(nameChange{Action: nameRename, MailboxID: mb.ID, From: mb.Name, To: to})
		}
	}

	missing, err := u.ListSubscribedMissing()
	if err != nil {
		return s, err
	}

	sort.Strings(missing)

	for _, name := range missing {
		if !utf8.ValidString(name) {
			s.add(nameChange{Action: nameInvalidUTF8, From: name})
			continue
		}

		if to, ok := utf8Name(name); ok {
			s.add(nameChange{Action: nameSubscription, From: name, To: to})
		}
	}

	return s, nil
}

// below reports whether name lies below one of the given mailboxes.
func below(name string, parents []string) bool {
	for _, p := range parents {
		if strings.HasPrefix(name, p+model.Delim) {
			return true
		}
	}

	return false
}

func (s *nameSummary) add(c nameChange) {
	s.Changes = append(s.Changes, c)

	switch c.Action {
	case nameRename:
		s.Renames++
	case nameSubscription:
		s.Subscriptions++
	case nameConflict:
		s.Conflicts++
	case nameInvalidUTF8:
		s.Invalid++
	}
}

func (k *mailboxNameConvert) Plan(_ context.Context, _ *Env, u model.User, _ string, _ json.RawMessage) (any, string, error) {
	s, err := nameChanges(u)

	return s, "", err
}

// Run applies the planned renames and subscription rewrites that are
// still needed, parents first. A mailbox is found by its id: renaming a
// parent has already moved its children, so a child is renamed from
// where it is now. The children of a mailbox whose rename failed stay
// where they are. Conflicts and invalid names are reported, not touched.
func (k *mailboxNameConvert) Run(_ context.Context, run *Run) (any, error) {
	var planned nameSummary
	if run.Plan != nil {
		if err := json.Unmarshal(run.Plan.Summary, &planned); err != nil {
			return nil, fmt.Errorf("plan summary: %w", err)
		}
	}

	rn, err := newRenamer(run.User)
	if err != nil {
		return nil, err
	}

	res := nameResult{Applied: []nameChange{}}

	for _, c := range planned.Changes {
		if err := run.Fence(); err != nil {
			return res, err
		}

		var done bool

		switch c.Action {
		case nameRename:
			done, err = rn.rename(c)
		case nameSubscription:
			done, err = rn.moveSubscription(c)
		default:
			continue
		}

		if err != nil {
			run.Partial("%s %q -> %q: %v", c.Action, c.From, c.To, err)
			continue
		}

		if done {
			res.Applied = append(res.Applied, c)
		}
	}

	return res, nil
}

// Errors of a planned rename that can no longer be done.
var (
	errNameTaken     = errors.New("a mailbox of the new name exists")
	errParentSkipped = errors.New("the rename of a parent mailbox failed")
)

// renamer applies renames to one listing of the account's mailboxes,
// which it keeps up to date, instead of listing them for every rename.
type renamer struct {
	u       model.User
	byID    map[string]string // mailbox id -> current name
	names   map[string]bool   // current names
	failed  []string          // current names of mailboxes whose rename failed
	missing map[string]bool   // subscriptions without a mailbox
}

func newRenamer(u model.User) (*renamer, error) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	missing, err := u.ListSubscribedMissing()
	if err != nil {
		return nil, err
	}

	r := &renamer{u: u, byID: map[string]string{}, names: map[string]bool{}, missing: map[string]bool{}}
	for _, mb := range boxes {
		r.byID[mb.ID], r.names[mb.Name] = mb.Name, true
	}

	for _, n := range missing {
		r.missing[n] = true
	}

	return r, nil
}

// rename renames the mailbox of c to c.To from its current name; done
// is false when it is gone or already there.
func (r *renamer) rename(c nameChange) (bool, error) {
	cur, ok := r.byID[c.MailboxID]
	if !ok || cur == c.To {
		return false, nil
	}

	if below(cur, r.failed) {
		return false, errParentSkipped
	}

	if r.names[c.To] {
		r.failed = append(r.failed, cur)
		return false, errNameTaken
	}

	if err := r.u.RenameMailbox(cur, c.To); err != nil {
		r.failed = append(r.failed, cur)
		return false, err
	}

	// The mailbox and its children carry the new prefix now.
	for id, n := range r.byID {
		if n == cur || strings.HasPrefix(n, cur+model.Delim) {
			moved := c.To + n[len(cur):]
			delete(r.names, n)
			r.byID[id], r.names[moved] = moved, true
		}
	}

	return true, nil
}

// moveSubscription moves a subscription without a mailbox to the UTF-8
// name; done is false when it is gone.
func (r *renamer) moveSubscription(c nameChange) (bool, error) {
	if !r.missing[c.From] {
		return false, nil
	}

	if err := r.u.SetSubscribed(c.To, true); err != nil {
		return false, err
	}

	if err := r.u.SetSubscribed(c.From, false); err != nil {
		return false, err
	}

	delete(r.missing, c.From)

	return true, nil
}
