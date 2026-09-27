package jmap

import (
	"encoding/json"
	"strings"
	"testing"

	"shardpost/internal/model"
	"shardpost/internal/sharenotice"
)

// TestRightsRoundTrip: an ACL entry read as JMAP rights and written back
// unchanged is the same entry, whatever IMAP rights JMAP cannot express
// it holds; and a write that changes a property changes only its IMAP
// rights.
func TestRightsRoundTrip(t *testing.T) {
	for _, r := range []string{"", "l", "r", "lr", "p", "lrp", "t", "te", "lrswipkxtea", "lrsp", "x", "a", "pk"} {
		if got := applyRights(r, rightsMap(r)); got != r {
			t.Errorf("%q read and written back is %q", r, got)
		}
	}

	none := map[string]bool{}
	for _, sr := range shareRights {
		none[sr.key] = false
	}

	for _, c := range []struct {
		old  string
		want map[string]bool
		out  string
	}{
		{"lrswipkxtea", none, "p"},
		{"lrp", map[string]bool{"maySetKeywords": true}, "lrwp"},
		{"l", map[string]bool{"mayReadItems": false}, "l"},
		{"l", map[string]bool{"mayReadItems": true}, "lr"},
		{"t", map[string]bool{"mayRemoveItems": true}, "te"},
		{"lrte", map[string]bool{"mayRemoveItems": false}, "lr"},
		{"", map[string]bool{"mayRename": true, "mayDelete": true, "mayShare": true}, "xa"},
		{"lrx", map[string]bool{"mayDelete": false, "mayRename": false}, "lr"},
	} {
		if got := applyRights(c.old, c.want); got != c.out {
			t.Errorf("%q with %v is %q, want %q", c.old, c.want, got, c.out)
		}
	}
}

// aclOf reads the ACL of a mailbox.
func aclOf(t *testing.T, u model.User, name string) map[string]string {
	t.Helper()

	acl, err := u.GetACL(name)
	if err != nil {
		t.Fatal(err)
	}

	return acl
}

// setTeam sends one update of alice's Team.
func (f *principals) setTeam(t *testing.T, user, account, patch string) map[string]any {
	t.Helper()

	return f.ok(t, user, "Mailbox/set", `{"accountId":"`+account+`","update":{"`+f.team+`":`+patch+`}}`)
}

// TestShareWithGet: shareWith shows the principals of the ACL — not the
// owner, "anyone" or a negative entry — and myRights has mayShare.
func TestShareWithGet(t *testing.T) {
	f := newPrincipals(t)

	res := f.ok(t, pAlice, "Mailbox/get", `{"properties":["name","shareWith","myRights"]}`)

	byName := map[string]map[string]any{}

	for _, o := range res["list"].([]any) {
		box := o.(map[string]any)
		byName[box[propName].(string)] = box
	}

	team, _ := byName["Team"][propShareWith].(map[string]any)
	bob, _ := team[pBob].(map[string]any)

	if len(team) != 1 || bob["mayReadItems"] != true || bob["maySetSeen"] != true || bob["maySetKeywords"] != false ||
		bob[propMayShare] != false || bob["maySubmit"] != false {
		t.Errorf("Team is shared with %v", byName["Team"][propShareWith])
	}

	solo, _ := byName["Solo"][propShareWith].(map[string]any)
	if len(solo) != 1 || solo[pDave] == nil {
		t.Errorf("Solo is shared with %v; anyone and a negative entry are no principals", solo)
	}

	if byName[inboxName][propShareWith] != nil {
		t.Errorf("INBOX is shared with %v", byName[inboxName][propShareWith])
	}

	if byName["Team"][propMyRights].(map[string]any)[propMayShare] != true {
		t.Error("the owner may not share its mailbox")
	}
}

