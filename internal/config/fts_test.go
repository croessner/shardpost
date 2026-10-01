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

func TestFTSIMAPSearch(t *testing.T) {
	c, err := Load(configFile(t, "fts:\n  imap_search: word\n"))
	if err != nil || !c.FTS.IMAPWordSearch() {
		t.Fatalf("word: %+v, %v", c, err)
	}

	c, err = Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.FTS.IMAPWordSearch() || c.FTS.IMAPSearch != "substring" {
		t.Fatalf("default: %+v, %v", c, err)
	}

	if _, err := Load(configFile(t, "fts:\n  imap_search: exact\n")); err == nil {
		t.Error("accepted an unknown search mode")
	}

	if c.FTS.VerifyParallel != 8 {
		t.Errorf("verify_parallel default %d", c.FTS.VerifyParallel)
	}

	for _, bad := range []string{"0", "65"} {
		if _, err := Load(configFile(t, "fts:\n  verify_parallel: "+bad+"\n")); err == nil {
			t.Errorf("accepted verify_parallel %s", bad)
		}
	}
}
