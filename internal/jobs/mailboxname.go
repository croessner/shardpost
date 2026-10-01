package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
// where it is now. Conflicts and invalid names are reported, not
// touched.
func (k *mailboxNameConvert) Run(_ context.Context, run *Run) (any, error) {
	var planned nameSummary
	if run.Plan != nil {
		_ = json.Unmarshal(run.Plan.Summary, &planned)
	}

	res := nameResult{Applied: []nameChange{}}

	for _, c := range planned.Changes {
		if err := run.Fence(); err != nil {
			return res, err
		}

		var (
			done bool
			err  error
		)

		switch c.Action {
		case nameRename:
			done, err = renameByID(run.User, c)
		case nameSubscription:
			done, err = moveSubscription(run.User, c)
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

// errNameTaken is a rename whose target appeared after the plan.
var errNameTaken = errors.New("a mailbox of the new name exists")

// renameByID renames the mailbox of c to c.To from its current name;
// done is false when it is gone or already there.
func renameByID(u model.User, c nameChange) (bool, error) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return false, err
	}

	i := slices.IndexFunc(boxes, func(mb model.Mailbox) bool { return mb.ID == c.MailboxID })
	if i < 0 || boxes[i].Name == c.To {
		return false, nil
	}

	if slices.ContainsFunc(boxes, func(mb model.Mailbox) bool { return mb.Name == c.To }) {
		return false, errNameTaken
	}

	return true, u.RenameMailbox(boxes[i].Name, c.To)
}

// moveSubscription moves a subscription without a mailbox to the UTF-8
// name; done is false when it is gone.
func moveSubscription(u model.User, c nameChange) (bool, error) {
	missing, err := u.ListSubscribedMissing()
	if err != nil || !slices.Contains(missing, c.From) {
		return false, err
	}

	if err := u.SetSubscribed(c.To, true); err != nil {
		return false, err
	}

	return true, u.SetSubscribed(c.From, false)
}
