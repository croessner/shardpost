package memstore

import (
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// TestOnlineRebuildOptions (B14i, temp/14 §5.6): an online rebuild over
// memstore indexes every message again with the store's options at
// once, as a cephstore generation built with them answers; an online
// reshard changes nothing.
func TestOnlineRebuildOptions(t *testing.T) {
	s := New()

	u, err := s.User("alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	raw := "Subject: forwarded\r\nContent-Type: message/rfc822\r\n\r\n" +
		"DKIM-Signature: v=1; b=sigtokenxyz\r\nSubject: inner\r\n\r\nbody text\r\n"
	if _, err := u.Deliver("INBOX", []byte(raw), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	if !signatureFound(t, u) {
		t.Fatal("the signature field of the attached message is not indexed with every field")
	}

	lay := u.(model.FTSLayouter)

	s.SetFTSOptions(FTSOptions{AttachedHeaders: fts.AttachedHeadersNoSig})

	rep, err := lay.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Tok: "job-x/1"})
	if err != nil || !rep.Noop || !signatureFound(t, u) {
		t.Fatalf("online reshard: %+v %v", rep, err)
	}

	rep, err = lay.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Rebuild: true, Tok: "job-x/2"})
	if err != nil || rep.Mode != model.FTSModeReindex || rep.Indexed != 1 || signatureFound(t, u) {
		t.Errorf("online rebuild with nosig: %+v %v", rep, err)
	}
}

// signatureFound reports whether the body search finds the signature
// field of the attached message.
func signatureFound(t *testing.T, u model.User) bool {
	t.Helper()

	hits, err := u.SearchField(fts.FieldBody, "sigtokenxyz")
	if err != nil {
		t.Fatal(err)
	}

	return len(hits) == 1
}
