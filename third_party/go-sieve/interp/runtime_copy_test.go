package interp

import "testing"

func TestRuntimeCopyPreservesFlags(t *testing.T) {
	d := &RuntimeData{Flags: []string{"\\Answered", "$label1"}, Test: &TestRuntime{LastErrors: 1}}
	c := d.Copy()
	if len(c.Flags) != 2 || c.Flags[0] != "\\Answered" || d.Flags[1] != "$label1" {
		t.Fatal("flags lost")
	}
	c.Flags[0] = "\\Seen"
	c.Test.LastErrors = 0
	if d.Flags[0] != "\\Answered" || d.Test.LastErrors != 1 {
		t.Fatal("copy mutated original")
	}
}