// TestShareWithSet: the patch forms of RFC 8620 5.3 change the ACL as
// the table in docs/jmap.md says, and leave what JMAP cannot express.
func TestShareWithSet(t *testing.T) {
	for _, c := range []struct {
		name, patch string
		want        map[string]string // identifier -> rights after
	}{
		{"one right", `{"shareWith/bob@example.org/maySetKeywords":true}`,
			map[string]string{pBob: "lrswp"}},
		{"one principal", `{"shareWith/bob@example.org":{"mayReadItems":true}}`,
			map[string]string{pBob: "lrp"}},
		{"whole map", `{"shareWith":{"erin@example.org":{"mayReadItems":true,"mayAddItems":true}}}`,
			map[string]string{pErin: "lri", pBob: ""}},
		{"remove a principal", `{"shareWith/bob@example.org":null}`,
			map[string]string{pBob: ""}},
		{"null", `{"shareWith":null}`,
			map[string]string{pBob: ""}},
		{"one of the x pair", `{"shareWith/bob@example.org/mayDelete":true}`,
			map[string]string{pBob: "lrspx"}},
		{"new principal by one right", `{"shareWith/erin@example.org/mayReadItems":true}`,
			map[string]string{pErin: "lr", pBob: "lrsp"}},
		{"escaped pointer", `{"shareWith/a~0b@example.org":null,"shareWith/bob@example.org/mayShare":true}`,
			map[string]string{pBob: "lrspa"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPrincipals(t)

			if err := f.alice.SetACL("Team", "anyone", "l"); err != nil {
				t.Fatal(err)
			}

			res := f.setTeam(t, pAlice, pAlice, c.patch)
			if res["notUpdated"].(map[string]any)[f.team] != nil {
				t.Fatalf("not updated: %v", res["notUpdated"])
			}

			acl := aclOf(t, f.alice, "Team")
			for id, want := range c.want {
				if acl[id] != want {
					t.Errorf("%s holds %q, want %q (ACL %v)", id, acl[id], want, acl)
				}
			}

			if acl["anyone"] != "l" {
				t.Errorf("the anyone entry is %q", acl["anyone"])
			}

			updated, _ := res["updated"].(map[string]any)[f.team].(map[string]any)
			if _, ok := updated[propShareWith]; !ok {
				t.Errorf("the update does not answer with shareWith: %v", res["updated"])
			}
		})
	}
}

// TestShareWithRefused: what a shareWith may not name or combine.
func TestShareWithRefused(t *testing.T) {
	for _, c := range []struct {
		name, patch, want string
	}{
		{"owner", `{"shareWith/alice@example.org":{"mayReadItems":true}}`, setInvalidProperties},
		{"unknown", `{"shareWith/nobody@example.org":{"mayReadItems":true}}`, setInvalidProperties},
		{"other shard", `{"shareWith/far@example.org":{"mayReadItems":true}}`, setInvalidProperties},
		{"alias", `{"shareWith/al@example.org":{"mayReadItems":true}}`, setInvalidProperties},
		{"anyone", `{"shareWith/anyone":{"mayReadItems":true}}`, setInvalidProperties},
		{"maySubmit", `{"shareWith/bob@example.org/maySubmit":true}`, setInvalidProperties},
		{"x apart", `{"shareWith/bob@example.org":{"mayRename":true,"mayDelete":false}}`, setInvalidProperties},
		{"x apart in two", `{"shareWith/bob@example.org/mayRename":true,"shareWith/bob@example.org/mayDelete":false}`,
			setInvalidProperties},
		{"no such right", `{"shareWith/bob@example.org/mayFly":true}`, setInvalidProperties},
		{"not a boolean", `{"shareWith/bob@example.org/mayShare":"yes"}`, setInvalidProperties},
		{"replace and patch", `{"shareWith":null,"shareWith/bob@example.org/mayShare":true}`, "invalidPatch"},
		{"principal and right", `{"shareWith/bob@example.org":null,"shareWith/bob@example.org/mayShare":true}`,
			"invalidPatch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPrincipals(t)

			res := f.setTeam(t, pAlice, pAlice, c.patch)

			serr, _ := res["notUpdated"].(map[string]any)[f.team].(map[string]any)
			if serr["type"] != c.want {
				t.Errorf("answered %v, want %s", res["notUpdated"], c.want)
			}

			if acl := aclOf(t, f.alice, "Team"); len(acl) != 1 || acl[pBob] != "lrsp" {
				t.Errorf("the ACL changed: %v", acl)
			}
		})
	}
}

