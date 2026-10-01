package charset

import "testing"

func TestMislabelledUTF8AndDeclared(t *testing.T) {
	for _, label := range []string{"iso-8859-1", "windows-1252", "koi8-r"} {
		if got := ToUTF8([]byte("Grüße"), label); got != "Grüße" {
			t.Errorf("UTF-8 labelled %s: %q", label, got)
		}
	}

	// not valid UTF-8: the label holds
	if got := ToUTF8([]byte("Gr\xfc\xdfe"), "iso-8859-1"); got != "Grüße" {
		t.Errorf("latin-1: %q", got)
	}

	// a multi-byte charset is not second-guessed
	if got := ToUTF8([]byte("\x82\xa0"), "shift_jis"); got != "あ" {
		t.Errorf("shift_jis: %q", got)
	}

	for label, want := range map[string]bool{"": false, "US-ASCII": false, "x-unknown": false, "iso-8859-1": true, `"utf-8"`: true} {
		if Declared(label) != want {
			t.Errorf("Declared(%q) = %v", label, !want)
		}
	}
}
