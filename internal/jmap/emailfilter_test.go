package jmap

import "testing"

// TestFilterThreeValued: the truth tables of the operators when a
// condition's hasAttachment is not known yet.
func TestFilterThreeValued(t *testing.T) {
	yes, no := true, false
	unknown := &cnode{attach: &yes}
	tru := &cnode{}
	fls := &cnode{preds: []func(*emailItem) bool{func(*emailItem) bool { return false }}}
	att := func(*emailItem) (bool, bool) { return false, false }

	op := func(o string, c ...*cnode) *cnode { return &cnode{op: o, children: c} }

	for _, c := range []struct {
		name string
		n    *cnode
		want tri
	}{
		{"unknown", unknown, triUnknown},
		{"NOT unknown", op(opNOT, unknown), triUnknown},
		{"NOT true", op(opNOT, tru, unknown), triFalse},
		{"NOT false", op(opNOT, fls), triTrue},
		{"AND unknown, false", op(opAND, unknown, fls), triFalse},
		{"AND unknown, true", op(opAND, unknown, tru), triUnknown},
		{"OR unknown, true", op(opOR, unknown, tru), triTrue},
		{"OR unknown, false", op(opOR, unknown, fls), triUnknown},
		{"empty AND", op(opAND), triTrue},
		{"empty OR", op(opOR), triFalse},
		{"empty NOT", op(opNOT), triTrue},
		{"false before unknown", &cnode{attach: &no, preds: fls.preds}, triFalse},
	} {
		if got := c.n.eval3(&emailItem{}, att); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}

	known := func(*emailItem) (bool, bool) { return true, true }
	if (&cnode{attach: &yes}).eval3(&emailItem{}, known) != triTrue || (&cnode{attach: &no}).eval3(&emailItem{}, known) != triFalse {
		t.Error("a known hasAttachment is not compared")
	}
}
