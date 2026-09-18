package interp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRegexCancellation(t *testing.T) {
	for _, octet := range []bool{false, true} {
		cm, err := compileMatcherRegex("(a|aa)*b", octet)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		start := time.Now()
		_, _, err = cm.matchContext(ctx, strings.Repeat("a", 2<<20))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("octet=%v: %v", octet, err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("matching did not stop promptly")
		}
	}
}

func TestCancelledRunDoesNotKeep(t *testing.T) {
	s := &Script{opts: &Options{}}
	d := NewRuntimeData(s, DummyPolicy{}, EnvelopeStatic{}, MessageStatic{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Execute(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(d.AppliedActions) != 0 {
		t.Fatal("cancelled run produced an action")
	}
}

func TestRegexContextCaptures(t *testing.T) {
	for _, octet := range []bool{false, true} {
		cm, err := compileMatcherRegex("(a)(x)?", octet)
		if err != nil {
			t.Fatal(err)
		}
		ok, m, err := cm.matchContext(context.Background(), "préapost")
		if err != nil || !ok || len(m) != 3 || m[0] != "a" || m[1] != "a" || m[2] != "" {
			t.Fatalf("%v %v %v", ok, m, err)
		}
	}
}

func TestMIMEBudgets(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n" + strings.Repeat("--x\r\nContent-Type: text/plain\r\n\r\nx\r\n", maxMimeParts) + "--x--\r\n")
	if _, err := parseMimeBudget(context.Background(), raw); err == nil {
		t.Fatal("unbounded part count")
	}
	nested := "Content-Type: text/plain\r\n\r\nx"
	for i := 0; i < maxMimeDepth+1; i++ {
		nested = "Content-Type: message/rfc822\r\n\r\n" + nested
	}
	if _, err := parseMimeBudget(context.Background(), []byte(nested)); err == nil {
		t.Fatal("unbounded MIME depth")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parseMimeBudget(ctx, []byte("Subject: x\r\n\r\nx")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