// TestShareWithNeedsA: in a shared account shareWith is null and cannot
// be written without right a; with it, a grantee shares further, but not
// with the owner.
func TestShareWithNeedsA(t *testing.T) {
	f := newPrincipals(t)

	res := f.ok(t, pBob, "Mailbox/get", `{"accountId":"`+pAlice+`","ids":["`+f.team+`"]}`)
	box := res["list"].([]any)[0].(map[string]any)

	if box[propShareWith] != nil || box[propMyRights].(map[string]any)[propMayShare] != false {
		t.Errorf("without a bob sees shareWith %v, myRights %v", box[propShareWith], box[propMyRights])
	}

	res = f.setTeam(t, pBob, pAlice, `{"shareWith/carol@example.org":{"mayReadItems":true}}`)
	if serr, _ := res["notUpdated"].(map[string]any)[f.team].(map[string]any); serr["type"] != setForbidden {
		t.Errorf("a share without a answered %v", res)
	}

	if err := f.alice.SetACL("Team", pBob, "lrsa"); err != nil {
		t.Fatal(err)
	}

	res = f.setTeam(t, pBob, pAlice, `{"shareWith/carol@example.org":{"mayReadItems":true}}`)
	if res["notUpdated"].(map[string]any)[f.team] != nil {
		t.Fatalf("a share with a was refused: %v", res["notUpdated"])
	}

	if acl := aclOf(t, f.alice, "Team"); acl[pCarol] != "lr" {
		t.Errorf("carol holds %q", acl[pCarol])
	}

	res = f.setTeam(t, pBob, pAlice, `{"shareWith/alice@example.org":{"mayReadItems":true}}`)
	if serr, _ := res["notUpdated"].(map[string]any)[f.team].(map[string]any); serr["type"] != setInvalidProperties {
		t.Errorf("sharing with the owner answered %v", res)
	}

	res = f.ok(t, pBob, "Mailbox/get", `{"accountId":"`+pAlice+`","ids":["`+f.team+`"],"properties":["shareWith"]}`)

	with, _ := res["list"].([]any)[0].(map[string]any)[propShareWith].(map[string]any)
	if len(with) != 2 || with[pBob] == nil || with[pCarol] == nil {
		t.Errorf("with a bob sees shareWith %v", with)
	}
}

// TestShareWithCreate: a new mailbox can be shared as it is created.
func TestShareWithCreate(t *testing.T) {
	f := newPrincipals(t)

	res := f.ok(t, pAlice, "Mailbox/set",
		`{"create":{"n":{"name":"New","shareWith":{"bob@example.org":{"mayReadItems":true}}}}}`)

	created, _ := res["created"].(map[string]any)["n"].(map[string]any)
	if created == nil || created[propShareWith] == nil {
		t.Fatalf("created %v, notCreated %v", res["created"], res["notCreated"])
	}

	if acl := aclOf(t, f.alice, "New"); acl[pBob] != "lr" {
		t.Errorf("bob holds %q on the new mailbox", acl[pBob])
	}

	res = f.ok(t, pAlice, "Mailbox/set", `{"create":{"n":{"name":"Newer","shareWith":{"nobody@example.org":{}}}}}`)
	if res["notCreated"].(map[string]any)["n"] == nil {
		t.Error("a mailbox shared with nobody was created")
	}

	if _, err := f.alice.Lookup("Newer"); err == nil {
		t.Error("the refused creation left a mailbox behind")
	}
}

