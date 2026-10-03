package config

import (
	"reflect"
	"testing"
)

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

// TestFTSLayoutKeys: the layout keys default to layout 3 (since 0.46.0;
// effective only with the cluster mark) with the bounds of the plan,
// layout 3 settings are taken, and an explicit layout 2 stays 2.
func TestFTSLayoutKeys(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}

	want := FTS{Layout: 3, ShardsMin: 1, ShardsMax: 128, DictShardsMax: 512, ShardTargetKeys: 100000, DictTargetKeys: 100000,
		ConvertBufferKeys: 262144, ConvertWriters: 8, AutoReshard: true, FrozenWaitSeconds: 10, ImportReshardSyncSeconds: 60}

	got := c.FTS
	got.FallbackCharset, got.HTML, got.IMAPSearch, got.VerifyParallel = "", "", "", 0

	if len(got.StatsTestAccounts) != 0 {
		t.Errorf("default test accounts %v", got.StatsTestAccounts)
	}

	got.StatsTestAccounts = nil

	if !reflect.DeepEqual(got, want) {
		t.Errorf("defaults %+v", got)
	}

	if w := c.FTS.ShardTargetWarnings(); len(w) != 0 {
		t.Errorf("the default targets warn: %v", w)
	}

	c, err = Load(configFile(t, "fts:\n  layout: 3\n  shards_min: 2\n  shards_max: 64\n  shard_target_keys: 300000\n  frozen_wait_seconds: 0\n"))
	if err != nil || c.FTS.Layout != 3 || c.FTS.ShardsMin != 2 || c.FTS.ShardsMax != 64 || c.FTS.FrozenWaitSeconds != 0 {
		t.Fatalf("layout 3: %+v, %v", c, err)
	}

	if len(c.FTS.ShardTargetWarnings()) != 1 {
		t.Errorf("a posting target above 133000 keys: %v", c.FTS.ShardTargetWarnings())
	}

	c, err = Load(configFile(t, "fts:\n  layout: 2\n"))
	if err != nil || c.FTS.Layout != 2 {
		t.Fatalf("explicit layout 2: %+v, %v", c.FTS, err)
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
		"fts:\n  dict_shards_max: 0\n",
		"fts:\n  dict_shards_max: 4097\n",
		"fts:\n  shards_min: 8\n  dict_shards_max: 4\n",
		"fts:\n  dict_target_keys: 9999\n",
		"fts:\n  dict_target_keys: 1000001\n",
		"fts:\n  convert_buffer_keys: 16383\n",
		"fts:\n  convert_buffer_keys: 4194305\n",
		"fts:\n  convert_writers: 0\n",
		"fts:\n  convert_writers: 65\n",
		"fts:\n  stats_test_accounts: [\"\"]\n",
		"fts:\n  stats_test_accounts: [\"bench-[\"]\n",
	} {
		if _, err := Load(configFile(t, body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

// TestFTSSeparateCaps (B14g): the dictionary has a cap and a target of
// its own, both targets warn above 133000 keys, and the test accounts
// are patterns.
func TestFTSSeparateCaps(t *testing.T) {
	c, err := Load(configFile(t, "fts:\n  shards_max: 64\n  dict_shards_max: 1024\n  dict_target_keys: 140000\n"+
		"  convert_writers: 16\n  convert_buffer_keys: 524288\n  stats_test_accounts: [\"bench-*\", \"rbdpoc2@exampleserver.de\"]\n"))
	if err != nil {
		t.Fatal(err)
	}

	f := c.FTS
	if f.ShardsMax != 64 || f.DictShardsMax != 1024 || f.DictTargetKeys != 140000 || f.ConvertWriters != 16 || f.ConvertBufferKeys != 524288 {
		t.Errorf("fts %+v", f)
	}

	if w := f.ShardTargetWarnings(); len(w) != 1 {
		t.Errorf("a dictionary target above 133000 keys: %v", w)
	}

	for account, want := range map[string]bool{"bench-00001": true, "rbdpoc2@exampleserver.de": true, "de10000@srvint.net": false, "xbench-1": false} {
		if got := f.StatsTestAccount(account); got != want {
			t.Errorf("test account %s: %v, want %v", account, got, want)
		}
	}
}
