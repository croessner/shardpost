package sieve

import (
	"context"
	"github.com/foxcpp/go-sieve/interp"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestIMAPEventsFixture(t *testing.T) {
	raw, err := os.ReadFile("tests/imapsieve-events.svtest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(strings.NewReader(string(raw)), DefaultOptions()); err == nil {
		t.Fatal("disabled extension accepted")
	}
	opts := DefaultOptions()
	opts.Interp.Config = map[string]string{"imapsieve_enabled": "yes", "imapsieve_context": "yes"}
	s, err := Load(strings.NewReader(string(raw)), opts)
	if err != nil {
		t.Fatal(err)
	}
	d := NewRuntimeData(s, interp.DummyPolicy{}, interp.EnvelopeStatic{}, interp.MessageStatic{})
	d.Env = interp.MapEnv{"location": "MS", "imap.cause": "FLAG", "imap.changedflags": `\Seen`}
	if err := s.Execute(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(d.Flags, "processed") {
		t.Fatal("event fixture did not run")
	}
}
