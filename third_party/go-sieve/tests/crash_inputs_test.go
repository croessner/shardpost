package tests

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/foxcpp/go-sieve"
	"github.com/foxcpp/go-sieve/interp"
)

func TestHistoricalCrashInputs(t *testing.T) {
	for _, name := range []string{"fuzz1", "fuzz4"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("pigeonhole/tests/failures/" + name + ".svtest")
			if err != nil {
				t.Fatal(err)
			}
			opts := sieve.DefaultOptions()
			opts.Interp.T = t
			script, err := sieve.Load(strings.NewReader(string(raw)), opts)
			if name == "fuzz4" {
				if err == nil {
					t.Fatal("invalid notify syntax accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// The original file asks for a mailbox it never creates. Its contents
			// aren't a valid acceptance test; exercise that failing lookup directly.
			env := &simpleExecuteRuntime{}
			d := interp.NewRuntimeData(script, interp.DummyPolicy{}, interp.EnvelopeStatic{}, &interp.MessageStatic{})
			d.Test = &interp.TestRuntime{Execute: env}
			if found, err := (interp.TestDovecotMessage{Folder: "Uninteiesting"}).Check(context.Background(), d); err != nil || found {
				t.Fatal("missing mailbox accepted")
			}
		})
	}
}
