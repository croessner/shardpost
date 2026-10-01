package charset

import "testing"

func TestToUTF8(t *testing.T) {
	cases := []struct {
		name, label string
		in          []byte
		want        string
	}{
		{"latin-1", "ISO-8859-1", []byte("Gr\xfc\xdfe aus K\xf6ln"), "Grüße aus Köln"},
		{"windows-1252", "windows-1252", []byte("\x80 5 \x84Zitat\x93"), "€ 5 „Zitat“"},
		{"quoted label", `"iso-8859-15"`, []byte("\xa4"), "€"},
		{"utf-8", "UTF-8", []byte("Grüße"), "Grüße"},
		{"utf-8 invalid", "utf-8", []byte("a\xffb"), "a�b"},
		{"unlabelled utf-8", "", []byte("Grüße"), "Grüße"},
		{"unlabelled 8-bit", "", []byte("Gr\xfc\xdfe"), "Grüße"},
		{"us-ascii 8-bit", "us-ascii", []byte("M\xfcller"), "Müller"},
		{"unknown label", "x-unknown", []byte("M\xfcller"), "Müller"},
		{"koi8-r", "koi8-r", []byte("\xf0\xd2\xc9\xd7\xc5\xd4"), "Привет"},
	}

	for _, c := range cases {
		if got := ToUTF8(c.in, c.label); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecodeHeader(t *testing.T) {
	cases := []struct{ in, want string }{
		{"=?windows-1252?Q?Rechnung_f=FCr_M=E4rz_=80?=", "Rechnung für März €"},
		{"=?ISO-8859-1?B?R3L832U=?= aus =?utf-8?q?K=C3=B6ln?=", "Grüße aus Köln"},
		{"M\xfcller <m@example.org>", "Müller <m@example.org>"},
		{"plain", "plain"},
		{"=?x-unknown?q?abc?=", "=?x-unknown?q?abc?="},
	}

	for _, c := range cases {
		if got := DecodeHeader(c.in); got != c.want {
			t.Errorf("DecodeHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSetFallback(t *testing.T) {
	t.Cleanup(func() { _ = SetFallback(DefaultFallback) })

	for _, bad := range []string{"utf-8", "UTF-16LE", "replacement", "no-such-charset", ""} {
		if err := SetFallback(bad); err == nil {
			t.Errorf("fallback %q accepted", bad)
		}
	}

	if err := SetFallback("iso-8859-5"); err != nil {
		t.Fatal(err)
	}

	if got := ToUTF8([]byte("\xbf\xe0\xd8\xd2\xd5\xe2"), ""); got != "Привет" {
		t.Errorf("iso-8859-5 fallback gave %q", got)
	}

	if got := ToUTF8([]byte("Grüße"), ""); got != "Grüße" {
		t.Errorf("valid UTF-8 must not go through the fallback: %q", got)
	}
}
