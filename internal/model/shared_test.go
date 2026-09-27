package model

import "testing"

// TestValidACLIdentifier: the bytes a pointer key separates on are no
// part of an identifier.
func TestValidACLIdentifier(t *testing.T) {
	for id, want := range map[string]bool{
		"bob": true, "bob@example.org": true, "anyone": true, "jörg": true,
		"": false, "bob/x": false, "bob\x00": false, "bob\xff": false,
	} {
		if got := ValidACLIdentifier(id); got != want {
			t.Errorf("%q: %v, want %v", id, got, want)
		}
	}
}
