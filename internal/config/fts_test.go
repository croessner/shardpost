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

// TestFTSLayoutKeys: the layout keys default to today's layout 2 with
// the bounds of the plan, and layout 3 settings are taken.
func TestFTSLayoutKeys(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}

	want := FTS{Layout: 2, ShardsMin: 1, ShardsMax: 32, ShardTargetKeys: 100000, AutoReshard: true,
		FrozenWaitSeconds: 10, ImportReshardSyncSeconds: 60}

	got := c.FTS
	got.FallbackCharset, got.HTML, got.IMAPSearch, got.VerifyParallel = "", "", "", 0

	if got != want {
		t.Errorf("defaults %+v", got)
	}

	if c.FTS.ShardTargetWarning() != "" {
		t.Error("the default target warns")
	}

	c, err = Load(configFile(t, "fts:\n  layout: 3\n  shards_min: 2\n  shards_max: 64\n  shard_target_keys: 300000\n  frozen_wait_seconds: 0\n"))
	if err != nil || c.FTS.Layout != 3 || c.FTS.ShardsMin != 2 || c.FTS.ShardsMax != 64 || c.FTS.FrozenWaitSeconds != 0 {
		t.Fatalf("layout 3: %+v, %v", c, err)
	}

	if c.FTS.ShardTargetWarning() == "" {
		t.Error("a target above 200000 keys does not warn")
	}
}

// TestFTSLayoutBounds: values out of range are refused.
func TestFTSLayoutBounds(t *testing.T) {
	for _, body := range []string{
		"fts:\n  layout: 1\n",
		"fts:\n  layout: 4\n",
		"fts:\n  shards_min: 0\n",
		"fts:\n  shards_max: 5000\n",
		"fts:\n  shards_min: 8\n  shards_max: 4\n",
		"fts:\n  shard_target_keys: 9999\n",
		"fts:\n  shard_target_keys: 1000001\n",
		"fts:\n  frozen_wait_seconds: 61\n",
		"fts:\n  import_reshard_sync_seconds: 0\n",
		"fts:\n  import_reshard_sync_seconds: 91\n",
		"fts:\n  dict_min_terms: -1\n",
	} {
		if _, err := Load(configFile(t, body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}