// TestShareWithShowsSharedAccount: a grant through shareWith writes the
// grantee's pointer, so the owner's account appears in its session.
func TestShareWithShowsSharedAccount(t *testing.T) {
	f := newPrincipals(t)

	if _, ok := sessionAs(t, f.s, pErin).Accounts[pAlice]; ok {
		t.Fatal("erin sees alice's account before the grant")
	}

	f.setTeam(t, pAlice, pAlice, `{"shareWith/erin@example.org":{"mayReadItems":true}}`)

	a, ok := sessionAs(t, f.s, pErin).Accounts[pAlice]
	if !ok || a.IsPersonal {
		t.Fatalf("erin's session after the grant: %v", sessionAs(t, f.s, pErin).Accounts)
	}

	res := f.ok(t, pErin, "Mailbox/get", `{"accountId":"`+pAlice+`"}`)
	if got := resultIDs(res); len(got) != 1 || got[0] != f.team {
		t.Errorf("erin sees %v in alice's account", got)
	}
}

// notices reads the share notifications of a user.
func (f *principals) notices(t *testing.T, user string) []map[string]any {
	t.Helper()

	var out []map[string]any

	for _, o := range f.ok(t, user, "ShareNotification/get", `{}`)["list"].([]any) {
		out = append(out, o.(map[string]any))
	}

	return out
}

// TestShareNotificationFromJMAP: a grant, a change and a revocation
// through shareWith each tell the grantee; its client can page through
// them with /changes and dismiss them.
func TestShareNotificationFromJMAP(t *testing.T) {
	f := newPrincipals(t)

	state := f.ok(t, pErin, "ShareNotification/get", `{}`)["state"].(string)

	f.setTeam(t, pAlice, pAlice, `{"shareWith/erin@example.org":{"mayReadItems":true}}`)

	list := f.notices(t, pErin)
	if len(list) != 1 {
		t.Fatalf("erin has %d notifications", len(list))
	}

	n := list[0]
	checkGrantNotice(t, f, n)
	dismissNotice(t, f, state, n[propID].(string))

	f.setTeam(t, pAlice, pAlice, `{"shareWith/erin@example.org":null}`)

	list = f.notices(t, pErin)
	if len(list) != 1 || list[0][propNewRights] != nil || list[0][propOldRights] == nil {
		t.Errorf("after the revocation erin has %v", list)
	}
}

// checkGrantNotice checks erin's notice of alice's grant on Team.
func checkGrantNotice(t *testing.T, f *principals, n map[string]any) {
	t.Helper()

	by, _ := n[propChangedBy].(map[string]any)
	newRights, _ := n[propNewRights].(map[string]any)

	if n[propObjectType] != typeMailbox || n[propObjectAccountID] != pAlice || n[propObjectID] != f.team ||
		n[propName] != "Team" || n[propOldRights] != nil {
		t.Errorf("the notification is %v", n)
	}

	if newRights["mayReadItems"] != true || newRights[propMayShare] != false || by[propPrincipalID] != pAlice ||
		by[propEmail] != pAlice {
		t.Errorf("the notification's rights are %v, changedBy %v", newRights, by)
	}
}

// dismissNotice pages to erin's notice with /changes, destroys it, and
// checks that /changes reports the destruction.
func dismissNotice(t *testing.T, f *principals, state, id string) {
	t.Helper()

	res := f.ok(t, pErin, "ShareNotification/changes", `{"sinceState":"`+state+`"}`)
	if created := res["created"].([]any); len(created) != 1 || created[0] != id {
		t.Errorf("changes: %v", res)
	}

	state = res["newState"].(string)

	res = f.ok(t, pErin, "ShareNotification/set", `{"destroy":["`+id+`","nope"],"update":{"x":{}}}`)
	if d := res["destroyed"].([]any); len(d) != 1 || res["notDestroyed"].(map[string]any)["nope"] == nil ||
		res["notUpdated"].(map[string]any)["x"] == nil {
		t.Errorf("set: %v", res)
	}

	res = f.ok(t, pErin, "ShareNotification/changes", `{"sinceState":"`+state+`"}`)
	if d := res["destroyed"].([]any); len(d) != 1 || d[0] != id {
		t.Errorf("changes after the dismissal: %v", res)
	}
}

