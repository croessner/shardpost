package quota

import (
	"testing"

	"shardpost/internal/config"
	"shardpost/internal/model"
)

func mailbox(name string, specialUse ...string) *model.Mailbox {
	return &model.Mailbox{Name: name, MailboxRec: model.MailboxRec{SpecialUse: specialUse}}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"Spam", "Spam", true},
		{"Spam", "spam", false},
		{"Spam*", "Spam/2026", true},
		{"*/Archiv", "Kunden/Archiv", true},
		{"*/Archiv", "Archiv", false},
		{"Liste?", "Liste1", true},
		{"Liste?", "Liste12", false},
		{"*", "", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"*", "a*b", true},
	}

	for _, c := range cases {
		if got := globMatch([]rune(c.pattern), []rune(c.name)); got != c.want {
			t.Errorf("%q ~ %q = %v", c.pattern, c.name, got)
		}
	}
}

func TestPolicyRules(t *testing.T) {
	p := NewPolicy(config.Quota{Rules: []config.QuotaRule{
		{SpecialUse: `\Trash`, ExtraStorage: "10%", ExtraMessages: "100"},
		{Mailbox: "inbox/Spam*", Ignore: true},
		{SpecialUse: `\Junk`, Ignore: true},
	}})

	if !p.Counts(mailbox("INBOX")) || p.Counts(mailbox("INBOX/Spam")) || p.Counts(mailbox("Junk", `\Junk`)) {
		t.Fatal("ignore rules")
	}

	if !p.RenameMayRecount() {
		t.Fatal("name ignore not noticed")
	}

	base := model.Quota{Bytes: 1000, Msgs: 10}
	if got := p.Limit(base, mailbox("Papierkorb", `\Trash`)); got != (model.Quota{Bytes: 1100, Msgs: 110}) {
		t.Fatalf("trash extra: %+v", got)
	}

	if got := p.Limit(base, mailbox("INBOX")); got != base {
		t.Fatalf("no rule: %+v", got)
	}

	if got := p.Limit(model.Quota{}, mailbox("Trash", `\Trash`)); got != (model.Quota{}) {
		t.Fatalf("unlimited stays unlimited: %+v", got)
	}

	var none *Policy
	if !none.Counts(mailbox("Spam")) || none.RenameMayRecount() || none.Limit(base, mailbox("x")) != base {
		t.Fatal("nil policy")
	}

	if NewPolicy(config.Quota{}) != nil {
		t.Fatal("empty configuration compiled")
	}
}

func TestCanonicalPattern(t *testing.T) {
	for in, want := range map[string]string{"inbox": "INBOX", "Inbox/Spam*": "INBOX/Spam*", "/Spam/": "Spam", "Inboxes": "Inboxes", "INBOX*": "INBOX*"} {
		if got := canonicalPattern(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}

	shadow := NewPolicy(config.Quota{Rules: []config.QuotaRule{
		{Mailbox: "Keep*", ExtraStorage: "1M"},
		{SpecialUse: `\Trash`, Ignore: true},
	}})
	if !shadow.RenameMayRecount() || !shadow.Counts(mailbox("KeepTrash", `\Trash`)) || shadow.Counts(mailbox("Trash", `\Trash`)) {
		t.Fatal("name rule shadowing a special-use ignore rule")
	}
}

func TestPolicyAllows(t *testing.T) {
	limit := model.Quota{Bytes: 1000, Msgs: 10}

	var strict *Policy
	if !strict.Allows(model.Quota{Bytes: 900}, limit, 100, 1) || strict.Allows(model.Quota{Bytes: 900}, limit, 101, 1) {
		t.Fatal("exact limit")
	}

	if strict.Allows(model.Quota{Msgs: 10}, limit, 1, 1) {
		t.Fatal("message limit")
	}

	if !strict.Allows(model.Quota{Bytes: 5000, Msgs: 50}, model.Quota{}, 1, 1) {
		t.Fatal("unlimited")
	}

	grace := NewPolicy(config.Quota{Grace: "10%"})
	if !grace.Allows(model.Quota{Bytes: 999}, limit, 101, 1) {
		t.Fatal("grace below the limit")
	}

	if grace.Allows(model.Quota{Bytes: 999}, limit, 102, 1) {
		t.Fatal("beyond grace")
	}

	if grace.Allows(model.Quota{Bytes: 1000}, limit, 1, 1) {
		t.Fatal("grace at the limit")
	}
}
