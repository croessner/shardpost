//go:build tentacle

package cephstore

import (
	"fmt"
	"testing"

	"shardpost/internal/model"
)

// TestImageDigest: the digest of an object covers name, size, data,
// xattrs and omap, independent of the order of the maps and unambiguous
// at the field borders.
func TestImageDigest(t *testing.T) {
	base := func() *objImage {
		return &objImage{Size: 3, Data: []byte("abc"), Xattrs: map[string][]byte{"user.a": []byte("1")},
			Omap: map[string][]byte{"k1": []byte("v1"), "k2": []byte("v2")}}
	}

	want := base().digest("obj")

	same := base()
	same.Omap = map[string][]byte{"k2": []byte("v2"), "k1": []byte("v1")}
	same.locks = []string{"worker"}

	if got := same.digest("obj"); got != want {
		t.Fatal("the digest depends on the map order or the lock state")
	}

	changes := map[string]func(*objImage){
		"data":       func(im *objImage) { im.Data = []byte("abd") },
		"size":       func(im *objImage) { im.Size = 4 },
		"xattr":      func(im *objImage) { im.Xattrs["user.a"] = []byte("2") },
		"no xattr":   func(im *objImage) { im.Xattrs = nil },
		"omap value": func(im *objImage) { im.Omap["k1"] = []byte("v") },
		"omap key":   func(im *objImage) { im.Omap["k3"] = nil },
		"border": func(im *objImage) {
			im.Omap = map[string][]byte{"k1v": []byte("1"), "k2": []byte("v2")}
		},
		"xattr to omap": func(im *objImage) {
			im.Omap["user.a"] = []byte("1")
			im.Xattrs = nil
		},
	}

	for what, change := range changes {
		im := base()
		change(im)

		if im.digest("obj") == want {
			t.Errorf("%s: same digest", what)
		}
	}

	if base().digest("obj2") == want {
		t.Error("the name is not part of the digest")
	}
}

// TestDigestShards: the digests of 300 000 objects go to at least six
// objects, none of which holds more than 50 000 (R3.12); the count stays
// within 1 and 256.
func TestDigestShards(t *testing.T) {
	const objects = 300000

	n := digestShards(objects)
	if n < 6 {
		t.Fatalf("%d digest objects for %d objects", n, objects)
	}

	counts := make([]int, n)
	for i := range objects {
		counts[model.IndexShard(fmt.Sprintf("kv.jmap/push/acct/user%07d@example.org", i), n)]++
	}

	for shard, c := range counts {
		if c > 50000 {
			t.Errorf("digest object %d holds %d digests", shard, c)
		}
	}

	if digestShards(0) != 1 || digestShards(1) != 1 || digestShards(1<<30) != maxDigestShards {
		t.Errorf("bounds: %d %d %d", digestShards(0), digestShards(1), digestShards(1<<30))
	}
}

// TestDigestSums: the namespace digests cover all four namespaces, an
// empty one included, and the total changes with any of them.
func TestDigestSums(t *testing.T) {
	d := nsDigests{}
	d.set(model.AdminNamespace, "kv.a", "1")

	sums := d.sums()
	for _, ns := range model.SystemNamespaces() {
		if sums[ns] == "" {
			t.Errorf("no digest of %s", ns)
		}
	}

	d.set(model.ServerNamespace, "metadata", "2")

	if d.sums()[digestTotal] == sums[digestTotal] {
		t.Error("the total does not change with a namespace")
	}

	if back, err := parseSums(sumsText(sums)); err != nil || back[digestTotal] != sums[digestTotal] {
		t.Errorf("round trip: %v %v", back, err)
	}
}

// TestMovable: what a migration moves.
func TestMovable(t *testing.T) {
	for _, c := range []struct {
		ns, oid string
		want    bool
	}{
		{model.AdminNamespace, "kv.job/1", true},
		{model.AdminNamespace, FenceOID, false},
		{model.AdminNamespace, runStateOID + "/~admin/000", false},
		{model.AccountsNamespace, "ix.accounts/head", true},
		{model.AccountsNamespace, "sweep/state", false},
		{model.LearningNamespace, learningWorkerOID, true},
		{model.ServerNamespace, "metadata", true},
		{model.PublicNamespace, "mailboxes", false},
		{"user@example.org", "kv.x", false},
		{quarantineNS(model.AdminNamespace, "r1"), "kv.job/1", false},
	} {
		if got := movable(c.ns, c.oid); got != c.want {
			t.Errorf("movable(%s, %s) = %v", c.ns, c.oid, got)
		}
	}

	if !isQuarantine("~admin.moved/r1", "r1") || isQuarantine("~admin.moved/r2", "r1") || isQuarantine("~public.moved/r1", "r1") {
		t.Error("isQuarantine")
	}
}

// TestPhaseOrder: the phases compare in their order; abort and revert
// reach none.
func TestPhaseOrder(t *testing.T) {
	if !phaseAtLeast(PhaseM5, PhaseM4) || phaseAtLeast(PhaseM3, PhaseM4) || phaseAtLeast(PhaseAbort, PhaseM1) || phaseAtLeast("", PhaseM1) {
		t.Fatal("phase order")
	}
}
