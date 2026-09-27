package mailmeta

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestParseAddressList(t *testing.T) {
	cases := []struct {
		in   string
		want []imap.Address
	}{
		{"user@domain", []imap.Address{{Mailbox: "user", Host: "domain"}}},
		{"From Real <fromuser@fromdomain.org>", []imap.Address{{Name: "From Real", Mailbox: "fromuser", Host: "fromdomain.org"}}},
		{"user@domain (Real Name)", []imap.Address{{Name: "Real Name", Mailbox: "user", Host: "domain"}}},
		{"<user-from (comment)@ (comment) domain.org>", []imap.Address{{Mailbox: "user-from", Host: "domain.org"}}},
		{"<@route:user@domain>", []imap.Address{{Mailbox: "user", Host: "domain"}}},
		{"a@b.org, c@d.org", []imap.Address{{Mailbox: "a", Host: "b.org"}, {Mailbox: "c", Host: "d.org"}}},
		{"group: g1@d1.org, g2@d2.org;, group2: g3@d3.org;", []imap.Address{
			{Mailbox: "group"}, {Mailbox: "g1", Host: "d1.org"}, {Mailbox: "g2", Host: "d2.org"}, {},
			{Mailbox: "group2"}, {Mailbox: "g3", Host: "d3.org"}, {}}},
		{"group:;, group2: (foo) ;", []imap.Address{{Mailbox: "group"}, {}, {Mailbox: "group2"}, {}}},
		{"user2: blah@domain1.org;", []imap.Address{{Mailbox: "user2"}, {Mailbox: "blah", Host: "domain1.org"}, {}}},
		{`"Quoted, Name" <q@x.org>`, []imap.Address{{Name: "Quoted, Name", Mailbox: "q", Host: "x.org"}}},
		{"=?utf-8?q?J=C3=BCrgen?= <j@x.org>", []imap.Address{{Name: "Jürgen", Mailbox: "j", Host: "x.org"}}},
		{"first.last@x.org", []imap.Address{{Mailbox: "first.last", Host: "x.org"}}},
		{"<>", []imap.Address{{}}},
		{"", nil},
	}
	for _, c := range cases {
		got := ParseAddressList(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestExtractDefaultsSenderReplyTo(t *testing.T) {
	env, _, _ := Extract([]byte("From: user@domain\r\nSender: \r\nSubject: x\r\n\r\nbody\r\n"))
	if env == nil || len(env.Sender) != 1 || env.Sender[0].Mailbox != "user" || len(env.ReplyTo) != 1 {
		t.Fatalf("got %+v", env)
	}
}

// TestParseAddressListCharsets: display names in charsets beyond UTF-8
// and ISO-8859-1 are decoded; one in a charset nobody knows stays as
// written.
func TestParseAddressListCharsets(t *testing.T) {
	for in, want := range map[string]string{
		"=?koi8-r?B?8NLJ18XU?= <a@example.org>":           "Привет",
		"=?windows-1252?B?R3L832UggA==?= <a@example.org>": "Grüße €",
		"=?x-unknown?Q?abc?= <a@example.org>":             "=?x-unknown?Q?abc?=",
	} {
		got := ParseAddressList(in)
		if len(got) != 1 || got[0].Name != want {
			t.Errorf("%s: %+v, want %q", in, got, want)
		}
	}

	if got := BaseSubject("=?koi8-r?B?8NLJ18XU?="); got != "ПРИВЕТ" {
		t.Errorf("subject %q", got)
	}
}
