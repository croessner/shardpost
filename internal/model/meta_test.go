package model

import (
	"encoding/json"
	"testing"
)

// TestMetaHasAttachmentRecord: an index record written before the field
// existed reads as unknown, and both answers survive a round trip, false
// included.
func TestMetaHasAttachmentRecord(t *testing.T) {
	var old Meta
	if err := json.Unmarshal([]byte(`{"uid":1,"guid":"g","size":3}`), &old); err != nil {
		t.Fatal(err)
	}

	if old.HasAttachment != nil {
		t.Errorf("an old record has hasAttachment %v, want unknown", *old.HasAttachment)
	}

	for _, v := range []bool{true, false} {
		b, _ := json.Marshal(&Meta{UID: 1, HasAttachment: &v})

		var back Meta
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}

		if back.HasAttachment == nil || *back.HasAttachment != v {
			t.Errorf("%s reads back as %v, want %v", b, back.HasAttachment, v)
		}
	}
}
