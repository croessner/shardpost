//go:build tentacle

package cephstore

import (
	"errors"
	"testing"
)

// TestRenamePlan: a rename moves the text and, for the active script,
// the active name in the same step; a missing script and a taken name
// change nothing.
func TestRenamePlan(t *testing.T) {
	cur := map[string][]byte{sieveKeyPrefix + "a": []byte("keep;"), sieveKeyPrefix + "b": []byte("stop;")}

	set, rm, activate, err := renamePlan(cur, "a", "a", "c")
	if err != nil || string(set[sieveKeyPrefix+"c"]) != "keep;" || len(rm) != 1 || rm[0] != sieveKeyPrefix+"a" || !activate {
		t.Errorf("renaming the active script: %v %v %v %v", set, rm, activate, err)
	}

	if _, _, activate, _ := renamePlan(cur, "b", "a", "c"); activate {
		t.Error("renaming another script moved the active name")
	}

	if set, rm, _, err := renamePlan(cur, "a", "a", "b"); !errors.Is(err, ErrScriptExists) || set != nil || rm != nil {
		t.Errorf("a taken name gave %v %v %v", set, rm, err)
	}

	if set, _, _, err := renamePlan(cur, "", "x", "c"); !errors.Is(err, ErrNoScript) || set != nil {
		t.Errorf("a missing script gave %v %v", set, err)
	}
}
