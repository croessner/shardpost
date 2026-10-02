package memstore

import (
	"sort"
	"time"

	"shardpost/internal/model"
)

var _ model.SubmissionCopies = (*User)(nil)

// PutSubmissionCopy implements model.SubmissionCopies.
func (u *User) PutSubmissionCopy(id string, raw []byte, expires time.Time) (string, error) {
	// The intent is a registry write: it registers the account, and is
	// refused for one being purged, as in the RADOS store.
	if err := u.register(""); err != nil {
		return "", err
	}

	guid := u.newGUID()

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.subCopies == nil {
		u.subCopies = map[string]model.SubmissionCopy{}
	}

	u.subCopies[guid] = model.SubmissionCopy{GUID: guid, ID: id, Size: int64(len(raw)), Created: time.Now().UTC(),
		Expires: expires.UTC()}
	u.blobs[guid] = append([]byte(nil), raw...)

	return guid, nil
}

// ReadSubmissionCopy implements model.SubmissionCopies.
func (u *User) ReadSubmissionCopy(guid string) ([]byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	raw, ok := u.blobs[guid]
	if !ok {
		return nil, model.ErrNoBlob
	}

	return append([]byte(nil), raw...), nil
}

// DropSubmissionCopy implements model.SubmissionCopies.
func (u *User) DropSubmissionCopy(guid string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if _, ok := u.subCopies[guid]; ok {
		delete(u.blobs, guid)
		delete(u.subCopies, guid)
	}

	return nil
}

// SubmissionCopies implements model.SubmissionCopies.
func (u *User) SubmissionCopies() ([]model.SubmissionCopy, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	out := make([]model.SubmissionCopy, 0, len(u.subCopies))
	for _, c := range u.subCopies {
		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].GUID < out[j].GUID })

	return out, nil
}
