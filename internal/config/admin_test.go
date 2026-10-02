package config

import "testing"

func TestAdminValidate(t *testing.T) {
	tlsOK := Admin{Addr: ":9443", TLSCert: "/c", TLSKey: "/k", ClientCA: "/ca", Clients: []AdminClient{{Identity: "ops", Roles: []string{"diagnose"}}}}

	fleet := tlsOK
	fleet.Clients = []AdminClient{{Identity: "fleet", Roles: []string{"diagnose"}, AllAccounts: true}}

	good := []Admin{
		{},
		{Socket: "/run/shardpost/admin.sock", SocketRoles: []string{"deliver"}},
		tlsOK,
		fleet,
	}
	for i, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}

	dup := tlsOK
	dup.Clients = append(dup.Clients, AdminClient{Identity: "OPS", Roles: []string{"deliver"}})

	noClients := tlsOK
	noClients.Clients = nil

	both := tlsOK
	both.Clients = []AdminClient{{Identity: "ops", Roles: []string{"diagnose"}, Accounts: []string{"*@a.org"}, AllAccounts: true}}

	bad := map[string]Admin{
		"relative socket":      {Socket: "admin.sock", SocketRoles: []string{"deliver"}},
		"socket without role":  {Socket: "/run/admin.sock"},
		"roles without socket": {SocketRoles: []string{"deliver"}},
		"tls without addr":     {TLSCert: "/c"},
		"clients without addr": {Clients: []AdminClient{{Identity: "ops", Roles: []string{"diagnose"}}}},
		"addr without ca":      {Addr: ":9443", TLSCert: "/c", TLSKey: "/k", Clients: tlsOK.Clients},
		"addr without clients": noClients,
		"bad port":             {Addr: ":99999", TLSCert: "/c", TLSKey: "/k", ClientCA: "/ca", Clients: tlsOK.Clients},
		"duplicate identity":   dup,
		"scoped all accounts":  both,
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAdminYAML(t *testing.T) {
	body := "admin:\n  socket: /run/shardpost/admin.sock\n  socket_roles: [diagnose, \"deliver:noenforce\"]\n"
	if c, err := Load(configFile(t, body)); err != nil || len(c.Admin.SocketRoles) != 2 {
		t.Fatalf("admin section: %v", err)
	}

	if _, err := Load(configFile(t, "admin:\n  socket: /run/a.sock\n  socket_roles: [root]\n")); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func TestScrubAndBatchDefaults(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}

	if c.Scrub.Parallel != 2 || c.Scrub.IntervalDays != 7 || c.Jobs.BatchParallel != 2 {
		t.Fatalf("scrub %+v jobs %+v", c.Scrub, c.Jobs)
	}

	if _, err := Load(configFile(t, "scrub:\n  interval_days: 0\n")); err == nil {
		t.Fatal("interval_days 0 accepted")
	}
}

// TestScrubIntents: the orphan source starts in shadow mode, the purge
// with the listing; both switch, and the bounds hold.
func TestScrubIntents(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.Scrub.OrphanSource != "shadow" || c.Scrub.IntentGraceSeconds != 3600 || c.Scrub.SweepSpillShards != 256 ||
		c.Scrub.SweepClockMarginSeconds != 60 ||
		!c.Purge.Listing || c.JMAP.UploadTTLSeconds != 86400 {
		t.Fatalf("defaults: %+v %+v %d %v", c.Scrub, c.Purge, c.JMAP.UploadTTLSeconds, err)
	}

	c, err = Load(configFile(t, "scrub:\n  orphan_source: intents\n  intent_grace_seconds: 7200\npurge:\n  listing: false\n"))
	if err != nil || c.Scrub.OrphanSource != "intents" || c.Scrub.IntentGraceSeconds != 7200 || c.Purge.Listing {
		t.Fatalf("switched: %+v %+v %v", c.Scrub, c.Purge, err)
	}

	refused(t, "scrub:\n  orphan_source: none\n", "scrub:\n  intent_grace_seconds: 60\n",
		"scrub:\n  sweep_spill_shards: 0\n", "jmap:\n  upload_ttl_seconds: 600\n", "scrub:\n  sweep_clock_margin_seconds: 0\n")
}

// refused fails for every configuration that loads.
func refused(t *testing.T, bodies ...string) {
	t.Helper()

	for _, bad := range bodies {
		if _, err := Load(configFile(t, bad)); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
}

func TestAdminKVIndex(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.Admin.KVIndex != "read" || c.Admin.KVQueueShards != 4 || c.Jobs.ExpirySeconds != 60 {
		t.Fatalf("defaults: %+v %v", c.Admin, err)
	}

	if c, err := Load(configFile(t, "admin:\n  kv_index: write\n  kv_queue_shards: 8\n")); err != nil || c.Admin.KVIndex != "write" || c.Admin.KVQueueShards != 8 {
		t.Fatalf("write mode: %v", err)
	}

	for _, bad := range []string{"admin:\n  kv_index: on\n", "admin:\n  kv_queue_shards: 9\n", "jobs:\n  expiry_seconds: 1\n"} {
		if _, err := Load(configFile(t, bad)); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
}

func TestLearningIndex(t *testing.T) {
	// read is the default since 0.47.0 (the queue behaves as write until
	// the backfill marker exists).
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.IMAPSieve.Learning.Index != "read" || c.IMAPSieve.Learning.QueueShards != 4 || c.IMAPSieve.Learning.LockRetrySeconds != 20 {
		t.Fatalf("learning index defaults: %+v %v", c.IMAPSieve.Learning, err)
	}

	if c, err := Load(configFile(t, "imapsieve:\n  learning:\n    index: write\n    lock_retry_seconds: 5\n")); err != nil ||
		c.IMAPSieve.Learning.Index != "write" || c.IMAPSieve.Learning.LockRetrySeconds != 5 {
		t.Fatalf("learning write mode: %v", err)
	}

	for _, bad := range []string{"imapsieve:\n  learning:\n    index: on\n", "imapsieve:\n  learning:\n    queue_shards: 9\n",
		"imapsieve:\n  learning:\n    lock_retry_seconds: 0\n"} {
		if _, err := Load(configFile(t, bad)); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
}
