package mutf7

import (
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for utf8, wire := range map[string]string{
		"":                       "",
		"INBOX":                  "INBOX",
		"Entwürfe":               "Entw&APw-rfe",
		"Ärger":                  "&AMQ-rger",
		"a&b":                    "a&-b",
		"&":                      "&-",
		"Shared/bob/Entwürfe":    "Shared/bob/Entw&APw-rfe",
		"~peter/mail/台北/日本語":     "~peter/mail/&U,BTFw-/&ZeVnLIqe-",
		"Grüße & Küsse":          "Gr&APwA3w-e &- K&APw-sse",
		"tab\there":              "tab&AAk-here",
		"emoji 😀":                "emoji &2D3eAA-",
		"üü":                     "&APwA,A-",
		"Entw&APw-rfe (literal)": "Entw&-APw-rfe (literal)",
	} {
		if got := Encode(utf8); got != wire {
			t.Errorf("Encode(%q) = %q, want %q", utf8, got, wire)
		}

		got, err := DecodeCanonical(wire)
		if err != nil || got != utf8 {
			t.Errorf("DecodeCanonical(%q) = %q, %v; want %q", wire, got, err, utf8)
		}
	}
}

func TestDecodeCanonicalRefuses(t *testing.T) {
	for in, want := range map[string]error{
		"&Jjo!":         ErrUnterminated, // RFC 3501 5.1.3 example of an invalid name
		"AT&T":          ErrUnterminated,
		"Entw&APw":      ErrUnterminated,
		"&U,BTFw":       ErrUnterminated,
		"&*-":           ErrInvalid,      // outside the alphabet
		"&APw/-":        ErrInvalid,      // "/" is "," in modified base64
		"&AP-":          ErrInvalid,      // half a UTF-16 unit
		"&AGE-":         ErrNotCanonical, // "a" encoded although it stands for itself
		"&APw-&APw-":    ErrNotCanonical, // superfluous shift
		"&APx-":         ErrNotCanonical, // stray bits after the last unit
		"&2D0-":         ErrNotCanonical, // unpaired high surrogate
		"Entw\xc3\xbc":  ErrNotASCII,
		"&AGEAYgBj-xyz": ErrNotCanonical,
	} {
		if _, err := DecodeCanonical(in); !errors.Is(err, want) {
			t.Errorf("DecodeCanonical(%q): %v, want %v", in, err, want)
		}
	}
}

func TestDecodeIsLenient(t *testing.T) {
	// Forms another server may produce: decoded, though not canonical.
	for in, want := range map[string]string{"&AGE-": "a", "&APw-&APw-": "üü", "plain": "plain"} {
		if got, err := Decode(in); err != nil || got != want {
			t.Errorf("Decode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
