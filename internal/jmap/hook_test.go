package jmap

import (
	"sync"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// hookedUser is an account of the in-memory store whose script writes
// and blob reads the test can watch and interfere with: what another
// session — a ManageSieve client, a second JMAP request — does in
// between the steps of a call.
type hookedUser struct {
	model.User

	mu sync.Mutex
	// beforeRename and afterRename run around RenameScript, afterPut
	// after a PutScript that succeeded; failPut makes PutScript fail.
	beforeRename func(oldName, newName string)
	afterRename  func(oldName, newName string)
	afterPut     func(name string)
	failPut      error
	// wholeReads counts the reads of a whole blob, by GUID.
	wholeReads map[string]int
}

// RenameScript implements model.User.
func (u *hookedUser) RenameScript(oldName, newName string) error {
	if f := u.beforeRename; f != nil {
		u.beforeRename = nil

		f(oldName, newName)
	}

	if err := u.User.RenameScript(oldName, newName); err != nil {
		return err
	}

	if f := u.afterRename; f != nil {
		u.afterRename = nil

		f(oldName, newName)
	}

	return nil
}

// PutScript implements model.User.
func (u *hookedUser) PutScript(name, text string) error {
	if u.failPut != nil {
		return u.failPut
	}

	if err := u.User.PutScript(name, text); err != nil {
		return err
	}

	if f := u.afterPut; f != nil {
		u.afterPut = nil

		f(name)
	}

	return nil
}

// ReadBlob implements model.Blobs.
func (u *hookedUser) ReadBlob(guid string, size, offset, length int64) ([]byte, error) {
	if offset == 0 && (length < 0 || length == size) {
		u.mu.Lock()
		u.wholeReads[guid]++
		u.mu.Unlock()
	}

	return u.User.(model.Blobs).ReadBlob(guid, size, offset, length)
}

// PutBlob implements model.Blobs.
func (u *hookedUser) PutBlob(raw []byte) (string, error) { return u.User.(model.Blobs).PutBlob(raw) }

// BlobSize implements model.Blobs.
func (u *hookedUser) BlobSize(guid string) (int64, error) { return u.User.(model.Blobs).BlobSize(guid) }

// hookStore hands out the one hooked account.
type hookStore struct{ u *hookedUser }

// User implements Store.
func (h hookStore) User(string) (model.User, error) { return h.u, nil }

// hookedServer is a front-end with Sieve over a hooked account.
func hookedServer(t *testing.T) (*Server, *hookedUser) {
	t.Helper()

	u, err := memstore.New().User("test")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	hu := &hookedUser{User: u, wholeReads: map[string]int{}}

	return newTestServer(t, func(o *Options) {
		o.Store = hookStore{hu}
		o.Sieve = &SieveOptions{}
	}), hu
}
