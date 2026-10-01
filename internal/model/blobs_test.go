package model

import (
	"reflect"
	"slices"
	"testing"
)

// TestBlobsCannotLink: an uploaded blob is never linked to a message
// (scaling plan B6): the upload expiry deletes it without asking whether
// a message uses it. Blobs therefore has no method that could make an
// upload the blob of a message; adding one has to revisit the expiry
// (cephstore/uploads.go) first.
func TestBlobsCannotLink(t *testing.T) {
	typ := reflect.TypeFor[Blobs]()

	var names []string
	for i := range typ.NumMethod() {
		names = append(names, typ.Method(i).Name)
	}

	slices.Sort(names)

	if want := []string{"BlobSize", "PutBlob", "ReadBlob"}; !slices.Equal(names, want) {
		t.Fatalf("model.Blobs has the methods %v, want %v", names, want)
	}
}
