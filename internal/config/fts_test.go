package config

import "testing"

// TestFTSDefaultsAndChecks: the text preparation defaults to
// Windows-1252 and visible HTML text, and refuses a fallback charset
// that cannot read 8-bit text and an unknown HTML mode.
func TestFTSDefaultsAndChecks(t *testing.T) {
	c, err := Load(configFile(t, "fts:\n  fallback_charset: iso-8859-15\n"))
	if err != nil {
		t.Fatal(err)
	}

	if c.FTS.FallbackCharset != "iso-8859-15" || c.FTS.HTML != "text" {
		t.Errorf("fts = %+v", c.FTS)
	}

	c, err = Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.FTS.FallbackCharset != "windows-1252" {
		t.Errorf("default fallback: %+v, %v", c, err)
	}

	for _, body := range []string{
		"fts:\n  fallback_charset: utf-8\n",
		"fts:\n  fallback_charset: no-such-charset\n",
		"fts:\n  fallback_charset: ''\n",
		"fts:\n  html: strip\n",
	} {
		if _, err := Load(configFile(t, body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}
