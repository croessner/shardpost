package cephstore

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"

	"shardpost/internal/model"
)

// Submission copies (scaling plan B10). A delayed JMAP submission sends
// a copy of the message taken when it was created, never the message in
// the mailbox, which the client may change or destroy in the meantime:
//
//	the account's registry:
//	  "sub:<guid>"     the copy's intent (blobIntent with Expires and the
//	                   submission id)
//	the account's data pool:
//	  "msg/<guid>/<n>" the copy, one reference, never linked to an entry
//
// The intent comes first, then the blob. The submission worker deletes
// the copy when the submission has ended (sent, canceled, failed); a
// purge finds the copies through their intents, and a scrub deletes a
// copy whose intent expired (the worker did not get to it), as it does
// for an upload. Expires lies well behind the release time
// (submissionCopyHold), so only a worker that was down for that long
// loses a copy, and then the submission fails instead of being sent.

// regSubmissionPrefix is the registry prefix of submission copies.
const regSubmissionPrefix = "sub:"

func submissionKey(guid string) string { return regSubmissionPrefix + guid }

// submissionIntent is the value of "sub:": a blob intent with the id of
// the submission.
type submissionIntent struct {
	blobIntent
	ID string `json:"id"`
}

// PutSubmissionCopy implements model.SubmissionCopies.
func (u *User) PutSubmissionCopy(id string, raw []byte, expires time.Time) (string, error) {
	guid := newGUID()
	size := int64(len(raw))

	in := submissionIntent{blobIntent: blobIntent{Time: time.Now().Unix(), Size: size, Chunks: u.chunkCount(size),
		Expires: expires.Unix()}, ID: id}

	if err := u.setIntent(map[string][]byte{submissionKey(guid): encode(in)}); err != nil {
		return "", err
	}

	// writeBlob removes what it created when it fails; the intent goes
	// with it.
	if _, err := u.writeBlob(guid, raw); err != nil {
		if derr := u.DropSubmissionCopy(guid); derr != nil {
			u.log.Warn("submission copy: intent left behind", "guid", guid, "err", derr)
		}

		return "", err
	}

	return guid, nil
}

// ReadSubmissionCopy implements model.SubmissionCopies.
func (u *User) ReadSubmissionCopy(guid string) ([]byte, error) {
	size, err := u.BlobSize(guid)
	if err != nil {
		return nil, err
	}

	return u.ReadBlob(guid, size, 0, -1)
}

// DropSubmissionCopy implements model.SubmissionCopies: the blob goes
// (its reference count to 0 under the version read, then the chunks),
// then the intent.
func (u *User) DropSubmissionCopy(guid string) error {
	if !model.ValidGUID(guid) {
		return model.ErrNoBlob
	}

	vals, err := getOmapKeys(u.idx, oidRegistry, []string{submissionKey(guid)})
	if err != nil {
		return err
	}

	var in submissionIntent

	v, ok := vals[submissionKey(guid)]
	if ok {
		_ = json.Unmarshal(v, &in)
	}

	// Nothing takes a reference to a submission copy; no cutoff.
	gone, err := u.collectBlob(guid, in.Size, max(in.Chunks, 1), false, time.Time{})
	if err != nil {
		return err
	}

	if !gone {
		return fmt.Errorf("submission copy %s: not deleted", guid)
	}

	if !ok {
		return nil
	}

	return rmKeys(u.idx, oidRegistry, []string{submissionKey(guid)})
}

// SubmissionCopies implements model.SubmissionCopies: one paged read of
// the registry.
func (u *User) SubmissionCopies() ([]model.SubmissionCopy, error) {
	vals, err := u.idx.GetAllOmapValues(oidRegistry, "", regSubmissionPrefix, omapPageSize)

	switch {
	case errCode(err) == syscall.ENOENT:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read submission intents: %w", err)
	}

	out := make([]model.SubmissionCopy, 0, len(vals))

	for k, v := range vals {
		var in submissionIntent

		_ = json.Unmarshal(v, &in)

		out = append(out, model.SubmissionCopy{GUID: strings.TrimPrefix(k, regSubmissionPrefix), ID: in.ID, Size: in.Size,
			Created: time.Unix(in.Time, 0).UTC(), Expires: time.Unix(in.Expires, 0).UTC()})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].GUID < out[j].GUID })

	return out, nil
}

var _ model.SubmissionCopies = (*User)(nil)
