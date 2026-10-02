package config

import "testing"

func TestSystemPool(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.Ceph.SystemPool != "" || c.Ceph.ClientOptions != nil {
		t.Fatalf("defaults: %+v %v", c.Ceph, err)
	}

	c, err = Load(configFile(t, "ceph:\n  system_pool: shardpost-system\n"))
	if err != nil || c.Ceph.SystemPool != "shardpost-system" {
		t.Fatalf("system pool: %+v %v", c.Ceph, err)
	}

	t.Setenv("CEPH_SYSTEM_POOL", "from-env")

	if c, err := Load(configFile(t, "ceph:\n  system_pool: shardpost-system\n")); err != nil || c.Ceph.SystemPool != "from-env" {
		t.Fatalf("environment: %+v %v", c.Ceph, err)
	}
}

func TestSystemPoolMustDiffer(t *testing.T) {
	for _, bad := range []string{
		"ceph:\n  system_pool: shardpost-index\n",
		"ceph:\n  system_pool: shardpost-data\n",
		"ceph:\n  index_pool: a\n  data_pool: b\n  system_pool: b\n",
	} {
		if _, err := Load(configFile(t, bad)); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
}

func TestClientOptions(t *testing.T) {
	c, err := Load(configFile(t, "ceph:\n  client_options:\n    objecter_inflight_ops: \"4096\"\n    rados_osd_op_timeout: \"30\"\n"))
	if err != nil || c.Ceph.ClientOptions["objecter_inflight_ops"] != "4096" || c.Ceph.ClientOptions["rados_osd_op_timeout"] != "30" {
		t.Fatalf("client options: %+v %v", c.Ceph.ClientOptions, err)
	}

	for _, bad := range []string{
		"ceph:\n  client_options:\n    mon_host: \"10.0.0.1\"\n",              // not on the list
		"ceph:\n  client_options:\n    objecter_inflight_ops: \"many\"\n",     // no number
		"ceph:\n  client_options:\n    objecter_inflight_ops: \"0\"\n",        // a throttle of 0
		"ceph:\n  client_options:\n    rados_mon_op_timeout: \"-1\"\n",        // negative
		"ceph:\n  client_options:\n    objecter_inflight_op_bytes: 1048576\n", // not a string
	} {
		if _, err := Load(configFile(t, bad)); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
}
