package config

import "testing"

func TestAdminValidate(t *testing.T) {
	tlsOK := Admin{Addr: ":9443", TLSCert: "/c", TLSKey: "/k", ClientCA: "/ca", Clients: []AdminClient{{Identity: "ops", Roles: []string{"diagnose"}}}}

	good := []Admin{
		{},
		{Socket: "/run/shardpost/admin.sock", SocketRoles: []string{"deliver"}},
		tlsOK,
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

func TestAdminKVIndex(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.Admin.KVIndex != "write" || c.Admin.KVQueueShards != 4 || c.Jobs.ExpirySeconds != 60 {
		t.Fatalf("defaults: %+v %v", c.Admin, err)
	}

	if c, err := Load(configFile(t, "admin:\n  kv_index: read\n  kv_queue_shards: 8\n")); err != nil || c.Admin.KVIndex != "read" || c.Admin.KVQueueShards != 8 {
		t.Fatalf("read mode: %v", err)
	}

	for _, bad := range []string{"admin:\n  kv_index: on\n", "admin:\n  kv_queue_shards: 9\n", "jobs:\n  expiry_seconds: 1\n"} {
		if _, err := Load(configFile(t, bad)); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
}