// TestShareNotificationQuery: the filter conditions of RFC 9670 3.4.1 and
// the sort by created.
func TestShareNotificationQuery(t *testing.T) {
	f := newPrincipals(t)

	f.setTeam(t, pAlice, pAlice, `{"shareWith/erin@example.org":{"mayReadItems":true}}`)

	if err := f.carol.SetACL("Carol", pErin, "lr"); err != nil {
		t.Fatal(err)
	}

	f.setTeam(t, pAlice, pAlice, `{"shareWith/carol@example.org":{"mayReadItems":true}}`)

	for _, c := range []struct {
		filter string
		want   int
	}{
		{`null`, 1},
		{`{"objectAccountId":"` + pAlice + `"}`, 1},
		{`{"objectAccountId":"` + pCarol + `"}`, 0},
		{`{"objectType":"Calendar"}`, 0},
		{`{"after":"2000-01-01T00:00:00Z"}`, 1},
		{`{"before":"2000-01-01T00:00:00Z"}`, 0},
		{`{"operator":"NOT","conditions":[{"objectType":"Mailbox"}]}`, 0},
	} {
		res := f.ok(t, pErin, "ShareNotification/query", `{"filter":`+c.filter+`,"sort":[{"property":"created"}]}`)
		if got := resultIDs(res); len(got) != c.want {
			t.Errorf("filter %s found %v, want %d", c.filter, got, c.want)
		}
	}

	if got, res := f.call(t, pErin, "ShareNotification/query", `{"sort":[{"property":"name"}]}`); got != kwError ||
		res["type"] != errUnsupportedSort {
		t.Errorf("a sort by name answered %s %v", got, res)
	}
}

// TestShareNotificationFromStorePaths: a change the IMAP and admin paths
// report to the recorder reaches the grantee like one of shareWith.
func TestShareNotificationFromStorePaths(t *testing.T) {
	f := newPrincipals(t)

	before := aclOf(t, f.alice, "Solo")[pErin]

	if err := f.alice.SetACL("Solo", pErin, "lrw"); err != nil {
		t.Fatal(err)
	}

	mb, err := f.alice.Lookup("Solo")
	if err != nil {
		t.Fatal(err)
	}

	f.s.o.Principals.Notices.Record(noticeChange(pAlice, mb, pErin, pBob, before))

	list := f.notices(t, pErin)
	if len(list) != 1 || list[0][propName] != "Solo" {
		t.Fatalf("erin has %v", list)
	}

	rights, _ := list[0][propNewRights].(map[string]any)
	if rights["mayReadItems"] != true || rights["maySetKeywords"] != true {
		t.Errorf("new rights %v; anyone's l is part of them", rights)
	}

	by, _ := list[0][propChangedBy].(map[string]any)
	if by[propPrincipalID] != pBob {
		t.Errorf("changed by %v", by)
	}

	if !strings.HasPrefix(list[0][propCreated].(string), "20") {
		t.Errorf("created is %v", list[0][propCreated])
	}

	raw, _ := json.Marshal(list[0])
	if strings.Contains(string(raw), `"p"`) {
		t.Errorf("a notice leaks IMAP rights: %s", raw)
	}
}

// noticeChange is the change the IMAP path reports after a SETACL.
func noticeChange(owner string, mb *model.Mailbox, grantee, by, before string) sharenotice.Change {
	return sharenotice.Change{Owner: owner, MailboxID: mb.ID, Mailbox: mb.Name, Grantee: grantee, By: by,
		Old: sharenotice.Effective(before, mb.ACL["anyone"]), New: model.EffectiveRights(mb.ACL, grantee)}
}
